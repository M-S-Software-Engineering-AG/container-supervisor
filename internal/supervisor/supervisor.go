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

const (
	statusSuccess = "success"
	statusFailure = "failure"
	statusSkipped = "skipped"
)

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
		status string
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

			if proc.Type == config.TypeService && proc.OnFailure == "restart" {
				for ctx.Err() == nil {
					st.status = runProcess(ctx, name, proc, factory, log)
					if ctx.Err() == nil {
						log.Warn().Str("process_name", name).Msg("service exited; restarting")
					}
				}
				return
			}

			if proc.IsScheduled() {
				st.status = runScheduled(ctx, name, proc, factory, log)
				if st.status == statusFailure && ctx.Err() == nil {
					aborted.Store(true)
					log.Error().Str("process_name", name).Msg("critical process failed; aborting run")
					cancel()
				}
				return
			}

			st.status = runWithRetries(ctx, name, proc, factory, log)

			if ctx.Err() != nil {
				return
			}

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

var cronNext = func(s config.Schedule, after time.Time) time.Time { return s.Next(after) }

func runScheduled(ctx context.Context, name string, proc config.Process, factory *logging.Factory, log zerolog.Logger) string {
	sched, err := proc.ParseSchedule()
	if err != nil {
		log.Error().Err(err).Str("process_name", name).Str("type", proc.Type).Msg("invalid schedule")
		return statusFailure
	}

	runNow := bool(proc.RunAtStart)

	from := time.Now()
	for ctx.Err() == nil {
		if !runNow {
			next := cronNext(sched, from)
			if next.IsZero() {
				log.Error().Str("process_name", name).Str("type", proc.Type).Msg("schedule has no next occurrence")
				return statusFailure
			}
			log.Debug().Str("process_name", name).Time("next_run", next).Msg("next run scheduled")
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

		if now := time.Now(); now.After(from) {
			from = now
		}

		if ctx.Err() != nil {
			return statusSkipped
		}
		if status == statusFailure && proc.OnFailure != "continue" {
			return statusFailure
		}
	}
	return statusSkipped
}

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
