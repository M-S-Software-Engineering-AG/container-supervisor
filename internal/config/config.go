// Package config loads the supervisor configuration from a YAML file and/or
// SUPERVISOR_-prefixed environment variables.
package config

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// yamlErrNoise matches the framing yaml.v3 wraps decode errors in: the "yaml:"
// prefix, the "unmarshal errors:" header, and per-error "line N:" positions.
// The line numbers point into the re-marshalled merge of file and env, not the
// user's file, so they mislead; the offending key name carries the message.
var yamlErrNoise = regexp.MustCompile(`(?m)^\s*(yaml: )?(unmarshal errors:)?\s*(line \d+: )?`)

// cleanYAMLError renders a decode error as a single readable line.
func cleanYAMLError(err error) string {
	s := strings.TrimSpace(yamlErrNoise.ReplaceAllString(err.Error(), ""))
	return strings.ReplaceAll(s, "\n", "; ")
}

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
	// Enabled, when explicitly false, keeps this process from ever starting; it
	// reports as a failure to anything that depends on it. Unset means enabled.
	// Read via IsEnabled.
	Enabled *Bool `yaml:"enabled"`
	// Cron is a standard 5-field schedule expression (minute hour day-of-month
	// month day-of-week), required and validated when Type is "cron".
	Cron string `yaml:"cron"`
	// RunAtStart, for a cron, runs it once as soon as its dependencies are
	// satisfied, on top of its schedule. Defaults to false: a schedule alone
	// means the first run is the first matching slot. Only valid for a cron.
	RunAtStart Bool `yaml:"run_at_start"`
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

// IsEnabled reports whether this process may start. Nil-safe: an unset enabled
// means true.
func (p Process) IsEnabled() bool { return p.Enabled == nil || bool(*p.Enabled) }

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
	// KnownFields rejects keys the Config structs don't define, so a typo in the
	// file (or a stray SUPERVISOR_ env var) aborts instead of being ignored.
	dec := yaml.NewDecoder(bytes.NewReader(merged))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil && err != io.EOF {
		return nil, fmt.Errorf("decode config: %s", cleanYAMLError(err))
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
			// Gating waits for the upstream to exit, so an upstream that never
			// does would block the dependent for the life of the container. A
			// service is not expected to exit; a cron keeps running its
			// schedule. An unknown dep has a zero Type here; supervisor.Validate
			// reports it.
			switch cfg.Processes[dep].Type {
			case TypeService:
				return nil, fmt.Errorf("process %q: cannot depends_on %q: a service is not expected to exit", name, dep)
			case TypeCron:
				return nil, fmt.Errorf("process %q: cannot depends_on %q: a cron runs on its schedule and never exits", name, dep)
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
				if _, err := ParseCron(proc.Cron); err != nil {
					return nil, fmt.Errorf("process %q: invalid cron %q: %w", name, proc.Cron, err)
				}
			}
		default:
			return nil, fmt.Errorf("process %q: invalid type %q (want service, one_shot, or cron)", name, proc.Type)
		}
		if proc.Type != TypeCron && bool(proc.RunAtStart) {
			return nil, fmt.Errorf("process %q: run_at_start is only valid for type cron", name)
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

// Indices into Schedule.fields, matching cronFields.
const (
	fMinute = iota
	fHour
	fDOM
	fMonth
	fDOW
)

// maxLookahead bounds Schedule.Next. Eight years covers the sparsest legal
// schedule, "0 0 29 2 *" crossing 2100 — a year divisible by 4 that is not a
// leap year, so the gap between 29 Februaries stretches to seven years.
const maxLookahead = 8

// Schedule is a parsed cron expression: a bitset of permitted values per field
// (every field's maximum fits in 64 bits). domStar and dowStar record whether
// the field was literally "*", which the bitset cannot express — "*" and "0-6"
// set identical bits — and which the day rule in dayMatches needs.
type Schedule struct {
	fields  [5]uint64
	domStar bool
	dowStar bool
}

// ParseCron parses a standard 5-field cron schedule (minute hour day-of-month
// month day-of-week). Each field is a comma-separated list of "*", a number, or
// a "lo-hi" range, any of which may carry a "/step". A step on a bare number
// runs to the end of the field, so "5/15" in the minute field means
// 5,20,35,50 — as crontab(5) reads it.
func ParseCron(expr string) (Schedule, error) {
	var s Schedule
	parts := strings.Fields(expr)
	if len(parts) != len(cronFields) {
		return s, fmt.Errorf("want 5 space-separated fields, got %d", len(parts))
	}
	for i, f := range cronFields {
		for _, term := range strings.Split(parts[i], ",") {
			bits, err := parseCronTerm(term, f)
			if err != nil {
				return s, fmt.Errorf("%s field: %w", f.name, err)
			}
			s.fields[i] |= bits
		}
	}
	// "Restricted" means the field names specific days. A leading "*" is
	// unrestricted even with a step ("*/2"), which is how crontab(5) reads it.
	s.domStar = strings.HasPrefix(parts[fDOM], "*")
	s.dowStar = strings.HasPrefix(parts[fDOW], "*")
	// A schedule no date can satisfy ("0 0 30 2 *") passes every per-field
	// check. Probing once here makes it a load error rather than a process
	// that silently never runs.
	if s.Next(time.Now()).IsZero() {
		return s, fmt.Errorf("no date can ever satisfy this schedule")
	}
	return s, nil
}

func parseCronTerm(term string, f cronField) (uint64, error) {
	base, stepStr, hasStep := strings.Cut(term, "/")
	step := 1
	if hasStep {
		n, err := strconv.Atoi(stepStr)
		if err != nil || n < 1 {
			return 0, fmt.Errorf("bad step %q", stepStr)
		}
		step = n
	}

	lo, hi := f.min, f.max
	if base != "*" {
		loStr, hiStr, isRange := strings.Cut(base, "-")
		var err error
		if lo, err = parseCronNum(loStr, f); err != nil {
			return 0, err
		}
		switch {
		case isRange:
			if hi, err = parseCronNum(hiStr, f); err != nil {
				return 0, err
			}
			if lo > hi {
				return 0, fmt.Errorf("inverted range %d-%d", lo, hi)
			}
		case hasStep:
			// "5/15" is "from 5 to the end of the field, every 15".
		default:
			hi = lo
		}
	}

	var bits uint64
	for v := lo; v <= hi; v += step {
		bits |= 1 << uint(v)
	}
	return bits, nil
}

func parseCronNum(s string, f cronField) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("bad value %q", s)
	}
	if n < f.min || n > f.max {
		return 0, fmt.Errorf("value %d out of range %d-%d", n, f.min, f.max)
	}
	return n, nil
}

func (s Schedule) has(field, v int) bool { return s.fields[field]&(1<<uint(v)) != 0 }

// dayMatches applies the standard day rule: when both day-of-month and
// day-of-week are restricted, a day qualifies if either one matches; when only
// one is restricted, only that one gates.
func (s Schedule) dayMatches(t time.Time) bool {
	switch {
	case s.domStar && s.dowStar:
		return true
	case s.domStar:
		return s.has(fDOW, int(t.Weekday()))
	case s.dowStar:
		return s.has(fDOM, t.Day())
	}
	return s.has(fDOM, t.Day()) || s.has(fDOW, int(t.Weekday()))
}

// Next returns the first minute strictly after t that the schedule matches, or
// the zero time if none falls within maxLookahead years. A day whose date
// cannot match is skipped whole, which keeps the sparsest schedules at days of
// iteration rather than minutes.
func (s Schedule) Next(t time.Time) time.Time {
	t = t.Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(maxLookahead, 0, 0)
	for t.Before(limit) {
		if !s.has(fMonth, int(t.Month())) || !s.dayMatches(t) {
			// Midnight tomorrow, in t's own location so DST shifts normalise.
			t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location()).AddDate(0, 0, 1)
			continue
		}
		if s.has(fHour, t.Hour()) && s.has(fMinute, t.Minute()) {
			return t
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}
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
