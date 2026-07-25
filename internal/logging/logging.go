// Package logging separates two kinds of output. The supervisor's own
// structured events come from Supervisor(), tagged "[supervisor]" (console)
// or process=supervisor (json). A child process's raw stdout/stderr is piped
// through a Factory.ProcessWriter, which tags each line with the process name:
// a bracketed, width-aligned prefix in console, or a "process" envelope in json.
package logging

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/mattn/go-isatty"
	"github.com/rs/zerolog"
)

var (
	out    io.Writer = os.Stderr
	mu     sync.Mutex
	format = "console"
)

// Configure sets the output format for loggers created afterwards. Anything
// other than "json" produces human-readable console output.
func Configure(outputFormat string) {
	format = outputFormat
}

type prefixWriter struct {
	prefix []byte
}

func (pw prefixWriter) Write(p []byte) (int, error) {
	// ponytail: one global lock keeps concurrent process lines from
	// interleaving; split per-writer only if logging ever bottlenecks.
	mu.Lock()
	defer mu.Unlock()
	buf := append(append(make([]byte, 0, len(pw.prefix)+len(p)), pw.prefix...), p...)
	if _, err := out.Write(buf); err != nil {
		return 0, err
	}
	return len(p), nil
}

// noColor disables ANSI unless the sink is a terminal.
func noColor() bool {
	f, ok := out.(*os.File)
	return !(ok && isatty.IsTerminal(f.Fd()))
}

// Supervisor returns the logger for the supervisor's own structured events.
// It is tagged "[supervisor]" in console format or process=supervisor in
// json; name the subject process with a field, e.g. .Str("process_name", n).
func Supervisor() zerolog.Logger {
	if format == "json" {
		return zerolog.New(prefixWriter{}).With().Timestamp().Str("process", "supervisor").Logger()
	}
	dst := zerolog.ConsoleWriter{Out: prefixWriter{[]byte("[supervisor] ")}, TimeFormat: "15:04:05", NoColor: noColor()}
	return zerolog.New(dst).With().Timestamp().Logger()
}

// Factory tags child-process output lines, padding console name tags to the
// width of the longest process name, e.g. "[a   ]" alongside "[abcd]".
type Factory struct {
	width int
}

func NewFactory(names []string) *Factory {
	w := 0
	for _, n := range names {
		if len(n) > w {
			w = len(n)
		}
	}
	return &Factory{width: w}
}

// ProcessWriter returns a writer for a child process's output stream. It splits
// input into lines and tags each with the process name: a console prefix, or a
// json envelope. In json format a line that is itself a json object/array is
// embedded under "output" (json in json); anything else becomes "message".
// With hideLabel the name is left off entirely: the line starts at column 0 in
// console; in json a line that is already json passes through untouched, and
// anything else becomes a bare {"message": ...}.
// Call Close to flush a trailing line with no newline.
func (f *Factory) ProcessWriter(name string, hideLabel bool) io.WriteCloser {
	if hideLabel {
		return &procWriter{}
	}
	return &procWriter{
		tag:   fmt.Sprintf("[%-*s] ", f.width, name),
		label: name,
	}
}

type procWriter struct {
	tag   string
	label string
	mu    sync.Mutex
	buf   []byte
}

func (w *procWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.emit(w.buf[:i])
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

func (w *procWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.emit(w.buf)
		w.buf = nil
	}
	return nil
}

func (w *procWriter) emit(line []byte) {
	line = bytes.TrimRight(line, "\r")
	var rendered []byte
	if format == "json" {
		t := bytes.TrimSpace(line)
		if structuredJSON(t) && w.label == "" {
			// No label to attach, so the envelope would add nothing: pass the
			// child's own json through as the line.
			rendered = append(append(make([]byte, 0, len(t)+1), t...), '\n')
		} else {
			env := struct {
				Process string          `json:"process,omitempty"`
				Output  json.RawMessage `json:"output,omitempty"`
				Message string          `json:"message,omitempty"`
			}{Process: w.label}
			if structuredJSON(t) {
				env.Output = json.RawMessage(t)
			} else {
				env.Message = string(line)
			}
			rendered, _ = json.Marshal(env)
			rendered = append(rendered, '\n')
		}
	} else {
		rendered = append(append([]byte(w.tag), line...), '\n')
	}
	mu.Lock()
	out.Write(rendered)
	mu.Unlock()
}

// structuredJSON reports whether line is a JSON object or array.
func structuredJSON(line []byte) bool {
	if len(line) == 0 || (line[0] != '{' && line[0] != '[') {
		return false
	}
	return json.Valid(line)
}
