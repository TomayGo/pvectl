package plan

import (
	gocontext "context"
	"os"
	"strings"
	"testing"

	plog "github.com/TomayGo/pvectl/internal/log"
	"github.com/TomayGo/pvectl/internal/state"
)

func lockEnv(t *testing.T, target string) *Env {
	t.Helper()
	return &Env{Target: target, StateDir: t.TempDir(), Log: plog.Discard()}
}

func lockEnvBare(target string) *Env {
	return &Env{Target: target, StateDir: os.TempDir(), Log: plog.Discard()}
}

func bgCtx() gocontext.Context { return gocontext.Background() }

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

// The checks that only make sense when the host disappears must not run for
// maint enter, which leaves guests running and the machine powered. Reporting
// "will stop VM 103 -- the Wi-Fi drops" for an operation that stops nothing
// trains people to ignore the warning.
func TestPreflightGatesShutdownOnlyChecks(t *testing.T) {
	shutdownOnly := map[string]bool{"self-host": true, "ssh-path": true, "guests": true}
	for _, forShutdown := range []bool{false, true} {
		names := plannedCheckNames(forShutdown)
		for c := range shutdownOnly {
			if got := names[c]; got != forShutdown {
				t.Errorf("forShutdown=%v: check %q present=%v, want %v", forShutdown, c, got, forShutdown)
			}
		}
		// The ones that apply to both must always be there: maint enter really
		// does drain and evacuate.
		for _, always := range []string{"cluster-lock", "quorum", "nodes-online", "ceph-health", "k8s-drain", "ha-capacity"} {
			if !names[always] {
				t.Errorf("forShutdown=%v: check %q is missing", forShutdown, always)
			}
		}
	}
}

// plannedCheckNames reports which checks Preflight would run, by skipping them
// all: a skipped check still appears in the report, an ungated one does not.
func plannedCheckNames(forShutdown bool) map[string]bool {
	e := lockEnvBare("pve-1")
	e.Opts.SkipChecks = append([]string(nil), CheckNames...)
	r := Preflight(bgCtx(), e, forShutdown)
	out := map[string]bool{}
	for _, c := range r.Results {
		out[c.Name] = true
	}
	return out
}
