package main

import (
	"strings"
	"testing"

	plog "github.com/tamago/pvectl/internal/log"
	"github.com/tamago/pvectl/internal/plan"
	"github.com/tamago/pvectl/internal/state"
)

// The same node operation is reachable at two depths: `pvectl reboot node X`
// names the subcommand "node" and leaves one argument, while
// `pvectl plan reboot node X` names it "reboot" and leaves two. Both must
// resolve to the same node -- the first shipped version accepted only the
// second and rejected `pvectl reboot node pve-3` outright.
func TestNodeOpCmdAcceptsBothArgShapes(t *testing.T) {
	cases := []struct {
		use  string
		args []string
	}{
		{"node", []string{"pve-3"}},
		{"reboot", []string{"node", "pve-3"}},
		{"enter", []string{"node", "pve-3"}},
	}
	for _, c := range cases {
		cmd := nodeOpCmd(c.use, plan.CmdReboot, "test")
		if err := cmd.Args(cmd, c.args); err != nil {
			t.Errorf("%s %v: %v", c.use, c.args, err)
			continue
		}
		if got := c.args[len(c.args)-1]; got != "pve-3" {
			t.Errorf("%s %v resolved to %q", c.use, c.args, got)
		}
	}
}

func TestNodeOpCmdRejectsBadArgs(t *testing.T) {
	cmd := nodeOpCmd("node", plan.CmdReboot, "test")
	if err := cmd.Args(cmd, nil); err == nil || !strings.Contains(err.Error(), "which node") {
		t.Errorf("missing name gave %v", err)
	}
	if err := cmd.Args(cmd, []string{"nod", "pve-3"}); err == nil {
		t.Error("a misspelled `node` keyword should be rejected")
	}
	if err := cmd.Args(cmd, []string{"node", "pve-3", "extra"}); err == nil {
		t.Error("three arguments should be rejected")
	}
}

// `rolling reboot` cannot reboot the node it runs on, so it hands the last one
// to the operator. A plain `reboot node pve-1` from a peer has to close that
// record, or the next rolling reboot resumes it and silently skips the nodes
// the previous run had already done.
func TestCloseRollingEntry(t *testing.T) {
	dir := t.TempDir()
	e := &plan.Env{Target: "pve-1", StateDir: dir, Log: plog.Discard()}
	prog := &state.Rolling{Order: []string{"pve-3", "pve-2", "pve-1"}, Done: []string{"pve-3", "pve-2"}}
	if err := prog.Save(dir); err != nil {
		t.Fatal(err)
	}

	closeRollingEntry(e, plan.CmdReboot, "pve-1")

	got, err := state.LoadRolling(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("finishing the last node should clear the record, still have %v", got.Done)
	}
}

func TestCloseRollingEntryKeepsUnfinishedRecord(t *testing.T) {
	dir := t.TempDir()
	e := &plan.Env{Target: "pve-2", StateDir: dir, Log: plog.Discard()}
	prog := &state.Rolling{Order: []string{"pve-3", "pve-2", "pve-1"}, Done: []string{"pve-3"}}
	if err := prog.Save(dir); err != nil {
		t.Fatal(err)
	}

	closeRollingEntry(e, plan.CmdReboot, "pve-2")
	closeRollingEntry(e, plan.CmdReboot, "pve-2") // idempotent: must not double-count

	got, err := state.LoadRolling(dir)
	if err != nil || got == nil {
		t.Fatalf("record should survive with pve-1 outstanding (%v, %v)", got, err)
	}
	if len(got.Done) != 2 {
		t.Fatalf("Done is %v, want two entries", got.Done)
	}
	if rem := got.Remaining(); len(rem) != 1 || rem[0] != "pve-1" {
		t.Fatalf("remaining is %v, want [pve-1]", rem)
	}
}

// With no rolling reboot in flight, a standalone reboot must not invent one.
func TestCloseRollingEntryNoRecord(t *testing.T) {
	dir := t.TempDir()
	e := &plan.Env{Target: "pve-3", StateDir: dir, Log: plog.Discard()}
	closeRollingEntry(e, plan.CmdReboot, "pve-3")
	got, err := state.LoadRolling(dir)
	if err != nil || got != nil {
		t.Fatalf("no record should have been created (%v, %v)", got, err)
	}
}

// Only the commands that put a node back in service close its rolling entry.
// `shutdown` leaves the window open; the `start` that closes it is what counts,
// and `resume` rescuing an interrupted reboot has to count too.
func TestClosesRollingEntry(t *testing.T) {
	for _, c := range []string{plan.CmdReboot, plan.CmdUpgrade, plan.CmdStart, plan.CmdMaintExit} {
		if !closesRollingEntry(rollingReboot, c) {
			t.Errorf("%s should close the rolling entry", c)
		}
	}
	for _, c := range []string{plan.CmdShutdown, plan.CmdMaintEnter} {
		if closesRollingEntry(rollingReboot, c) {
			t.Errorf("%s leaves the node out of service and must not close the entry", c)
		}
	}
}

// A rolling upgrade is only advanced by an upgrade: counting a plain reboot
// would mark a node done that never installed anything.
func TestRollingUpgradeClosedOnlyByUpgrade(t *testing.T) {
	if !closesRollingEntry(rollingUpgrade, plan.CmdUpgrade) {
		t.Fatal("upgrade must close a rolling-upgrade entry")
	}
	for _, c := range []string{plan.CmdReboot, plan.CmdStart, plan.CmdMaintExit, plan.CmdShutdown} {
		if closesRollingEntry(rollingUpgrade, c) {
			t.Errorf("%s must not close a rolling-upgrade entry", c)
		}
	}
}

// Continuing a rolling upgrade without the flag inherits the recorded release;
// naming a different one is refused rather than producing a mixed cluster.
func TestAdoptRecordedTalosTarget(t *testing.T) {
	defer func(o plan.Options) { g.opts = o }(g.opts)
	prog := &state.Rolling{Command: rollingUpgrade, TalosVersion: "v1.13.7"}

	g.opts.TalosVersion, g.opts.TalosImage = "", ""
	if err := adoptRecordedTalosTarget(prog); err != nil || g.opts.TalosVersion != "v1.13.7" {
		t.Fatalf("recorded version not inherited: %v %q", err, g.opts.TalosVersion)
	}
	g.opts.TalosVersion = "v1.14.0"
	if err := adoptRecordedTalosTarget(prog); err == nil {
		t.Fatal("a different release must be refused")
	}
}

func TestCloseRollingEntryIgnoresShutdown(t *testing.T) {
	dir := t.TempDir()
	e := &plan.Env{Target: "pve-1", StateDir: dir, Log: plog.Discard()}
	prog := &state.Rolling{Order: []string{"pve-3", "pve-2", "pve-1"}, Done: []string{"pve-3", "pve-2"}}
	if err := prog.Save(dir); err != nil {
		t.Fatal(err)
	}
	closeRollingEntry(e, plan.CmdShutdown, "pve-1")
	got, err := state.LoadRolling(dir)
	if err != nil || got == nil {
		t.Fatalf("record should survive a shutdown (%v, %v)", got, err)
	}
	if len(got.Done) != 2 {
		t.Fatalf("shutdown must not mark the node done: %v", got.Done)
	}
}
