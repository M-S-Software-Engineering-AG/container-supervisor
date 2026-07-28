// Package supervisor runs the configured processes as a dependency graph:
// a process with no depends_on starts immediately, one with dependencies
// starts once each upstream has exited with the required outcome. Run
// validates the graph (dependencies exist, no cycles) before starting.
package supervisor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jehuda-ruzinski/container-supervisor/internal/config"
	"github.com/jehuda-ruzinski/container-supervisor/internal/logging"
	"github.com/rs/zerolog"
)

// terminal statuses a process can reach.
const (
	statusSuccess = "success"
	statusFailure = "failure"
	statusSkipped = "skipped" // a dependency's condition was not met, or cancelled before start
)

// Validate reports the first problem in the dependency graph: a depends_on
// referencing an unknown process, or a cycle.
func Validate(cfg *config.Config) error {
	for name, proc := range cfg.Processes {
		for dep := range proc.DependsOn {
			if _, ok := cfg.Processes[dep]; !ok {
				return fmt.Errorf("process %q depends_on unknown process %q", name, dep)
			}
		}
	}

	const (
		unvisited = iota
		visiting
		done
	)
	color := make(map[string]int, len(cfg.Processes))
	var visit func(string) error
	visit = func(name string) error {
		color[name] = visiting
		for dep := range cfg.Processes[name].DependsOn {
			switch color[dep] {
			case visiting:
				return fmt.Errorf("circular dependency: %q -> %q", name, dep)
			case unvisited:
				if err := visit(dep); err != nil {
					return err
				}
			}
		}
		color[name] = done
		return nil
	}
	for name := range cfg.Processes {
		if color[name] == unvisited {
			if err := visit(name); err != nil {
				return err
			}
		}
	}
	return nil
}

// Run validates the graph, then starts every process, each waiting on its
// dependencies. It returns when all processes have reached a terminal state.
// A process that fails with on_failure "fail" (the default) aborts the run:
// the context is cancelled, killing running processes, and Run returns an
// error. "continue" tolerates the failure and lets other branches proceed.
// "retry <count>" re-runs it up to count times before treating exhaustion as
// a "fail".
func Run(parent context.Context, cfg *config.Config, log zerolog.Logger) error {
	if err := Validate(cfg); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var aborted atomic.Bool

	names := make([]string, 0, len(cfg.Processes))
	for name := range cfg.Processes {
		names = append(names, name)
	}
	tagged := make([]string, 0, len(names))
	for _, name := range names {
		if p := cfg.Processes[name]; p.IsEnabled() && !p.HidesLabel() {
			tagged = append(tagged, name)
		}
	}
	factory := logging.NewFactory(tagged)

	type state struct {
		done   chan struct{}
		status string // read only after done is closed
	}
	states := make(map[string]*state, len(cfg.Processes))
	for _, name := range names {
		states[name] = &state{done: make(chan struct{})}
	}

	var wg sync.WaitGroup
	for name, proc := range cfg.Processes {
		wg.Add(1)
		go func(name string, proc config.Process) {
			defer wg.Done()
			st := states[name]
			defer close(st.done)

			// A disabled process never starts and never aborts the run; it counts as
			// a failure for dependents, so those requiring "success" are skipped and
			// those requiring "failure"/"any" proceed. Its own dependencies are moot.
			if !proc.IsEnabled() {
				st.status = statusFailure
				log.Info().Str("process_name", name).Msg("process disabled; not starting")
				return
			}

			for depName, cond := range proc.DependsOn {
				dep := states[depName]
				select {
				case <-ctx.Done():
					st.status = statusSkipped
					log.Warn().Str("process_name", name).Msg("cancelled before start")
					return
				case <-dep.done:
				}
				if !conditionMet(cond.Exit, dep.status) {
					st.status = statusSkipped
					log.Warn().Str("process_name", name).Str("dependency", depName).
						Str("required", cond.Exit).Str("got", dep.status).
						Msg("dependency condition not met; skipping")
					return
				}
			}

			// A service with on_failure "restart" is kept up: rerun it whenever it
			// stops, until the run is cancelled. It never aborts the run itself.
			if proc.Type == config.TypeService && proc.OnFailure == "restart" {
				for ctx.Err() == nil {
					st.status = runProcess(ctx, name, proc, factory, log)
					if ctx.Err() == nil {
						log.Warn().Str("process_name", name).Msg("service exited; restarting")
					}
				}
				return
			}

			// A cron runs on its schedule until the run is cancelled, so it never
			// reaches a terminal state on its own.
			if proc.Type == config.TypeCron {
				st.status = runCron(ctx, name, proc, factory, log)
				if st.status == statusFailure && ctx.Err() == nil {
					aborted.Store(true)
					log.Error().Str("process_name", name).Msg("critical process failed; aborting run")
					cancel()
				}
				return
			}

			st.status = runWithRetries(ctx, name, proc, factory, log)
			// A process killed because the run is already aborting didn't fail on
			// its own; don't re-report it as the cause.
			if ctx.Err() != nil {
				return
			}
			// A service exiting cleanly on its own is still an unexpected stop.
			serviceStopped := st.status == statusSuccess && proc.Type == config.TypeService
			if (st.status == statusFailure || serviceStopped) && proc.OnFailure != "continue" {
				aborted.Store(true)
				if serviceStopped {
					log.Error().Str("process_name", name).Msg("service exited unexpectedly; aborting run")
				} else {
					log.Error().Str("process_name", name).Msg("critical process failed; aborting run")
				}
				cancel()
			}
		}(name, proc)
	}
	wg.Wait()
	if aborted.Load() {
		return fmt.Errorf("run aborted: a critical process failed")
	}
	return nil
}

// conditionMet reports whether an upstream's terminal status satisfies the
// required exit outcome. A skipped upstream never satisfies a condition.
func conditionMet(want, status string) bool {
	switch want {
	case statusSuccess:
		return status == statusSuccess
	case statusFailure:
		return status == statusFailure
	case "any":
		return status == statusSuccess || status == statusFailure
	}
	return false
}

// cronNext reports when a cron process should next fire. A package var so tests
// can substitute a fast schedule without stubbing out the wait itself, which is
// where cancellation is handled.
var cronNext = func(s config.Schedule, after time.Time) time.Time { return s.Next(after) }

// runCron runs proc on its schedule until the run is cancelled, which it reports
// as statusSkipped. It returns statusFailure only when the run should abort:
// an occurrence failed and on_failure is not "continue", or the schedule itself
// is unusable.
func runCron(ctx context.Context, name string, proc config.Process, factory *logging.Factory, log zerolog.Logger) string {
	sched, err := config.ParseCron(proc.Cron)
	if err != nil {
		// config.Load rejects this; only reachable for a config built in code.
		log.Error().Err(err).Str("process_name", name).Str("cron", proc.Cron).Msg("invalid cron schedule")
		return statusFailure
	}

	runNow := bool(proc.RunAtStart)
	// from is the point each slot is computed after. It advances to the slot just
	// fired rather than to time.Now(), because Next only guarantees "strictly
	// after" at minute granularity: a timer that fires a hair early would leave
	// now inside the previous minute, and computing from there would hand back
	// the same slot and run it twice.
	from := time.Now()
	for ctx.Err() == nil {
		if !runNow {
			next := cronNext(sched, from)
			if next.IsZero() {
				log.Error().Str("process_name", name).Str("cron", proc.Cron).Msg("cron schedule has no next occurrence")
				return statusFailure
			}
			log.Debug().Str("process_name", name).Time("next_run", next).Msg("cron scheduled")
			timer := time.NewTimer(time.Until(next))
			select {
			case <-ctx.Done():
				timer.Stop()
				return statusSkipped
			case <-timer.C:
			}
			from = next
		}
		runNow = false

		status := runWithRetries(ctx, name, proc, factory, log)
		// ponytail: an occurrence that overran its own schedule resumes from now,
		// so it skips the slots it covered rather than firing them back to back.
		// Missed slots are never made up; queue them here if one ever must be.
		if now := time.Now(); now.After(from) {
			from = now
		}
		// Killed because the run is shutting down: that is the cancellation, not
		// a failure of this occurrence.
		if ctx.Err() != nil {
			return statusSkipped
		}
		if status == statusFailure && proc.OnFailure != "continue" {
			return statusFailure
		}
	}
	return statusSkipped
}

// runWithRetries runs proc, then re-runs it up to RetryCount times for as long
// as it keeps failing.
func runWithRetries(ctx context.Context, name string, proc config.Process, factory *logging.Factory, log zerolog.Logger) string {
	status := runProcess(ctx, name, proc, factory, log)
	for retries := proc.RetryCount(); status == statusFailure && retries > 0 && ctx.Err() == nil; retries-- {
		log.Warn().Str("process_name", name).Int("retries_left", retries).Msg("process failed; retrying")
		status = runProcess(ctx, name, proc, factory, log)
	}
	return status
}

func runProcess(ctx context.Context, name string, proc config.Process, factory *logging.Factory, log zerolog.Logger) string {
	log.Info().Str("process_name", name).Str("path", proc.Path).Strs("args", proc.Args).Msg("starting process")

	cmd := exec.CommandContext(ctx, proc.Path, proc.Args...)
	if len(proc.Env) > 0 {
		cmd.Env = os.Environ()
		for k, v := range proc.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	stdout := factory.ProcessWriter(name, proc.HidesLabel())
	stderr := factory.ProcessWriter(name, proc.HidesLabel())
	cmd.Stdout, cmd.Stderr = stdout, stderr

	err := cmd.Run()
	stdout.Close()
	stderr.Close()

	if err != nil {
		log.Error().Err(err).Str("process_name", name).Msg("process exited with error")
		return statusFailure
	}
	log.Info().Str("process_name", name).Msg("process exited successfully")
	return statusSuccess
}
