package plan

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

// Step names are the keys of state.json. A run interrupted under one build is
// resumed or aborted under the next, which finds each step -- and the rollback
// data it recorded -- by these exact strings. The lists are spelled out here
// rather than built from the constants so that changing a value fails a test.
func TestStepNamesAreStable(t *testing.T) {
	enter := []string{"lock", "preflight", "silence", "ceph-flags", "ha-evacuate", "k8s-drain"}
	want := map[string][]string{
		CmdMaintEnter: enter,
		CmdMaintExit:  {"lock", "restore", "verify"},
		CmdShutdown:   append(append([]string{}, enter...), "talos-stop", "guest-stop", "host-shutdown", "wait-down"),
		CmdReboot: append(append([]string{}, enter...), "talos-stop", "guest-stop", "host-reboot", "wait-down",
			"wait-up", "restore", "verify"),
		CmdUpgrade: append(append([]string{}, enter...), "talos-upgrade", "host-upgrade", "talos-stop", "guest-stop",
			"host-reboot", "wait-down", "wait-up", "restore", "verify", "upgrade-check"),
		CmdStart: {"lock", "wake", "wait-up", "restore", "verify"},
	}
	for command, names := range want {
		steps, err := StepsFor(command, &Env{Cfg: minimalConfig()})
		if err != nil {
			t.Fatalf("%s: %v", command, err)
		}
		r := &Runner{Steps: steps}
		if got := r.StepNames(); !reflect.DeepEqual(got, names) {
			t.Errorf("%s steps:\n got %v\nwant %v", command, got, names)
		}
	}
}

// --skip-check values are part of the command line, and of every script that
// passes one.
func TestCheckNamesAreStable(t *testing.T) {
	want := []string{"cluster-lock", "quorum", "nodes-online", "self-host", "ceph-health", "ceph-pgs", "ceph-flags",
		"k8s-ready", "k8s-drain", "etcd", "ha-capacity", "ha-affinity", "tasks", "guests", "ssh-path",
		"ephemeral-data", "apt-plan", "talos-target"}
	if !reflect.DeepEqual(CheckNames, want) {
		t.Errorf("check names:\n got %v\nwant %v", CheckNames, want)
	}
}

// A step that keeps going past failures reports them on one line, joined with
// "; " -- the text state.json records -- while each cause stays reachable.
func TestJoinErrors(t *testing.T) {
	if err := joinErrors(nil); err != nil {
		t.Fatalf("no errors must be nil, got %v", err)
	}
	sentinel := errors.New("no route to host")
	err := joinErrors([]error{
		fmt.Errorf("unset noout: %w", sentinel),
		errors.New("unset norebalance: HTTP 500"),
	})
	if got, want := err.Error(), "unset noout: no route to host; unset norebalance: HTTP 500"; got != want {
		t.Errorf("message %q, want %q", got, want)
	}
	if !errors.Is(err, sentinel) {
		t.Error("a joined error must still match its causes")
	}
	// Nested the way restore nests undoCephFlags.
	outer := joinErrors([]error{fmt.Errorf("ceph flags: %w", err), errors.New("silence: gone")})
	if got, want := outer.Error(), "ceph flags: unset noout: no route to host; unset norebalance: HTTP 500; silence: gone"; got != want {
		t.Errorf("nested message %q, want %q", got, want)
	}
	if !errors.Is(outer, sentinel) {
		t.Error("a nested joined error must still match its causes")
	}
}
