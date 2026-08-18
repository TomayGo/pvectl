package plan

import (
	"testing"

	"github.com/tamago/pvectl/internal/config"
)

// State round-trips through JSON, so a []int written by one invocation comes
// back as []any in the next. guest-stop's rollback depends on getting the
// original ids back.
func TestToIntsAfterJSONRoundTrip(t *testing.T) {
	got := toInts([]any{float64(103), float64(105)})
	if len(got) != 2 || got[0] != 103 || got[1] != 105 {
		t.Fatalf("got %v, want [103 105]", got)
	}
	if got := toInts([]int{101}); len(got) != 1 || got[0] != 101 {
		t.Fatalf("native []int not handled: %v", got)
	}
	if toInts(nil) != nil {
		t.Fatal("nil should stay nil")
	}
}

func TestToStringsAfterJSONRoundTrip(t *testing.T) {
	got := toStrings([]any{"noout", "norebalance"})
	if len(got) != 2 || got[0] != "noout" {
		t.Fatalf("got %v", got)
	}
}

func TestCheckNamesAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, n := range CheckNames {
		if seen[n] {
			t.Fatalf("duplicate check name %q", n)
		}
		seen[n] = true
		if !ValidCheckName(n) {
			t.Fatalf("%q is listed but not accepted", n)
		}
	}
	if ValidCheckName("no-such-check") {
		t.Fatal("unknown names must be rejected so a typo is not silently ignored")
	}
}

// Every command must produce a plan, and shutdown/maint-enter must be the ones
// that leave the window open.
func TestStepsForEveryCommand(t *testing.T) {
	e := &Env{Cfg: minimalConfig()}
	for _, c := range []string{CmdMaintEnter, CmdMaintExit, CmdShutdown, CmdReboot, CmdStart} {
		steps, err := StepsFor(c, e)
		if err != nil {
			t.Fatalf("%s: %v", c, err)
		}
		if len(steps) == 0 {
			t.Fatalf("%s produced no steps", c)
		}
		if steps[0].Name != "lock" {
			t.Fatalf("%s does not start with lock: %s", c, steps[0].Name)
		}
	}
	if _, err := StepsFor("nonsense", e); err == nil {
		t.Fatal("unknown command should error")
	}
	if !KeepsLockOnSuccess(CmdShutdown) || !KeepsLockOnSuccess(CmdMaintEnter) {
		t.Fatal("shutdown and maint-enter must keep the lock")
	}
	if KeepsLockOnSuccess(CmdReboot) || KeepsLockOnSuccess(CmdStart) || KeepsLockOnSuccess(CmdMaintExit) {
		t.Fatal("commands that close the window must release the lock")
	}
}

// The reboot plan has to include the restore and verify steps; a reboot that
// left the node cordoned would be worse than doing nothing.
func TestRebootPlanRestoresAndVerifies(t *testing.T) {
	steps, err := StepsFor(CmdReboot, &Env{Cfg: minimalConfig()})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, s := range steps {
		names[s.Name] = true
	}
	for _, want := range []string{"preflight", "ceph-flags", "k8s-drain", "host-reboot", "wait-up", "restore", "verify"} {
		if !names[want] {
			t.Errorf("reboot plan is missing %s", want)
		}
	}
}

func minimalConfig() *config.Config {
	c := &config.Config{}
	c.Cluster.CephFlags = []string{"noout", "norebalance"}
	return c
}
