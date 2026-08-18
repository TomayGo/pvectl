package main

import (
	"strings"
	"testing"

	"github.com/tamago/pvectl/internal/plan"
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
