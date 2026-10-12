package main

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tamago/pvectl/internal/state"
)

// The peer must run with the operator's flags, but never with --restart (it
// would discard the progress it is meant to continue) or a Talos target (the
// recorded one is inherited, and an alias could resolve differently).
func TestForwardedFlags(t *testing.T) {
	root := newRoot()
	cmd, _, err := root.Find([]string{"rolling", "upgrade", "cluster"})
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"--restart", "--yes", "--talos-version=latest", "--skip-check=ceph-health",
		"--skip-check=tasks", "--timeout-up=20m", "-v", "--log-dir=/tmp/x y"}
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	got := forwardedFlags(cmd.Flags())
	want := []string{"--log-dir=/tmp/x y", "--skip-check=ceph-health", "--skip-check=tasks",
		"--timeout-up=20m0s", "--verbose=true"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
}

func TestHandoffPeersPreferFinishedNodes(t *testing.T) {
	prog := &state.Rolling{Order: []string{"pve-3", "pve-2", "pve-1"}, Done: []string{"pve-3", "pve-2"}}
	if got := handoffPeers(prog, "pve-1"); !reflect.DeepEqual(got, []string{"pve-2", "pve-3"}) {
		t.Errorf("self last: got %v", got)
	}
	// Run from pve-2: pve-3 is done, pve-1 is not; it takes over and hands
	// off again when it reaches itself.
	prog.Done = []string{"pve-3"}
	if got := handoffPeers(prog, "pve-2"); !reflect.DeepEqual(got, []string{"pve-3", "pve-1"}) {
		t.Errorf("self in the middle: got %v", got)
	}
}

func TestHandoffArgvAndUnit(t *testing.T) {
	argv := handoffArgv("/usr/local/bin/pvectl", "upgrade", "pve-1", []string{"--verbose=true"})
	if got := strings.Join(argv, " "); got != "/usr/local/bin/pvectl rolling upgrade cluster --yes --handoff-from=pve-1 --verbose=true" {
		t.Errorf("argv: %s", got)
	}
	u := handoffUnit(rollingUpgrade, time.Date(2026, 10, 12, 15, 4, 5, 0, time.UTC))
	if u != "pvectl-rolling-upgrade-20261012-150405" {
		t.Errorf("unit: %s", u)
	}
}
