package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jehuda-ruzinski/container-supervisor/internal/config"
	"github.com/rs/zerolog"
)

func cfg(procs map[string]config.Process) *config.Config {
	return &config.Config{Processes: procs}
}

func dep(name, exit string) map[string]config.Dependency {
	return map[string]config.Dependency{name: {Exit: exit}}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     *config.Config
		wantErr bool
	}{
		{"no deps", cfg(map[string]config.Process{"a": {}, "b": {}}), false},
		{"linear chain", cfg(map[string]config.Process{
			"a": {}, "b": {DependsOn: dep("a", "success")}, "c": {DependsOn: dep("b", "success")},
		}), false},
		{"unknown dep", cfg(map[string]config.Process{
			"a": {DependsOn: dep("ghost", "success")},
		}), true},
		{"self cycle", cfg(map[string]config.Process{
			"a": {DependsOn: dep("a", "success")},
		}), true},
		{"two-node cycle", cfg(map[string]config.Process{
			"a": {DependsOn: dep("b", "success")}, "b": {DependsOn: dep("a", "success")},
		}), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.cfg)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// script writes an executable shell script that appends label to orderFile,
// then exits with the given code, and returns its path.
func script(t *testing.T, dir, label, orderFile string, exitCode int) string {
	t.Helper()
	path := filepath.Join(dir, label+".sh")
	body := "#!/bin/sh\necho " + label + " >> " + orderFile + "\nexit " + itoa(exitCode) + "\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	return string(rune('0' + n))
}

// scriptBody writes an executable script with the given shell body.
func scriptBody(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name+".sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunOrdersByDependency(t *testing.T) {
	dir := t.TempDir()
	order := filepath.Join(dir, "order")

	c := cfg(map[string]config.Process{
		"first":  {Path: script(t, dir, "first", order, 0), Type: config.TypeOneShot},
		"second": {Path: script(t, dir, "second", order, 0), Type: config.TypeOneShot, DependsOn: dep("first", "success")},
	})

	if err := Run(context.Background(), c, zerolog.Nop()); err != nil {
		t.Fatal(err)
	}

	got, _ := os.ReadFile(order)
	if string(got) != "first\nsecond\n" {
		t.Fatalf("order = %q, want first then second", got)
	}
}

func TestRunPassesArgs(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "args")

	c := cfg(map[string]config.Process{
		"p": {Path: scriptBody(t, dir, "p", `echo "$@" > `+out), Args: []string{"--dry-run", "--limit", "10"}, Type: config.TypeOneShot},
	})

	if err := Run(context.Background(), c, zerolog.Nop()); err != nil {
		t.Fatal(err)
	}

	got, _ := os.ReadFile(out)
	if strings.TrimSpace(string(got)) != "--dry-run --limit 10" {
		t.Fatalf("args = %q, want %q", got, "--dry-run --limit 10")
	}
}

func TestRunPassesEnv(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "env")

	c := cfg(map[string]config.Process{
		"p": {Path: scriptBody(t, dir, "p", `echo "$GREETING" > ` + out), Env: map[string]string{"GREETING": "hello"}, Type: config.TypeOneShot},
	})

	if err := Run(context.Background(), c, zerolog.Nop()); err != nil {
		t.Fatal(err)
	}

	got, _ := os.ReadFile(out)
	if strings.TrimSpace(string(got)) != "hello" {
		t.Fatalf("env = %q, want %q", got, "hello")
	}
}

func TestRunSkipsWhenConditionUnmet(t *testing.T) {
	dir := t.TempDir()
	order := filepath.Join(dir, "order")

	c := cfg(map[string]config.Process{
		"up":   {Path: script(t, dir, "up", order, 1), OnFailure: "continue"},             // exits non-zero, tolerated
		"down": {Path: script(t, dir, "down", order, 0), DependsOn: dep("up", "success")}, // needs success
	})

	if err := Run(context.Background(), c, zerolog.Nop()); err != nil {
		t.Fatal(err)
	}

	got, _ := os.ReadFile(order)
	if string(got) != "up\n" {
		t.Fatalf("order = %q, want up only (down skipped)", got)
	}
}

// TestRunDisabledReportsFailure checks a disabled process never runs, does not
// abort the run despite the default on_failure, and reads as a failure to
// dependents.
func TestRunDisabledReportsFailure(t *testing.T) {
	dir := t.TempDir()
	order := filepath.Join(dir, "order")
	off := config.Bool(false)

	c := cfg(map[string]config.Process{
		"off":       {Path: script(t, dir, "off", order, 0), Type: config.TypeOneShot, Enabled: &off},
		"needsOK":   {Path: script(t, dir, "needsOK", order, 0), Type: config.TypeOneShot, DependsOn: dep("off", "success")},
		"needsFail": {Path: script(t, dir, "needsFail", order, 0), Type: config.TypeOneShot, DependsOn: dep("off", "failure")},
	})

	if err := Run(context.Background(), c, zerolog.Nop()); err != nil {
		t.Fatalf("Run() = %v, want nil (disabled must not abort)", err)
	}
	if got, _ := os.ReadFile(order); string(got) != "needsFail\n" {
		t.Fatalf("order = %q, want needsFail only", got)
	}
}

func TestRunFailFastCancelsSiblings(t *testing.T) {
	dir := t.TempDir()
	order := filepath.Join(dir, "order")

	c := cfg(map[string]config.Process{
		"crash": {Path: scriptBody(t, dir, "crash", "exit 1")}, // default on_failure=fail
		"slow":  {Path: scriptBody(t, dir, "slow", "sleep 5\necho slow >> "+order)},
	})

	err := Run(context.Background(), c, zerolog.Nop())
	if err == nil {
		t.Fatal("Run() = nil, want error from aborted run")
	}
	if got, _ := os.ReadFile(order); len(got) != 0 {
		t.Fatalf("order = %q, want empty (slow killed by cancel)", got)
	}
}

func TestRunContinueToleratesFailure(t *testing.T) {
	dir := t.TempDir()
	order := filepath.Join(dir, "order")

	c := cfg(map[string]config.Process{
		"flaky": {Path: scriptBody(t, dir, "flaky", "exit 1"), OnFailure: "continue"},
		"other": {Path: scriptBody(t, dir, "other", "echo other >> "+order), Type: config.TypeOneShot},
	})

	if err := Run(context.Background(), c, zerolog.Nop()); err != nil {
		t.Fatalf("Run() = %v, want nil (failure tolerated)", err)
	}
	if got, _ := os.ReadFile(order); string(got) != "other\n" {
		t.Fatalf("order = %q, want other to have run", got)
	}
}

// TestRunServiceRestart checks that a service with on_failure "restart" is
// rerun each time it exits, until the run is cancelled.
func TestRunServiceRestart(t *testing.T) {
	dir := t.TempDir()
	count := filepath.Join(dir, "count")
	body := "printf x >> " + count + "\nsleep 0.05"
	c := cfg(map[string]config.Process{
		"svc": {Path: scriptBody(t, dir, "svc", body), Type: config.TypeService, OnFailure: "restart"},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if err := Run(ctx, c, zerolog.Nop()); err != nil {
		t.Fatalf("Run() = %v, want nil (restart loop ends on cancel)", err)
	}
	if got, _ := os.ReadFile(count); len(got) < 2 {
		t.Fatalf("service ran %d times, want >= 2 (restarted)", len(got))
	}
}

// TestRunServiceExitAborts checks that a service with on_failure "exit" (the
// default) aborts the run when it stops.
func TestRunServiceExitAborts(t *testing.T) {
	dir := t.TempDir()
	c := cfg(map[string]config.Process{
		"svc": {Path: scriptBody(t, dir, "svc", "exit 1"), Type: config.TypeService, OnFailure: "exit"},
	})
	if err := Run(context.Background(), c, zerolog.Nop()); err == nil {
		t.Fatal("Run() = nil, want error (service exit aborts the run)")
	}
}

// TestRunCronOnFailure checks that on_failure applies to cron: a failing cron
// with the default aborts the run, while "continue" tolerates it.
func TestRunCronOnFailure(t *testing.T) {
	dir := t.TempDir()

	abort := cfg(map[string]config.Process{
		"backup": {Path: scriptBody(t, dir, "backup", "exit 1"), Type: config.TypeCron, Cron: "0 3 * * *"},
	})
	if err := Run(context.Background(), abort, zerolog.Nop()); err == nil {
		t.Fatal("Run() = nil, want error (cron failure aborts by default)")
	}

	tolerate := cfg(map[string]config.Process{
		"backup": {Path: scriptBody(t, dir, "backup2", "exit 1"), Type: config.TypeCron, Cron: "0 3 * * *", OnFailure: "continue"},
	})
	if err := Run(context.Background(), tolerate, zerolog.Nop()); err != nil {
		t.Fatalf("Run() = %v, want nil (cron failure tolerated)", err)
	}
}

// TestRunOneShotDependencyOutcomes checks that a one_shot b reacts correctly to
// every outcome of the one_shot a it depends on: it runs when a's exit matches
// the required condition, and is skipped otherwise. a uses on_failure=continue
// so a non-zero exit doesn't abort the run before b's turn.
func TestRunOneShotDependencyOutcomes(t *testing.T) {
	cases := []struct {
		name     string
		aExit    int
		required string
		bRuns    bool
	}{
		{"success meets success", 0, "success", true},
		{"success meets any", 0, "any", true},
		{"success misses failure", 0, "failure", false},
		{"failure meets failure", 1, "failure", true},
		{"failure meets any", 1, "any", true},
		{"failure misses success", 1, "success", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			order := filepath.Join(dir, "order")

			c := cfg(map[string]config.Process{
				"a": {Path: script(t, dir, "a", order, tc.aExit), Type: config.TypeOneShot, OnFailure: "continue"},
				"b": {Path: script(t, dir, "b", order, 0), Type: config.TypeOneShot, DependsOn: dep("a", tc.required)},
			})

			if err := Run(context.Background(), c, zerolog.Nop()); err != nil {
				t.Fatalf("Run() = %v, want nil", err)
			}
			got, _ := os.ReadFile(order)
			if ranB := strings.Contains(string(got), "b"); ranB != tc.bRuns {
				t.Fatalf("b ran = %v, want %v (order=%q)", ranB, tc.bRuns, got)
			}
		})
	}
}

// TestRunOneShotFanIn checks that c waits for both a and b, which have no
// dependencies and so start concurrently. c must run only after both finish.
func TestRunOneShotFanIn(t *testing.T) {
	dir := t.TempDir()
	order := filepath.Join(dir, "order")

	c := cfg(map[string]config.Process{
		"a": {Path: script(t, dir, "a", order, 0), Type: config.TypeOneShot},
		"b": {Path: script(t, dir, "b", order, 0), Type: config.TypeOneShot},
		"c": {Path: script(t, dir, "c", order, 0), Type: config.TypeOneShot,
			DependsOn: map[string]config.Dependency{"a": {Exit: "success"}, "b": {Exit: "success"}}},
	})

	if err := Run(context.Background(), c, zerolog.Nop()); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	// a and b race, so either order before c; c is always last.
	if got, _ := os.ReadFile(order); string(got) != "a\nb\nc\n" && string(got) != "b\na\nc\n" {
		t.Fatalf("order = %q, want a and b (either order) before c", got)
	}
}

func TestRunRetrySucceedsAfterFailures(t *testing.T) {
	dir := t.TempDir()
	count := filepath.Join(dir, "count")

	// Appends a byte per run, fails until the 3rd attempt then exits 0.
	body := "printf x >> " + count + "\n" +
		"[ $(wc -c < " + count + ") -ge 3 ] && exit 0\nexit 1"
	c := cfg(map[string]config.Process{
		"flaky": {Path: scriptBody(t, dir, "flaky", body), Type: config.TypeOneShot, OnFailure: "retry 3"},
	})

	if err := Run(context.Background(), c, zerolog.Nop()); err != nil {
		t.Fatalf("Run() = %v, want nil (succeeds on retry)", err)
	}
	if got, _ := os.ReadFile(count); len(got) != 3 {
		t.Fatalf("ran %d times, want 3", len(got))
	}
}

func TestRunRetryExhaustedAborts(t *testing.T) {
	dir := t.TempDir()
	count := filepath.Join(dir, "count")

	body := "printf x >> " + count + "\nexit 1"
	c := cfg(map[string]config.Process{
		"doomed": {Path: scriptBody(t, dir, "doomed", body), Type: config.TypeOneShot, OnFailure: "retry 2"},
	})

	if err := Run(context.Background(), c, zerolog.Nop()); err == nil {
		t.Fatal("Run() = nil, want error after retries exhausted")
	}
	if got, _ := os.ReadFile(count); len(got) != 3 {
		t.Fatalf("ran %d times, want 3 (1 initial + 2 retries)", len(got))
	}
}
