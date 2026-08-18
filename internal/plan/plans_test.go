package plan

import (
	"testing"

	"github.com/tamago/pvectl/internal/state"
)

// Re-running a command instead of `resume` starts a fresh state, but the flags
// the abandoned attempt set are still set on the cluster. Losing the record of
// which ones we own is what leaves noout behind after restore.
func TestCarryRollbackData(t *testing.T) {
	dir := t.TempDir()
	old := state.NewState(dir, "shutdown", "pve-3", "pve-1", []string{"ceph-flags", "silence", "guest-stop"})
	old.SetData("ceph-flags", "set_by_us", []string{"noout"})
	old.SetData("silence", "ids", []string{"abc"})
	old.SetData("guest-stop", "stopped", []int{105})

	// The new plan has no guest-stop step, so its data must not be resurrected.
	fresh := state.NewState(dir, "maint-enter", "pve-3", "pve-1", []string{"ceph-flags", "silence"})
	carryRollbackData(old, fresh)

	if got := toStrings(fresh.Step("ceph-flags").Data["set_by_us"]); len(got) != 1 || got[0] != "noout" {
		t.Fatalf("ceph flag ownership not carried: %v", got)
	}
	if got := toStrings(fresh.Step("silence").Data["ids"]); len(got) != 1 || got[0] != "abc" {
		t.Fatalf("silence ids not carried: %v", got)
	}
	if fresh.Step("guest-stop") != nil {
		t.Fatal("a step absent from the new plan must not be added")
	}
}
