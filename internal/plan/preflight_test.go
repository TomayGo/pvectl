package plan

import (
	"strings"
	"testing"

	plog "github.com/tamago/pvectl/internal/log"
	"github.com/tamago/pvectl/internal/state"
)

func lockEnv(t *testing.T, target string) *Env {
	t.Helper()
	return &Env{Target: target, StateDir: t.TempDir(), Log: plog.Discard()}
}

func statusOf(r *Report, name string) (Status, string) {
	for _, c := range r.Results {
		if c.Name == name {
			return c.Status, c.Detail
		}
	}
	return "", "missing"
}

func TestCheckLockFreeIsPass(t *testing.T) {
	e := lockEnv(t, "pve-3")
	r := &Report{}
	checkLock(e, r)
	if st, d := statusOf(r, "cluster-lock"); st != Pass {
		t.Fatalf("free lock reported %s: %s", st, d)
	}
}

// preflight is step 2 of every plan and step 1 is what takes the lock, so the
// check must not flag the run's own lock. This shipped broken: the first real
// `pvectl reboot node pve-3` failed its own preflight on the lock it had just
// acquired.
func TestCheckLockIgnoresOurOwnLock(t *testing.T) {
	e := lockEnv(t, "pve-3")
	lk, err := state.Acquire(e.StateDir, "pve-1", "pve-3", "reboot")
	if err != nil {
		t.Fatal(err)
	}
	e.Lock = lk
	r := &Report{}
	checkLock(e, r)
	st, d := statusOf(r, "cluster-lock")
	if st != Pass {
		t.Fatalf("own lock reported %s: %s", st, d)
	}
	if !strings.Contains(d, "this run") {
		t.Fatalf("detail should say the lock is ours, got %q", d)
	}
}

// A lock held for a different node still blocks, whether or not this process
// holds one -- that is the whole point of the check.
func TestCheckLockBlocksForeignLock(t *testing.T) {
	e := lockEnv(t, "pve-3")
	if _, err := state.Acquire(e.StateDir, "pve-2", "pve-2", "shutdown"); err != nil {
		t.Fatal(err)
	}
	r := &Report{}
	checkLock(e, r)
	if st, d := statusOf(r, "cluster-lock"); st != Fail {
		t.Fatalf("foreign lock reported %s: %s", st, d)
	}
	if !r.Failed() {
		t.Fatal("report should be failing")
	}
}

// Standalone `pvectl preflight` holds no lock, so any lock on disk is foreign.
func TestCheckLockStandaloneSeesAnyLock(t *testing.T) {
	e := lockEnv(t, "pve-3")
	if _, err := state.Acquire(e.StateDir, "pve-1", "pve-3", "reboot"); err != nil {
		t.Fatal(err)
	}
	r := &Report{}
	checkLock(e, r) // e.Lock is nil: we are not the holder
	if st, _ := statusOf(r, "cluster-lock"); st != Fail {
		t.Fatalf("standalone preflight should report a held lock, got %s", st)
	}
}
