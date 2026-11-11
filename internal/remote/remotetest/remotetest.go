// Package remotetest provides a remote.Runner that runs nothing, for tests of
// the code that would otherwise ssh to a peer or call ha-manager.
package remotetest

import (
	"context"
	"io"
	"strings"
	"sync"
)

// Call is one command a test subject tried to run.
type Call struct {
	Name string
	Args []string
	// Stream is true for a Runner.Stream call, false for Runner.Run.
	Stream bool
}

// Line is the command as a single string, for matching in assertions.
func (c Call) Line() string { return strings.Join(append([]string{c.Name}, c.Args...), " ") }

// Script is the last argument: for an ssh call, the remote command.
func (c Call) Script() string {
	if len(c.Args) == 0 {
		return ""
	}
	return c.Args[len(c.Args)-1]
}

// Result is what a faked command printed and how it ended.
type Result struct {
	Stdout string
	Stderr string
	Err    error
}

// Fake records every call and answers it with Answer. A nil Answer means every
// command succeeds silently.
type Fake struct {
	Answer func(Call) Result

	mu    sync.Mutex
	calls []Call
}

// Calls returns what has been run so far, in order.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

func (f *Fake) answer(c Call) Result {
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
	if f.Answer == nil {
		return Result{}
	}
	return f.Answer(c)
}

func (f *Fake) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	r := f.answer(Call{Name: name, Args: args})
	return []byte(r.Stdout + r.Stderr), r.Err
}

func (f *Fake) Stream(ctx context.Context, stdout, stderr io.Writer, name string, args ...string) error {
	r := f.answer(Call{Name: name, Args: args, Stream: true})
	_, _ = io.WriteString(stdout, r.Stdout)
	_, _ = io.WriteString(stderr, r.Stderr)
	return r.Err
}
