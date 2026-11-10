package plan

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	plog "github.com/TomayGo/pvectl/internal/log"
	"github.com/TomayGo/pvectl/internal/state"
)

// unwritableRunner builds a runner whose state.json cannot be written: its
// directory does not exist, which is what Save sees when /etc/pve has gone
// read-only or away. It returns the buffer the run reports into.
func unwritableRunner(t *testing.T, steps []Step) (*Runner, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	e := &Env{Target: "pve-1", Log: plog.To(&out)}
	r := &Runner{Env: e, Steps: steps, Name: CmdReboot}
	e.State = state.NewState(filepath.Join(t.TempDir(), "gone"), CmdReboot, "pve-1", "pve-2", r.StepNames())
	return r, &out
}

// A failed step is what `resume` and `abort` work from. If recording it fails
// too, the run still stops with the step's own error -- but it has to say that
// state.json does not have it.
func TestRunWarnsWhenTheFailureCannotBeSaved(t *testing.T) {
	boom := errors.New("drain refused")
	r, out := unwritableRunner(t, []Step{{
		Name: "k8s-drain",
		Do:   func(ctx context.Context, e *Env) error { return boom },
	}})
	err := r.Run(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("Run must still return the step's error, got %v", err)
	}
	if got := r.Env.State.Step("k8s-drain").Status; got != state.StepFailed {
		t.Errorf("step is %s in memory, want failed", got)
	}
	if !strings.Contains(out.String(), "  ! could not persist the failure of k8s-drain: ") {
		t.Errorf("no warning about the unsaved failure:\n%s", out)
	}
}

// With every step already done there is nothing left to run, only the final
// "finished" to write. Failing to write it is not a failed run.
func TestRunWarnsWhenTheFinishedRunCannotBeSaved(t *testing.T) {
	r, out := unwritableRunner(t, []Step{{Name: "restore"}, {Name: "verify"}})
	r.Env.State.Set("restore", state.StepDone, nil)
	r.Env.State.Set("verify", state.StepSkipped, nil)
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("an unsaved finish must not fail the run: %v", err)
	}
	if !r.Env.State.Finished {
		t.Error("the run must be finished in memory")
	}
	if !strings.Contains(out.String(), "  ! could not persist the finished run: ") {
		t.Errorf("no warning about the unsaved finish:\n%s", out)
	}
}

// Rollback keeps going whatever happens; an undo it could not record is still
// undone, and said to be unrecorded.
func TestRollbackWarnsWhenAnUndoCannotBeSaved(t *testing.T) {
	undone := 0
	r, out := unwritableRunner(t, []Step{
		{Name: "silence", Undo: func(ctx context.Context, e *Env) error { undone++; return nil }},
		{Name: "ceph-flags", Undo: func(ctx context.Context, e *Env) error { undone++; return nil }},
	})
	r.Env.State.Set("silence", state.StepDone, nil)
	r.Env.State.Set("ceph-flags", state.StepDone, nil)
	if err := r.Rollback(context.Background()); err != nil {
		t.Fatalf("an unsaved undo must not fail the rollback: %v", err)
	}
	if undone != 2 {
		t.Errorf("%d step(s) undone, want 2", undone)
	}
	for _, name := range []string{"silence", "ceph-flags"} {
		if got := r.Env.State.Step(name).Status; got != state.StepUndone {
			t.Errorf("%s is %s in memory, want undone", name, got)
		}
		if !strings.Contains(out.String(), "  ! could not persist the rollback of "+name+": ") {
			t.Errorf("no warning about the unsaved undo of %s:\n%s", name, out)
		}
	}
}

// The other side of the change: when the saves work, nothing new is printed.
func TestRunAndRollbackStayQuietWhenSavesWork(t *testing.T) {
	var out bytes.Buffer
	e := &Env{Target: "pve-1", Log: plog.To(&out)}
	r := &Runner{Env: e, Name: CmdReboot, Steps: []Step{{
		Name: "silence",
		Do:   func(ctx context.Context, e *Env) error { return nil },
		Undo: func(ctx context.Context, e *Env) error { return nil },
	}}}
	dir := t.TempDir()
	e.State = state.NewState(dir, CmdReboot, "pve-1", "pve-2", r.StepNames())
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "could not persist") {
		t.Errorf("a working save produced a warning:\n%s", out.String())
	}
	saved, err := state.LoadState(dir)
	if err != nil || saved == nil {
		t.Fatalf("load: %v", err)
	}
	if !saved.Finished || saved.Step("silence").Status != state.StepUndone {
		t.Errorf("state.json has finished=%v silence=%s", saved.Finished, saved.Step("silence").Status)
	}
}
