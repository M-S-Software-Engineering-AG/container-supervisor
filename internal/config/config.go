// Package config loads the supervisor configuration from a YAML file and/or
// SUPERVISOR_-prefixed environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// envPrefix is stripped from environment variable names; the remainder is
// lowercased and split on "__" to address nested config keys, e.g.
// SUPERVISOR_PROCESSES__API__PATH=./api -> processes.api.path.
const envPrefix = "SUPERVISOR_"

// Process types. Every process must declare one. A service is long-running:
// exiting at all (even 0) is unexpected and aborts the run. A one_shot is
// expected to finish: exit 0 is a clean completion. A cron runs on the schedule
// in its Cron field. A non-zero exit routes through OnFailure.
const (
	TypeService = "service"
	TypeOneShot = "one_shot"
	TypeCron    = "cron"
)

type Process struct {
	Path string `yaml:"path"`
	// Args are passed to the process verbatim, e.g. ["--dry-run"]. Kept separate
	// from Path so values with spaces need no quoting/escaping.
	Args Args `yaml:"arguments"`
	// Env adds environment variables for the process on top of the supervisor's
	// own environment; a key here overrides an inherited one of the same name.
	Env map[string]string `yaml:"environment"`
	// DependsOn gates startup on upstream outcomes. Any type may depend, but the
	// upstream must not be a service, which is not expected to exit.
	DependsOn map[string]Dependency `yaml:"depends_on"`
	// Type is required: "service", "one_shot", or "cron"; see the Type* constants.
	Type string `yaml:"type"`
	// Cron is a standard 5-field schedule expression (minute hour day-of-month
	// month day-of-week), required and validated when Type is "cron".
	Cron string `yaml:"cron"`
	// HideLabel drops this process's name from its output lines: no "[name] "
	// prefix in console (the output starts at the start of the line), and in
	// json a line that is already json passes through unwrapped. Unset (nil)
	// takes the global Config.HideLabels; Load resolves it. Read via HidesLabel.
	HideLabel *Bool `yaml:"hide_label"`
	// OnFailure controls what happens when this process ends unexpectedly.
	// For one_shot and cron (a non-zero exit): "fail" (default) aborts the whole
	// run, "continue" tolerates it and lets other branches proceed, "retry
	// <count>" re-runs it up to count times and, if it still fails, aborts like
	// "fail". For service (any exit, since a service should stay up): "exit"
	// (default) aborts the run, "restart" re-runs it until the run is cancelled,
	// "continue" tolerates the stop.
	OnFailure string `yaml:"on_failure"`
}

// Args is a process's argument list. In YAML it's a normal sequence
// (["--dry-run", "--limit", "10"]); from a scalar — such as a SUPERVISOR_ env
// var, which can only carry a string — it's split on whitespace.
type Args []string

// UnmarshalYAML accepts either a sequence or a whitespace-separated scalar, so
// arguments can be set via env vars, not only in the config file.
// ponytail: whitespace split means env-set args can't themselves contain
// spaces; use the YAML sequence form when an argument must.
func (a *Args) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		*a = strings.Fields(node.Value)
		return nil
	}
	var s []string
	if err := node.Decode(&s); err != nil {
		return err
	}
	*a = s
	return nil
}

// Bool is a boolean that also accepts a quoted scalar, so it can be set from a
// SUPERVISOR_ env var, which can only carry a string.
type Bool bool

func (b *Bool) UnmarshalYAML(node *yaml.Node) error {
	v, err := strconv.ParseBool(node.Value)
	if err != nil {
		return fmt.Errorf("invalid boolean %q", node.Value)
	}
	*b = Bool(v)
	return nil
}

// HidesLabel reports whether this process's output lines drop its name. Nil-safe:
// a process that never set hide_label was resolved to the global default by Load.
func (p Process) HidesLabel() bool { return p.HideLabel != nil && bool(*p.HideLabel) }

// RetryCount returns n when OnFailure is "retry n", else 0. Only meaningful
// after Load has validated the value.
func (p Process) RetryCount() int {
	if s, ok := strings.CutPrefix(p.OnFailure, "retry "); ok {
		n, _ := strconv.Atoi(strings.TrimSpace(s))
		return n
	}
	return 0
}

// Dependency is a condition on an upstream process, keyed by that process's
// name in Process.DependsOn. Exit is the required exit outcome: "success"
// (exit 0), "failure" (non-zero), or "any" (exited at all).
type Dependency struct {
	Exit string `yaml:"exit"`
}

type Config struct {
	LogLevel        string `yaml:"loglevel"`
	LogOutputFormat string `yaml:"log_output_format"`
	// HideLabels is the default for every process's HideLabel; a process that
	// sets hide_label itself wins either way.
	HideLabels Bool               `yaml:"hide_labels"`
	Processes  map[string]Process `yaml:"processes"`
}

// Load reads config from the YAML file at path (skipped if empty) and overlays
// any SUPERVISOR_ environment variables on top. LogLevel defaults to "info".
func Load(path string) (*Config, error) {
	raw := map[string]any{}
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if err := yaml.Unmarshal(data, &raw); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	}

	applyEnv(raw)

	merged, err := yaml.Marshal(raw)
	if err != nil {
		return nil, err
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(merged, cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	if cfg.LogOutputFormat == "" {
		cfg.LogOutputFormat = "console"
	}
	for name, proc := range cfg.Processes {
		if proc.HideLabel == nil {
			proc.HideLabel = &cfg.HideLabels
		}
		for dep, cond := range proc.DependsOn {
			// Gating waits for the upstream to exit, so a service upstream would
			// block the dependent for the life of the container. An unknown dep
			// has a zero Type here; supervisor.Validate reports it.
			if cfg.Processes[dep].Type == TypeService {
				return nil, fmt.Errorf("process %q: cannot depends_on %q: a service is not expected to exit", name, dep)
			}
			switch cond.Exit {
			case "success", "failure", "any":
			default:
				return nil, fmt.Errorf("process %q depends_on %q: invalid exit %q (want success, failure, or any)", name, dep, cond.Exit)
			}
		}
		switch proc.Type {
		case "":
			return nil, fmt.Errorf("process %q: type is required (want service, one_shot, or cron)", name)
		case TypeService:
			switch proc.OnFailure {
			case "", "exit", "restart", "continue":
			default:
				return nil, fmt.Errorf("process %q: invalid on_failure %q for service (want exit, restart, or continue)", name, proc.OnFailure)
			}
		case TypeOneShot, TypeCron:
			switch {
			case proc.OnFailure == "", proc.OnFailure == "fail", proc.OnFailure == "continue":
			case strings.HasPrefix(proc.OnFailure, "retry "):
				if n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(proc.OnFailure, "retry "))); err != nil || n < 1 {
					return nil, fmt.Errorf("process %q: invalid on_failure %q (retry needs a positive count)", name, proc.OnFailure)
				}
			default:
				return nil, fmt.Errorf("process %q: invalid on_failure %q (want continue, retry <count>, or fail)", name, proc.OnFailure)
			}
			if proc.Type == TypeCron {
				if proc.Cron == "" {
					return nil, fmt.Errorf("process %q: type cron requires a cron schedule", name)
				}
				if err := validateCron(proc.Cron); err != nil {
					return nil, fmt.Errorf("process %q: invalid cron %q: %w", name, proc.Cron, err)
				}
			}
		default:
			return nil, fmt.Errorf("process %q: invalid type %q (want service, one_shot, or cron)", name, proc.Type)
		}
		cfg.Processes[name] = proc
	}
	return cfg, nil
}

// cronField is one position in a standard 5-field cron expression, with its
// permitted value range (inclusive).
type cronField struct {
	name     string
	min, max int
}

var cronFields = []cronField{
	{"minute", 0, 59},
	{"hour", 0, 23},
	{"day-of-month", 1, 31},
	{"month", 1, 12},
	{"day-of-week", 0, 6},
}

// validateCron checks expr is a standard 5-field cron schedule. Each field is a
// comma-separated list of "*", a number, or a "lo-hi" range, any of which may
// carry a "/step". It validates ranges and bounds, not scheduling semantics.
func validateCron(expr string) error {
	parts := strings.Fields(expr)
	if len(parts) != len(cronFields) {
		return fmt.Errorf("want 5 space-separated fields, got %d", len(parts))
	}
	for i, f := range cronFields {
		for _, term := range strings.Split(parts[i], ",") {
			if err := validateCronTerm(term, f); err != nil {
				return fmt.Errorf("%s field: %w", f.name, err)
			}
		}
	}
	return nil
}

func validateCronTerm(term string, f cronField) error {
	base, step, hasStep := strings.Cut(term, "/")
	if hasStep {
		if n, err := strconv.Atoi(step); err != nil || n < 1 {
			return fmt.Errorf("bad step %q", step)
		}
	}
	if base == "*" {
		return nil
	}
	lo, hi, isRange := strings.Cut(base, "-")
	if err := validateCronNum(lo, f); err != nil {
		return err
	}
	if isRange {
		if err := validateCronNum(hi, f); err != nil {
			return err
		}
	}
	return nil
}

func validateCronNum(s string, f cronField) error {
	n, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("bad value %q", s)
	}
	if n < f.min || n > f.max {
		return fmt.Errorf("value %d out of range %d-%d", n, f.min, f.max)
	}
	return nil
}

func applyEnv(m map[string]any) {
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(k, envPrefix) {
			continue
		}
		path := strings.Split(strings.ToLower(strings.TrimPrefix(k, envPrefix)), "__")
		setNested(m, path, v)
	}
}

// setNested writes val at the given key path, creating intermediate maps.
// Each __ segment is a map key, so any scalar leaf is reachable at any depth
// (e.g. depends_on.<name>.exit). List-valued fields have no path here; the
// arguments list instead accepts a scalar via Args.UnmarshalYAML.
func setNested(m map[string]any, path []string, val string) {
	for i, key := range path {
		if i == len(path)-1 {
			m[key] = val
			return
		}
		next, ok := m[key].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[key] = next
		}
		m = next
	}
}
