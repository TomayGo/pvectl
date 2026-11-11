package remote

import (
	"context"
	"io"
	"os/exec"
)

// Runner starts a process on the node pvectl runs on. Everything pvectl
// executes rather than calls goes through one -- ssh to a peer, and the local
// ha-manager -- so tests can stand a fake in for the cluster.
type Runner interface {
	// Run runs the command to completion and returns its stdout and stderr
	// combined, as exec.Cmd.CombinedOutput does.
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
	// Stream runs the command to completion with its stdout and stderr
	// connected to the given writers. A command that could not be started at
	// all fails with a *StartError.
	Stream(ctx context.Context, stdout, stderr io.Writer, name string, args ...string) error
}

// StartError reports that a command never ran: the binary is missing, or the
// process could not be created. It is told apart from a command that ran and
// failed because the two mean different things to the operator.
type StartError struct{ Err error }

func (e *StartError) Error() string { return e.Err.Error() }
func (e *StartError) Unwrap() error { return e.Err }

// Exec is the Runner that really runs things, through os/exec.
type Exec struct{}

func (Exec) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func (Exec) Stream(ctx context.Context, stdout, stderr io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Start(); err != nil {
		return &StartError{Err: err}
	}
	return cmd.Wait()
}

// SSHArgs is the argument list for running script on h: Args(h), then the
// script as the single remote command.
func SSHArgs(h Host, script string) []string {
	return append(Args(h), script)
}

// LineWriter splits what is written to it into lines and hands each one,
// without its line ending, to Line. Call Flush once the stream has ended to
// deliver a last line that had no newline.
type LineWriter struct {
	Line func(string)
	buf  []byte
}

func (w *LineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := indexByte(w.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		w.Line(dropCR(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
}

// Flush delivers whatever is left after the last newline.
func (w *LineWriter) Flush() {
	if len(w.buf) > 0 {
		w.Line(dropCR(w.buf))
		w.buf = nil
	}
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

// dropCR matches bufio.ScanLines, which is what read these streams before.
func dropCR(b []byte) string {
	if n := len(b); n > 0 && b[n-1] == '\r' {
		return string(b[:n-1])
	}
	return string(b)
}
