package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeYAML(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadYAML(t *testing.T) {
	p := writeYAML(t, `
processes:
  db:
    path: /bin/postgres
    type: service
  migrate:
    path: /bin/migrate
    type: one_shot
  api:
    path: /bin/api
    type: service
    depends_on:
      migrate:
        exit: success
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("loglevel default: got %q want info", cfg.LogLevel)
	}
	if cfg.LogOutputFormat != "console" {
		t.Errorf("log_output_format default: got %q want console", cfg.LogOutputFormat)
	}
	if cfg.Processes["api"].Path != "/bin/api" {
		t.Errorf("api.path: got %q", cfg.Processes["api"].Path)
	}
	if got := cfg.Processes["api"].DependsOn["migrate"].Exit; got != "success" {
		t.Errorf("api.depends_on[migrate].exit: got %q want success", got)
	}
	if got := cfg.Processes["db"].Type; got != TypeService {
		t.Errorf("db.type: got %q want service", got)
	}
}

func TestHideLabels(t *testing.T) {
	p := writeYAML(t, `
hide_labels: true
processes:
  a:
    path: /bin/a
    type: service
  b:
    path: /bin/b
    type: service
    hide_label: false
  c:
    path: /bin/c
    type: service
    hide_label: true
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"a": true, "b": false, "c": true}
	for name, w := range want {
		if got := cfg.Processes[name].HidesLabel(); got != w {
			t.Errorf("%s.HidesLabel(): got %v want %v", name, got, w)
		}
	}

	if cfg2, err := Load(writeYAML(t, "processes:\n  a:\n    path: /bin/a\n    type: service\n")); err != nil {
		t.Fatal(err)
	} else if cfg2.Processes["a"].HidesLabel() {
		t.Error("a.HidesLabel(): got true want false without hide_labels")
	}
	if (Process{}).HidesLabel() {
		t.Error("zero Process.HidesLabel(): got true want false")
	}
}

func TestEnabled(t *testing.T) {
	p := writeYAML(t, `
processes:
  a:
    path: /bin/a
    type: one_shot
  b:
    path: /bin/b
    type: one_shot
    enabled: false
  c:
    path: /bin/c
    type: one_shot
    enabled: true
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"a": true, "b": false, "c": true}
	for name, w := range want {
		if got := cfg.Processes[name].IsEnabled(); got != w {
			t.Errorf("%s.IsEnabled(): got %v want %v", name, got, w)
		}
	}
	if !(Process{}).IsEnabled() {
		t.Error("zero Process.IsEnabled(): got false want true")
	}
}

func TestCronType(t *testing.T) {
	p := writeYAML(t, "processes:\n  backup:\n    path: /bin/backup\n    type: cron\n    cron: \"0 3 * * *\"\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Processes["backup"].Cron; got != "0 3 * * *" {
		t.Errorf("backup.cron: got %q want %q", got, "0 3 * * *")
	}
}

func TestCronMissingSchedule(t *testing.T) {
	p := writeYAML(t, "processes:\n  backup:\n    path: /bin/backup\n    type: cron\n")
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for cron type without cron schedule, got nil")
	}
}

func TestParseCron(t *testing.T) {
	valid := []string{"0 3 * * *", "*/15 * * * *", "0 0,12 1-15 */2 1-5", "59 23 31 12 6",
		"10-50/7 * * * *", "5/15 * * * *", "0 0 29 2 *"}
	for _, expr := range valid {
		if _, err := ParseCron(expr); err != nil {
			t.Errorf("ParseCron(%q): unexpected error: %v", expr, err)
		}
	}
	invalid := []string{"", "0 3 * *", "0 3 * * * *", "60 * * * *", "* 24 * * *", "* * 0 * *",
		"* * * 13 *", "* * * * 7", "*/0 * * * *", "1-x * * * *", "a * * * *",
		"50-10 * * * *",
		"0 0 30 2 *",
	}
	for _, expr := range invalid {
		if _, err := ParseCron(expr); err == nil {
			t.Errorf("ParseCron(%q): expected error, got nil", expr)
		}
	}
}

func TestScheduleNext(t *testing.T) {
	at := func(y int, mo time.Month, d, h, mi int) time.Time {
		return time.Date(y, mo, d, h, mi, 0, 0, time.UTC)
	}
	cases := []struct {
		expr string
		from time.Time
		want time.Time
	}{
		{"* * * * *", at(2026, time.July, 28, 10, 30), at(2026, time.July, 28, 10, 31)},
		{"0 3 * * *", at(2026, time.July, 28, 3, 0), at(2026, time.July, 29, 3, 0)},
		{"0 3 * * *", at(2026, time.July, 28, 2, 59), at(2026, time.July, 28, 3, 0)},
		{"0 3 * * *", at(2026, time.July, 31, 4, 0), at(2026, time.August, 1, 3, 0)},
		{"30 0 1 1 *", at(2026, time.July, 28, 0, 0), at(2027, time.January, 1, 0, 30)},
		{"*/15 * * * *", at(2026, time.July, 28, 10, 1), at(2026, time.July, 28, 10, 15)},
		{"10-50/7 * * * *", at(2026, time.July, 28, 10, 11), at(2026, time.July, 28, 10, 17)},
		{"5/15 * * * *", at(2026, time.July, 28, 10, 6), at(2026, time.July, 28, 10, 20)},
		{"0 0 29 2 *", at(2026, time.July, 28, 0, 0), at(2028, time.February, 29, 0, 0)},
		{"0 0 * * 1", at(2026, time.July, 28, 0, 0), at(2026, time.August, 3, 0, 0)},
		{"0 0 15 * *", at(2026, time.July, 28, 0, 0), at(2026, time.August, 15, 0, 0)},
		{"0 0 1 * 1", at(2026, time.July, 28, 0, 0), at(2026, time.August, 1, 0, 0)},
		{"0 0 15 * */1", at(2026, time.July, 28, 0, 0), at(2026, time.August, 15, 0, 0)},
	}
	for _, c := range cases {
		s, err := ParseCron(c.expr)
		if err != nil {
			t.Errorf("ParseCron(%q): %v", c.expr, err)
			continue
		}
		if got := s.Next(c.from); !got.Equal(c.want) {
			t.Errorf("ParseCron(%q).Next(%s) = %s, want %s", c.expr,
				c.from.Format(time.RFC3339), got.Format(time.RFC3339), c.want.Format(time.RFC3339))
		}
	}
}

func TestScheduleNextAdvances(t *testing.T) {
	s, err := ParseCron("*/1 * * * *")
	if err != nil {
		t.Fatalf("ParseCron: %v", err)
	}
	prev := time.Date(2026, time.July, 28, 8, 45, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		next := s.Next(prev)
		if d := next.Sub(prev); d != time.Minute {
			t.Fatalf("Next(%s) = %s, advanced %v, want 1m",
				prev.Format(time.RFC3339), next.Format(time.RFC3339), d)
		}
		prev = next
	}

	almost := time.Date(2026, time.July, 28, 8, 45, 59, int(999*time.Millisecond), time.UTC)
	if got, want := s.Next(almost), time.Date(2026, time.July, 28, 8, 46, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("Next(%s) = %s, want %s", almost.Format(time.RFC3339Nano),
			got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

func TestRunAtStartOnlyForCron(t *testing.T) {
	for _, typ := range []string{"one_shot", "service"} {
		p := writeYAML(t, "processes:\n  p:\n    path: /bin/p\n    type: "+typ+"\n    run_at_start: true\n")
		if _, err := Load(p); err == nil {
			t.Errorf("type %s: expected error for run_at_start, got nil", typ)
		}
	}
	p := writeYAML(t, "processes:\n  p:\n    path: /bin/p\n    type: cron\n    cron: \"0 3 * * *\"\n    run_at_start: true\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("cron with run_at_start should load: %v", err)
	}
	if !bool(cfg.Processes["p"].RunAtStart) {
		t.Error("p.run_at_start: got false, want true")
	}
}

func TestCronInvalidSchedule(t *testing.T) {
	p := writeYAML(t, "processes:\n  backup:\n    path: /bin/backup\n    type: cron\n    cron: \"nonsense\"\n")
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for invalid cron schedule, got nil")
	}
}

func TestParseEvery(t *testing.T) {
	valid := map[string]time.Duration{
		"@every 5sec":      5 * time.Second,
		"@every 5s":        5 * time.Second,
		"@every 30secs":    30 * time.Second,
		"@every 1second":   time.Second,
		"@every 2minutes":  2 * time.Minute,
		"@every 90min":     90 * time.Minute,
		"@every 1hour":     time.Hour,
		"@every 250ms":     250 * time.Millisecond,
		"@every 1min30sec": 90 * time.Second,
		"5sec":             5 * time.Second,
		"  @every  5sec ":  5 * time.Second,
	}
	for expr, want := range valid {
		s, err := ParseEvery(expr)
		if err != nil {
			t.Errorf("ParseEvery(%q): unexpected error: %v", expr, err)
			continue
		}
		from := time.Date(2026, time.July, 28, 10, 30, 0, 0, time.UTC)
		if got := s.Next(from).Sub(from); got != want {
			t.Errorf("ParseEvery(%q): interval %v, want %v", expr, got, want)
		}
	}
	invalid := []string{"", "@every", "@every 5", "@every sec", "@every 0sec", "@every -5sec",
		"@every 5 5 sec", "@every fivesec", "0 3 * * *"}
	for _, expr := range invalid {
		if _, err := ParseEvery(expr); err == nil {
			t.Errorf("ParseEvery(%q): expected error, got nil", expr)
		}
	}
}

func TestTickerNextAdvances(t *testing.T) {
	s, err := ParseEvery("@every 5sec")
	if err != nil {
		t.Fatalf("ParseEvery: %v", err)
	}
	prev := time.Date(2026, time.July, 28, 10, 30, 12, 500, time.UTC)
	for i := 0; i < 3; i++ {
		next := s.Next(prev)
		if d := next.Sub(prev); d != 5*time.Second {
			t.Fatalf("Next(%s) advanced %v, want 5s", prev.Format(time.RFC3339Nano), d)
		}
		prev = next
	}
}

func TestTickerType(t *testing.T) {
	p := writeYAML(t, "processes:\n  poll:\n    path: /bin/poll\n    type: ticker\n    ticker: \"@every 5sec\"\n    run_at_start: true\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Processes["poll"].Ticker; got != "@every 5sec" {
		t.Errorf("poll.ticker: got %q want %q", got, "@every 5sec")
	}

	bad := map[string]string{
		"missing interval":       "processes:\n  poll:\n    path: /bin/poll\n    type: ticker\n",
		"invalid interval":       "processes:\n  poll:\n    path: /bin/poll\n    type: ticker\n    ticker: \"@every nonsense\"\n",
		"cron key on ticker":     "processes:\n  poll:\n    path: /bin/poll\n    type: ticker\n    ticker: \"@every 5sec\"\n    cron: \"0 3 * * *\"\n",
		"ticker key on cron":     "processes:\n  poll:\n    path: /bin/poll\n    type: cron\n    cron: \"0 3 * * *\"\n    ticker: \"@every 5sec\"\n",
		"ticker key on one_shot": "processes:\n  poll:\n    path: /bin/poll\n    type: one_shot\n    ticker: \"@every 5sec\"\n",
	}
	for name, yaml := range bad {
		if _, err := Load(writeYAML(t, yaml)); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

func TestDependsOnTickerRejected(t *testing.T) {
	p := writeYAML(t, "processes:\n  poll:\n    path: /bin/poll\n    type: ticker\n    ticker: \"@every 5sec\"\n"+
		"  app:\n    path: /bin/app\n    type: service\n    depends_on:\n      poll:\n        exit: success\n")
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for depends_on a ticker, got nil")
	}
}

func TestServiceOnFailure(t *testing.T) {
	ok := []string{"", "exit", "restart", "continue"}
	for _, v := range ok {
		p := writeYAML(t, "processes:\n  svc:\n    path: /bin/svc\n    type: service\n    on_failure: \""+v+"\"\n")
		if _, err := Load(p); err != nil {
			t.Errorf("service on_failure %q: unexpected error: %v", v, err)
		}
	}
	bad := []string{"fail", "retry 2", "restartt"}
	for _, v := range bad {
		p := writeYAML(t, "processes:\n  svc:\n    path: /bin/svc\n    type: service\n    on_failure: \""+v+"\"\n")
		if _, err := Load(p); err == nil {
			t.Errorf("service on_failure %q: expected error, got nil", v)
		}
	}
}

func TestRestartOnlyForService(t *testing.T) {
	for _, typ := range []string{"one_shot", "cron"} {
		body := "processes:\n  p:\n    path: /bin/p\n    type: " + typ + "\n    on_failure: restart\n"
		if typ == "cron" {
			body += "    cron: \"0 3 * * *\"\n"
		}
		if _, err := Load(writeYAML(t, body)); err == nil {
			t.Errorf("type %s on_failure restart: expected error, got nil", typ)
		}
	}
}

func TestMissingType(t *testing.T) {
	p := writeYAML(t, "processes:\n  api:\n    path: /bin/api\n")
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for missing type, got nil")
	}
}

func TestProcessType(t *testing.T) {
	p := writeYAML(t, "processes:\n  migrate:\n    path: /bin/migrate\n    type: one_shot\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Processes["migrate"].Type; got != TypeOneShot {
		t.Errorf("migrate.type: got %q want one_shot", got)
	}
}

func TestInvalidType(t *testing.T) {
	p := writeYAML(t, "processes:\n  api:\n    path: /bin/api\n    type: daemon\n")
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for invalid type value, got nil")
	}
}

func TestUnknownKeyRejected(t *testing.T) {
	cases := map[string]string{
		"top level": "loglevl: debug\n",
		"process":   "processes:\n  api:\n    type: service\n    pth: /bin/api\n",
		"depends_on": "processes:\n  migrate:\n    type: one_shot\n" +
			"  api:\n    type: service\n    depends_on:\n      migrate:\n        exit: success\n        when: later\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeYAML(t, body))
			if err == nil {
				t.Fatal("expected error for unknown key, got nil")
			}
			if strings.Contains(err.Error(), "line ") {
				t.Errorf("error leaks a line number from the merged doc: %v", err)
			}
		})
	}
}

func TestUnknownEnvKeyRejected(t *testing.T) {
	p := writeYAML(t, "processes:\n  api:\n    path: /bin/api\n    type: service\n")
	t.Setenv("SUPERVISOR_PROCESSES__API__PTH", "/bin/typo")
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for unknown env-set key, got nil")
	}
}

func TestEnvOverride(t *testing.T) {
	p := writeYAML(t, "processes:\n  api:\n    path: /bin/old\n    type: service\n")
	t.Setenv("SUPERVISOR_LOGLEVEL", "debug")
	t.Setenv("SUPERVISOR_LOG_OUTPUT_FORMAT", "json")
	t.Setenv("SUPERVISOR_PROCESSES__API__PATH", "/bin/api")

	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("loglevel: got %q want debug", cfg.LogLevel)
	}
	if cfg.Processes["api"].Path != "/bin/api" {
		t.Errorf("env override: got %q want /bin/api", cfg.Processes["api"].Path)
	}
}

func TestEnvDefinesDependsOn(t *testing.T) {
	t.Setenv("SUPERVISOR_PROCESSES__MIGRATE__PATH", "/bin/migrate")
	t.Setenv("SUPERVISOR_PROCESSES__MIGRATE__TYPE", "one_shot")
	t.Setenv("SUPERVISOR_PROCESSES__DB__PATH", "/bin/postgres")
	t.Setenv("SUPERVISOR_PROCESSES__DB__TYPE", "service")
	t.Setenv("SUPERVISOR_PROCESSES__DB__DEPENDS_ON__MIGRATE__EXIT", "success")

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Processes["db"].DependsOn["migrate"].Exit; got != "success" {
		t.Errorf("depends_on via env: got %q want success", got)
	}
}

func TestEnvDefinesArguments(t *testing.T) {
	t.Setenv("SUPERVISOR_PROCESSES__API__PATH", "/bin/api")
	t.Setenv("SUPERVISOR_PROCESSES__API__TYPE", "one_shot")
	t.Setenv("SUPERVISOR_PROCESSES__API__ARGUMENTS", "--dry-run --limit 10")

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	got := []string(cfg.Processes["api"].Args)
	want := []string{"--dry-run", "--limit", "10"}
	if len(got) != len(want) {
		t.Fatalf("args via env: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("args via env: got %v want %v", got, want)
		}
	}
}

func TestYAMLArgumentsStillListForm(t *testing.T) {
	p := writeYAML(t, "processes:\n  api:\n    path: /bin/api\n    type: one_shot\n    arguments: [\"--flag with space\", \"--other\"]\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	got := []string(cfg.Processes["api"].Args)
	if len(got) != 2 || got[0] != "--flag with space" || got[1] != "--other" {
		t.Fatalf("yaml list args: got %v", got)
	}
}

func TestInvalidExit(t *testing.T) {
	p := writeYAML(t, `
processes:
  migrate:
    path: /bin/migrate
    type: one_shot
  api:
    path: /bin/api
    type: one_shot
    depends_on:
      migrate:
        exit: sucess
`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for invalid exit value, got nil")
	}
}

func TestDependsOnServiceRejected(t *testing.T) {
	p := writeYAML(t, `
processes:
  db:
    path: /bin/postgres
    type: service
  seed:
    path: /bin/seed
    type: one_shot
    depends_on:
      db:
        exit: any
`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for depends_on a service, got nil")
	}
}

func TestDependsOnCronRejected(t *testing.T) {
	p := writeYAML(t, `
processes:
  backup:
    path: /bin/backup
    type: cron
    cron: "0 3 * * *"
  report:
    path: /bin/report
    type: one_shot
    depends_on:
      backup:
        exit: any
`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for depends_on a cron, got nil")
	}
}

func TestServiceDependsOnOneShot(t *testing.T) {
	p := writeYAML(t, `
processes:
  migrate:
    path: /bin/migrate
    type: one_shot
  api:
    path: /bin/api
    type: service
    depends_on:
      migrate:
        exit: success
`)
	if _, err := Load(p); err != nil {
		t.Fatalf("service depends_on one_shot should load: %v", err)
	}
}
