package plan

import (
	"testing"

	"github.com/TomayGo/pvectl/internal/state"
)

func haRunner(t *testing.T, command string) *Runner {
	t.Helper()
	e := &Env{Cfg: minimalConfig()}
	steps, err := StepsFor(command, e)
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{Env: e, Steps: steps, Name: command}
	e.State = state.NewState(t.TempDir(), command, "pve-1", "pve-2", r.StepNames())
	return r
}

// A maintenance request is in force long before its wait finishes. If the
// wait times out (or the process dies in it), abort still has to end the
// maintenance, or the node is left out of HA placement indefinitely.
func TestRollbackUndoesStartedHAEvacuation(t *testing.T) {
	for _, status := range []state.StepStatus{state.StepFailed, state.StepRunning} {
		r := haRunner(t, CmdMaintEnter)
		st := r.Env.State
		for _, s := range []string{"lock", "preflight", "silence", "ceph-flags"} {
			st.Set(s, state.StepDone, nil)
		}
		st.Set(haStep, status, nil)
		got := r.undoable()
		want := []string{"lock", "preflight", "silence", "ceph-flags", haStep}
		if len(got) != len(want) {
			t.Fatalf("%s: undoable %v, want %v", status, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s: undoable %v, want %v", status, got, want)
			}
		}
	}
}

// Only steps that opt in are undone when unfinished: a drain that failed
// midway is retried by resume, not reversed.
func TestRollbackSkipsOtherUnfinishedSteps(t *testing.T) {
	r := haRunner(t, CmdMaintEnter)
	st := r.Env.State
	st.Set("lock", state.StepDone, nil)
	st.Set(haStep, state.StepDone, nil)
	st.Set("k8s-drain", state.StepFailed, nil)
	got := r.undoable()
	if len(got) != 2 || got[1] != haStep {
		t.Fatalf("undoable %v", got)
	}
}

// Ownership of the HA maintenance is what restore and abort act on, so it has
// to survive state.json and the carry-over into a fresh attempt.
func TestHAOwnershipSurvivesStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	old := state.NewState(dir, CmdShutdown, "pve-1", "pve-2", []string{haStep})
	old.SetData(haStep, haOwnedKey, true)
	if err := old.Save(); err != nil {
		t.Fatal(err)
	}
	loaded, err := state.LoadState(dir)
	if err != nil || loaded == nil {
		t.Fatalf("load: %v", err)
	}
	e := &Env{State: loaded}
	if !haOwned(e) {
		t.Fatal("ownership lost in the JSON round trip")
	}

	fresh := state.NewState(dir, CmdReboot, "pve-1", "pve-2", []string{haStep})
	carryRollbackData(loaded, fresh)
	if !haOwned(&Env{State: fresh}) {
		t.Fatal("ownership not carried into a fresh attempt")
	}

	fresh.SetData(haStep, haOwnedKey, false)
	if haOwned(&Env{State: fresh}) {
		t.Fatal("ended maintenance still reported as owned")
	}
}

func TestPlansEndHAMaintenance(t *testing.T) {
	// Every plan that puts the node into maintenance either keeps the window
	// open (shutdown, maint enter) or ends with restore, which lifts it.
	for _, c := range []string{CmdReboot, CmdUpgrade} {
		r := haRunner(t, c)
		if r.step(haStep) == nil || r.step("restore") == nil {
			t.Fatalf("%s must both enter and leave HA maintenance", c)
		}
	}
	for _, c := range []string{CmdStart, CmdMaintExit} {
		r := haRunner(t, c)
		if r.step("restore") == nil {
			t.Fatalf("%s must restore", c)
		}
	}
	if s := haRunner(t, CmdShutdown).step(haStep); s == nil || s.Undo == nil || !s.UndoIfStarted {
		t.Fatal("ha-evacuate must be undoable, also when unfinished")
	}
}
