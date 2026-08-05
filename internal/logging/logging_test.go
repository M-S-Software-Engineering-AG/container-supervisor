package logging

import (
	"bytes"
	"strings"
	"testing"
)

func TestTagAlignment(t *testing.T) {
	var buf bytes.Buffer
	old := out
	out = &buf
	defer func() { out = old }()

	f := NewFactory([]string{"a", "abcd"})
	wa, wb := f.ProcessWriter("a", false), f.ProcessWriter("abcd", false)
	wa.Write([]byte("x\n"))
	wb.Write([]byte("y\n"))
	lm := Supervisor()
	lm.Info().Msg("z")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 lines, got %d: %q", len(lines), buf.String())
	}
	want := []string{"[a   ] ", "[abcd] ", "[supervisor] "}
	for i, w := range want {
		if !strings.HasPrefix(lines[i], w) {
			t.Errorf("line %d: got %q, want prefix %q", i, lines[i], w)
		}
	}
}

func TestHideLabel(t *testing.T) {
	for _, tc := range []struct{ format, line, want string }{
		{"console", "plain", "plain\n"},
		{"json", "plain", `{"message":"plain"}` + "\n"},
		{"json", `{"lvl":"info"}`, `{"lvl":"info"}` + "\n"},
	} {
		var buf bytes.Buffer
		oldOut, oldFmt := out, format
		out, format = &buf, tc.format
		w := NewFactory([]string{"api"}).ProcessWriter("api", true)
		w.Write([]byte(tc.line + "\n"))
		w.Close()
		out, format = oldOut, oldFmt
		if got := buf.String(); got != tc.want {
			t.Errorf("%s %q: got %q, want %q", tc.format, tc.line, got, tc.want)
		}
	}
}

func TestSupervisorJSON(t *testing.T) {
	var buf bytes.Buffer
	oldOut, oldFmt := out, format
	out, format = &buf, "json"
	defer func() { out, format = oldOut, oldFmt }()

	lm := Supervisor()
	lm.Info().Str("process_name", "api").Msg("process registered")

	line := strings.TrimSpace(buf.String())
	if !strings.Contains(line, `"process":"supervisor"`) || !strings.Contains(line, `"process_name":"api"`) {
		t.Errorf("supervisor json line: got %q", line)
	}
}

func TestProcessWriterJSON(t *testing.T) {
	var buf bytes.Buffer
	oldOut, oldFmt := out, format
	out, format = &buf, "json"
	defer func() { out, format = oldOut, oldFmt }()

	w := NewFactory([]string{"api"}).ProcessWriter("api", false)
	w.Write([]byte(`{"lvl":"info","msg":"up"}` + "\n"))
	w.Write([]byte("plain text\n"))
	w.Write([]byte("no newline yet"))
	w.Close()

	got := strings.Split(strings.TrimSpace(buf.String()), "\n")
	want := []string{
		`{"process":"api","output":{"lvl":"info","msg":"up"}}`,
		`{"process":"api","message":"plain text"}`,
		`{"process":"api","message":"no newline yet"}`,
	}
	for i, wline := range want {
		if i >= len(got) || got[i] != wline {
			t.Errorf("line %d: got %q, want %q", i, got[i:], wline)
		}
	}
}

func TestProcessWriterConsole(t *testing.T) {
	var buf bytes.Buffer
	oldOut, oldFmt := out, format
	out, format = &buf, "console"
	defer func() { out, format = oldOut, oldFmt }()

	w := NewFactory([]string{"api", "worker"}).ProcessWriter("api", false)
	w.Write([]byte(`{"lvl":"info"}` + "\n"))
	w.Close()

	if got := buf.String(); got != "[api   ] {\"lvl\":\"info\"}\n" {
		t.Errorf("console line: got %q", got)
	}
}
