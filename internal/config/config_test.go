package config

import (
	"os"
	"path/filepath"
	"testing"
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
	want := map[string]bool{"a": true, "b": false, "c": true} // global default, overridden off, set on
	for name, w := range want {
		if got := cfg.Processes[name].HidesLabel(); got != w {
			t.Errorf("%s.HidesLabel(): got %v want %v", name, got, w)
		}
	}
	// unset with no global default, and the nil-safe path for a hand-built Process
	if cfg2, err := Load(writeYAML(t, "processes:\n  a:\n    path: /bin/a\n    type: service\n")); err != nil {
		t.Fatal(err)
	} else if cfg2.Processes["a"].HidesLabel() {
		t.Error("a.HidesLabel(): got true want false without hide_labels")
	}
	if (Process{}).HidesLabel() {
		t.Error("zero Process.HidesLabel(): got true want false")
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

func TestValidateCron(t *testing.T) {
	valid := []string{"0 3 * * *", "*/15 * * * *", "0 0,12 1-15 */2 1-5", "59 23 31 12 6"}
	for _, expr := range valid {
		if err := validateCron(expr); err != nil {
			t.Errorf("validateCron(%q): unexpected error: %v", expr, err)
		}
	}
	invalid := []string{"", "0 3 * *", "0 3 * * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * * 13 *", "* * * * 7", "*/0 * * * *", "1-x * * * *", "a * * * *"}
	for _, expr := range invalid {
		if err := validateCron(expr); err == nil {
			t.Errorf("validateCron(%q): expected error, got nil", expr)
		}
	}
}

func TestCronInvalidSchedule(t *testing.T) {
	p := writeYAML(t, "processes:\n  backup:\n    path: /bin/backup\n    type: cron\n    cron: \"nonsense\"\n")
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for invalid cron schedule, got nil")
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

// A service never exits, so gating on one would park the dependent forever.
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

// The inverse is the point of the feature: prep one_shot, then the service.
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
