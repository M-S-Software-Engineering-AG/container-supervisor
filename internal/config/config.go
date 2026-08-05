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

var yamlErrNoise = regexp.MustCompile(`(?m)^\s*(yaml: )?(unmarshal errors:)?\s*(line \d+: )?`)

func cleanYAMLError(err error) string {
	s := strings.TrimSpace(yamlErrNoise.ReplaceAllString(err.Error(), ""))
	return strings.ReplaceAll(s, "\n", "; ")
}

const envPrefix = "SUPERVISOR_"

const (
	TypeService = "service"
	TypeOneShot = "one_shot"
	TypeCron    = "cron"
	TypeTicker  = "ticker"
)

const validTypes = "service, one_shot, cron, or ticker"

type Process struct {
	Path       string                `yaml:"path"`
	Args       Args                  `yaml:"arguments"`
	Env        map[string]string     `yaml:"environment"`
	DependsOn  map[string]Dependency `yaml:"depends_on"`
	Type       string                `yaml:"type"`
	Enabled    *Bool                 `yaml:"enabled"`
	Cron       string                `yaml:"cron"`
	Ticker     string                `yaml:"ticker"`
	RunAtStart Bool                  `yaml:"run_at_start"`
	HideLabel  *Bool                 `yaml:"hide_label"`
	OnFailure  string                `yaml:"on_failure"`
}

type Args []string

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

type Bool bool

func (b *Bool) UnmarshalYAML(node *yaml.Node) error {
	v, err := strconv.ParseBool(node.Value)
	if err != nil {
		return fmt.Errorf("invalid boolean %q", node.Value)
	}
	*b = Bool(v)
	return nil
}

func (p Process) HidesLabel() bool { return p.HideLabel != nil && bool(*p.HideLabel) }

func (p Process) IsEnabled() bool { return p.Enabled == nil || bool(*p.Enabled) }

func (p Process) IsScheduled() bool { return p.Type == TypeCron || p.Type == TypeTicker }

func (p Process) scheduleExpr() string {
	switch p.Type {
	case TypeCron:
		return p.Cron
	case TypeTicker:
		return p.Ticker
	}
	return ""
}

func (p Process) ParseSchedule() (Schedule, error) {
	switch p.Type {
	case TypeCron:
		return ParseCron(p.Cron)
	case TypeTicker:
		return ParseEvery(p.Ticker)
	}
	return Schedule{}, fmt.Errorf("type %q has no schedule", p.Type)
}

func (p Process) RetryCount() int {
	if s, ok := strings.CutPrefix(p.OnFailure, "retry "); ok {
		n, _ := strconv.Atoi(strings.TrimSpace(s))
		return n
	}
	return 0
}

type Dependency struct {
	Exit string `yaml:"exit"`
}

type Config struct {
	LogLevel        string             `yaml:"loglevel"`
	LogOutputFormat string             `yaml:"log_output_format"`
	HideLabels      Bool               `yaml:"hide_labels"`
	Processes       map[string]Process `yaml:"processes"`
}

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
			switch depType := cfg.Processes[dep].Type; depType {
			case TypeService:
				return nil, fmt.Errorf("process %q: cannot depends_on %q: a service is not expected to exit", name, dep)
			case TypeCron, TypeTicker:
				return nil, fmt.Errorf("process %q: cannot depends_on %q: a %s runs on its schedule and never exits", name, dep, depType)
			}
			switch cond.Exit {
			case "success", "failure", "any":
			default:
				return nil, fmt.Errorf("process %q depends_on %q: invalid exit %q (want success, failure, or any)", name, dep, cond.Exit)
			}
		}
		switch proc.Type {
		case "":
			return nil, fmt.Errorf("process %q: type is required (want %s)", name, validTypes)
		case TypeService:
			switch proc.OnFailure {
			case "", "exit", "restart", "continue":
			default:
				return nil, fmt.Errorf("process %q: invalid on_failure %q for service (want exit, restart, or continue)", name, proc.OnFailure)
			}
		case TypeOneShot, TypeCron, TypeTicker:
			switch {
			case proc.OnFailure == "", proc.OnFailure == "fail", proc.OnFailure == "continue":
			case strings.HasPrefix(proc.OnFailure, "retry "):
				if n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(proc.OnFailure, "retry "))); err != nil || n < 1 {
					return nil, fmt.Errorf("process %q: invalid on_failure %q (retry needs a positive count)", name, proc.OnFailure)
				}
			default:
				return nil, fmt.Errorf("process %q: invalid on_failure %q (want continue, retry <count>, or fail)", name, proc.OnFailure)
			}
			if proc.IsScheduled() {
				expr := proc.scheduleExpr()
				if expr == "" {
					return nil, fmt.Errorf("process %q: type %s requires a %s schedule", name, proc.Type, proc.Type)
				}
				if _, err := proc.ParseSchedule(); err != nil {
					return nil, fmt.Errorf("process %q: invalid %s %q: %w", name, proc.Type, expr, err)
				}
			}
		default:
			return nil, fmt.Errorf("process %q: invalid type %q (want %s)", name, proc.Type, validTypes)
		}

		if proc.Type != TypeCron && proc.Cron != "" {
			return nil, fmt.Errorf("process %q: cron is only valid for type cron", name)
		}
		if proc.Type != TypeTicker && proc.Ticker != "" {
			return nil, fmt.Errorf("process %q: ticker is only valid for type ticker", name)
		}
		if !proc.IsScheduled() && bool(proc.RunAtStart) {
			return nil, fmt.Errorf("process %q: run_at_start is only valid for type cron or ticker", name)
		}
		cfg.Processes[name] = proc
	}
	return cfg, nil
}

const everyPrefix = "@every"

var everyUnits = strings.NewReplacer(
	"milliseconds", "ms", "millisecond", "ms", "millis", "ms", "msec", "ms",
	"seconds", "s", "second", "s", "secs", "s", "sec", "s",
	"minutes", "m", "minute", "m", "mins", "m", "min", "m",
	"hours", "h", "hour", "h", "hrs", "h", "hr", "h",
)

func ParseEvery(expr string) (Schedule, error) {
	interval := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(expr), everyPrefix))
	if interval == "" {
		return Schedule{}, fmt.Errorf(`want an interval, e.g. "@every 5sec"`)
	}
	d, err := time.ParseDuration(everyUnits.Replace(interval))
	if err != nil {
		return Schedule{}, fmt.Errorf("bad interval %q (want a number and a unit, e.g. 5sec)", interval)
	}
	if d <= 0 {
		return Schedule{}, fmt.Errorf("interval %q must be positive", interval)
	}

	return Schedule{every: d}, nil
}

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

const (
	fMinute = iota
	fHour
	fDOM
	fMonth
	fDOW
)

const maxLookahead = 8

type Schedule struct {
	fields  [5]uint64
	domStar bool
	dowStar bool
	every   time.Duration
}

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

	s.domStar = strings.HasPrefix(parts[fDOM], "*")
	s.dowStar = strings.HasPrefix(parts[fDOW], "*")

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

func (s Schedule) Next(t time.Time) time.Time {
	if s.every > 0 {
		return t.Add(s.every)
	}
	t = t.Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(maxLookahead, 0, 0)
	for t.Before(limit) {
		if !s.has(fMonth, int(t.Month())) || !s.dayMatches(t) {
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
