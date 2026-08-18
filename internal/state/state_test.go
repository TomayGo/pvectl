package state

import (
	"errors"
	"testing"
)

// The lock is what makes "two nodes down at once" structurally impossible, so
// a second Acquire must fail rather than overwrite.
func TestAcquireIsExclusive(t *testing.T) {
	dir := t.TempDir()
	l1, err := Acquire(dir, "pve-1", "pve-3", "shutdown")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(dir, "pve-2", "pve-2", "shutdown"); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Acquire returned %v, want ErrLocked", err)
	}
	held, err := Peek(dir)
	if err != nil || held == nil {
		t.Fatalf("Peek returned %v, %v", held, err)
	}
	if held.Target != "pve-3" || held.Holder != "pve-1" {
		t.Fatalf("lock records %+v", held)
	}
	if err := l1.Release(); err != nil {
		t.Fatal(err)
	}
	if err := l1.Release(); err != nil {
		t.Fatalf("double release should be a no-op, got %v", err)
	}
	if held, _ := Peek(dir); held != nil {
		t.Fatal("lock still present after release")
	}
}

// A maintenance window spans several invocations: `shutdown node X` then
// `start node X`. The second must adopt the lock, not fail on it.
func TestAcquireOrAdopt(t *testing.T) {
	dir := t.TempDir()
	if _, err := Acquire(dir, "pve-1", "pve-3", "shutdown"); err != nil {
		t.Fatal(err)
	}
	l, adopted, err := AcquireOrAdopt(dir, "pve-1", "pve-3", "start")
	if err != nil || !adopted || l == nil {
		t.Fatalf("adopting the same target returned %v, adopted=%v", err, adopted)
	}
	if _, _, err := AcquireOrAdopt(dir, "pve-1", "pve-2", "shutdown"); !errors.Is(err, ErrLocked) {
		t.Fatalf("a different target must still conflict, got %v", err)
	}
}

func TestStateRoundTripAndResumePoint(t *testing.T) {
	dir := t.TempDir()
	s := NewState(dir, "shutdown", "pve-3", "pve-1", []string{"lock", "preflight", "ceph-flags", "host-shutdown"})
	s.Set("lock", StepDone, nil)
	s.Set("preflight", StepDone, nil)
	s.SetData("ceph-flags", "set_by_us", []string{"noout"})
	s.Set("ceph-flags", StepFailed, errors.New("boom"))
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	got, err := LoadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.FirstIncomplete() != "ceph-flags" {
		t.Fatalf("resume point is %q, want ceph-flags", got.FirstIncomplete())
	}
	done := got.CompletedSteps()
	if len(done) != 2 || done[0] != "lock" || done[1] != "preflight" {
		t.Fatalf("completed steps are %v", done)
	}
	if got.Step("ceph-flags").Error != "boom" {
		t.Fatalf("error not preserved: %q", got.Step("ceph-flags").Error)
	}
	if got.Step("ceph-flags").Data["set_by_us"] == nil {
		t.Fatal("rollback data not preserved")
	}
}

func TestDryRunStateWritesNothing(t *testing.T) {
	dir := t.TempDir()
	s := NewState(dir, "shutdown", "pve-3", "pve-1", []string{"lock"})
	s.SetDryRun(true)
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatal("a dry run must not write state.json")
	}
}

func TestRollingRemaining(t *testing.T) {
	r := &Rolling{Order: []string{"pve-3", "pve-2", "pve-1"}, Done: []string{"pve-3"}}
	got := r.Remaining()
	if len(got) != 2 || got[0] != "pve-2" || got[1] != "pve-1" {
		t.Fatalf("remaining is %v", got)
	}
}
