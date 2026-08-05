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
		"p": {Path: scriptBody(t, dir, "p", `echo "$GREETING" > `+out), Env: map[string]string{"GREETING": "hello"}, Type: config.TypeOneShot},
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
		"up":   {Path: script(t, dir, "up", order, 1), OnFailure: "continue"},
		"down": {Path: script(t, dir, "down", order, 0), DependsOn: dep("up", "success")},
	})

	if err := Run(context.Background(), c, zerolog.Nop()); err != nil {
		t.Fatal(err)
	}

	got, _ := os.ReadFile(order)
	if string(got) != "up\n" {
		t.Fatalf("order = %q, want up only (down skipped)", got)
	}
}

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
		"crash": {Path: scriptBody(t, dir, "crash", "exit 1")},
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

func TestRunServiceExitAborts(t *testing.T) {
	dir := t.TempDir()
	c := cfg(map[string]config.Process{
		"svc": {Path: scriptBody(t, dir, "svc", "exit 1"), Type: config.TypeService, OnFailure: "exit"},
	})
	if err := Run(context.Background(), c, zerolog.Nop()); err == nil {
		t.Fatal("Run() = nil, want error (service exit aborts the run)")
	}
}

func fireEvery(t *testing.T, d time.Duration) {
	t.Helper()
	orig := cronNext
	cronNext = func(config.Schedule, time.Time) time.Time { return time.Now().Add(d) }
	t.Cleanup(func() { cronNext = orig })
}

func TestRunCronOnFailure(t *testing.T) {
	dir := t.TempDir()
	fireEvery(t, 10*time.Millisecond)

	abort := cfg(map[string]config.Process{
		"backup": {Path: scriptBody(t, dir, "backup", "exit 1"), Type: config.TypeCron, Cron: "0 3 * * *"},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := Run(ctx, abort, zerolog.Nop()); err == nil {
		t.Fatal("Run() = nil, want error (cron failure aborts by default)")
	}
	if ctx.Err() != nil {
		t.Error("Run() only returned once the context expired; want a prompt abort")
	}

	tolerate := cfg(map[string]config.Process{
		"backup": {Path: scriptBody(t, dir, "backup2", "exit 1"), Type: config.TypeCron, Cron: "0 3 * * *", OnFailure: "continue"},
	})
	ctx2, cancel2 := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel2()
	if err := Run(ctx2, tolerate, zerolog.Nop()); err != nil {
		t.Fatalf("Run() = %v, want nil (cron failure tolerated)", err)
	}
}

func TestRunCronFiresRepeatedly(t *testing.T) {
	dir := t.TempDir()
	fireEvery(t, 10*time.Millisecond)
	count := filepath.Join(dir, "count")

	c := cfg(map[string]config.Process{
		"backup": {Path: scriptBody(t, dir, "backup", "printf x >> "+count), Type: config.TypeCron, Cron: "0 3 * * *"},
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := Run(ctx, c, zerolog.Nop()); err != nil {
		t.Fatalf("Run() = %v, want nil (cron loop ends on cancel)", err)
	}
	if got, _ := os.ReadFile(count); len(got) < 2 {
		t.Fatalf("cron ran %d times, want >= 2 (fired on each occurrence)", len(got))
	}
}

func TestRunCronSkipsOverrunSlots(t *testing.T) {
	const slot = 50 * time.Millisecond
	orig := cronNext
	cronNext = func(_ config.Schedule, after time.Time) time.Time { return after.Truncate(slot).Add(slot) }
	t.Cleanup(func() { cronNext = orig })

	dir := t.TempDir()
	count := filepath.Join(dir, "count")
	c := cfg(map[string]config.Process{
		"backup": {
			Path: scriptBody(t, dir, "backup", "printf x >> "+count+"\nsleep 0.2"),
			Type: config.TypeCron, Cron: "0 3 * * *",
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := Run(ctx, c, zerolog.Nop()); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}

	got, _ := os.ReadFile(count)
	if len(got) == 0 || len(got) > 8 {
		t.Fatalf("cron ran %d times, want 1-8 (overrun slots skipped, not queued)", len(got))
	}
}

func TestRunCronRunAtStart(t *testing.T) {
	for _, tc := range []struct {
		name     string
		atStart  config.Bool
		wantRuns int
	}{
		{"run_at_start", true, 1},
		{"schedule only", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			count := filepath.Join(dir, "count")
			c := cfg(map[string]config.Process{
				"backup": {
					Path: scriptBody(t, dir, "backup", "printf x >> "+count), Type: config.TypeCron,
					Cron: "0 3 * * *", RunAtStart: tc.atStart,
				},
			})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := Run(ctx, c, zerolog.Nop()); err != nil {
				t.Fatalf("Run() = %v, want nil", err)
			}
			got, _ := os.ReadFile(count)
			if len(got) != tc.wantRuns {
				t.Fatalf("cron ran %d times, want %d", len(got), tc.wantRuns)
			}
		})
	}
}

func TestRunCronCleanShutdownMidRun(t *testing.T) {
	dir := t.TempDir()
	fireEvery(t, 10*time.Millisecond)

	c := cfg(map[string]config.Process{
		"backup": {Path: scriptBody(t, dir, "backup", "sleep 1"), Type: config.TypeCron, Cron: "0 3 * * *"},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := Run(ctx, c, zerolog.Nop()); err != nil {
		t.Fatalf("Run() = %v, want nil (cancellation mid-run is a clean stop)", err)
	}
}

func TestRunCronCancelDuringWait(t *testing.T) {
	dir := t.TempDir()
	c := cfg(map[string]config.Process{
		"backup": {Path: scriptBody(t, dir, "backup", "true"), Type: config.TypeCron, Cron: "0 3 * * *"},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- Run(ctx, c, zerolog.Nop()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not return after cancellation; the schedule wait ignores ctx")
	}
}

func TestRunTickerFiresRepeatedly(t *testing.T) {
	dir := t.TempDir()
	count := filepath.Join(dir, "count")

	c := cfg(map[string]config.Process{
		"poll": {Path: scriptBody(t, dir, "poll", "printf x >> "+count), Type: config.TypeTicker, Ticker: "@every 10ms"},
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := Run(ctx, c, zerolog.Nop()); err != nil {
		t.Fatalf("Run() = %v, want nil (ticker loop ends on cancel)", err)
	}
	if got, _ := os.ReadFile(count); len(got) < 2 {
		t.Fatalf("ticker ran %d times, want >= 2 (fired on each interval)", len(got))
	}
}

func TestRunTickerRunAtStart(t *testing.T) {
	dir := t.TempDir()
	count := filepath.Join(dir, "count")

	c := cfg(map[string]config.Process{
		"poll": {
			Path: scriptBody(t, dir, "poll", "printf x >> "+count),
			Type: config.TypeTicker, Ticker: "@every 1hour", RunAtStart: true,
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := Run(ctx, c, zerolog.Nop()); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	if got, _ := os.ReadFile(count); len(got) != 1 {
		t.Fatalf("ticker ran %d times, want 1 (run_at_start only, interval not yet due)", len(got))
	}
}

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

	if got, _ := os.ReadFile(order); string(got) != "a\nb\nc\n" && string(got) != "b\na\nc\n" {
		t.Fatalf("order = %q, want a and b (either order) before c", got)
	}
}

func TestRunRetrySucceedsAfterFailures(t *testing.T) {
	dir := t.TempDir()
	count := filepath.Join(dir, "count")

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
