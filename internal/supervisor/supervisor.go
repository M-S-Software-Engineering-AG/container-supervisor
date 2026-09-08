package supervisor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

			log := log
			if proc.LogLevel != "" {
				lvl, _ := zerolog.ParseLevel(proc.LogLevel)
				log = log.Level(lvl)
			}

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
				if bool(cond.IgnoreExitWhenDisabled) && !cfg.Processes[depName].IsEnabled() {
					continue
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
					st.status = runProcess(ctx, name, proc, cfg.EnvDir, factory, log)
					if ctx.Err() == nil {
						log.Warn().Str("process_name", name).Msg("service exited; restarting")
					}
				}
				return
			}

			if proc.IsScheduled() {
				st.status = runScheduled(ctx, name, proc, cfg.EnvDir, factory, log)
				if st.status == statusFailure && ctx.Err() == nil {
					aborted.Store(true)
					log.Error().Str("process_name", name).Msg("critical process failed; aborting run")
					cancel()
				}
				return
			}

			st.status = runWithRetries(ctx, name, proc, cfg.EnvDir, factory, log)

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

func runScheduled(ctx context.Context, name string, proc config.Process, envDir string, factory *logging.Factory, log zerolog.Logger) string {
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

		status := runWithRetries(ctx, name, proc, envDir, factory, log)

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

func runWithRetries(ctx context.Context, name string, proc config.Process, envDir string, factory *logging.Factory, log zerolog.Logger) string {
	status := runProcess(ctx, name, proc, envDir, factory, log)
	for retries := proc.RetryCount(); status == statusFailure && retries > 0 && ctx.Err() == nil; retries-- {
		log.Warn().Str("process_name", name).Int("retries_left", retries).Msg("process failed; retrying")
		status = runProcess(ctx, name, proc, envDir, factory, log)
	}
	return status
}

// readEnvDir reads env_dir where each file is one variable: the filename is the
// key, the file's contents (trailing newline trimmed) are the value. This keeps
// multiline values (PEM keys, certs) intact, which a line-based format cannot.
func readEnvDir(dir string, log zerolog.Logger) map[string]string {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Error().Err(err).Str("env_dir", dir).Msg("reading env dir")
		}
		return nil
	}
	env := map[string]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			log.Error().Err(err).Str("file", e.Name()).Msg("reading env file")
			continue
		}
		env[e.Name()] = strings.TrimRight(string(data), "\n")
	}
	return env
}

// buildEnv is the child's full environment: the supervisor's own, then env_dir
// files, then the process's environment: block (config always wins).
func buildEnv(proc config.Process, envDir string, log zerolog.Logger) map[string]string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	for k, v := range readEnvDir(envDir, log) {
		env[k] = v
	}
	for k, v := range proc.Env {
		env[k] = v
	}
	return env
}

// prepareSecrets resolves Docker-style *_FILE variables named in proc.Secrets:
// for each listed NAME_FILE holding a path, it reads the file and writes
// env_dir/NAME so later processes see $NAME. Writes are atomic (temp + rename)
// to avoid torn reads.
func prepareSecrets(name string, proc config.Process, envDir string, log zerolog.Logger) string {
	if envDir == "" {
		log.Error().Str("process_name", name).Msg("prepare_secrets needs env_dir set")
		return statusFailure
	}
	if err := os.MkdirAll(envDir, 0o700); err != nil {
		log.Error().Err(err).Str("process_name", name).Str("env_dir", envDir).Msg("creating env dir")
		return statusFailure
	}
	env := buildEnv(proc, envDir, log)
	for _, varName := range proc.Secrets {
		path, ok := env[varName]
		if !ok || path == "" {
			log.Error().Str("process_name", name).Str("var", varName).Msg("secret variable is unset")
			return statusFailure
		}
		key := strings.TrimSuffix(varName, "_FILE")
		data, err := os.ReadFile(path)
		if err != nil {
			log.Error().Err(err).Str("process_name", name).Str("var", varName).Str("path", path).Msg("reading secret file")
			return statusFailure
		}
		tmp := filepath.Join(envDir, "."+key+".tmp")
		if err := os.WriteFile(tmp, data, 0o600); err != nil {
			log.Error().Err(err).Str("process_name", name).Str("var", key).Msg("writing secret")
			return statusFailure
		}
		if err := os.Rename(tmp, filepath.Join(envDir, key)); err != nil {
			log.Error().Err(err).Str("process_name", name).Str("var", key).Msg("writing secret")
			return statusFailure
		}
		log.Info().Str("process_name", name).Str("var", key).Msg("secret prepared")
	}
	log.Info().Str("process_name", name).Int("secrets", len(proc.Secrets)).Msg("prepare_secrets done")
	return statusSuccess
}

func runProcess(ctx context.Context, name string, proc config.Process, envDir string, factory *logging.Factory, log zerolog.Logger) string {
	if proc.Type == config.TypePrepareSecrets {
		return prepareSecrets(name, proc, envDir, log)
	}

	log.Info().Str("process_name", name).Str("path", proc.Path).Strs("args", proc.Args).Msg("starting process")

	cmd := exec.CommandContext(ctx, proc.Path, proc.Args...)
	env := buildEnv(proc, envDir, log)
	cmd.Env = make([]string, 0, len(env))
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
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
