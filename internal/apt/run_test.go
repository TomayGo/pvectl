package apt

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	plog "github.com/TomayGo/pvectl/internal/log"
	"github.com/TomayGo/pvectl/internal/remote"
	"github.com/TomayGo/pvectl/internal/remote/remotetest"
)

var host = Host{Name: "pve-1", IP: "192.0.2.11"}

// Every apt operation is one ssh to the host's LAN IP, with the host key
// pinned to the node's entry in pmxcfs and the script as the remote command.
func TestCommandsGoOverSSHToTheHost(t *testing.T) {
	run := &remotetest.Fake{Answer: func(c remotetest.Call) remotetest.Result {
		if c.Script() == "uname -r" {
			return remotetest.Result{Stdout: "7.0.14-25-pve\n"}
		}
		return remotetest.Result{}
	}}
	c := NewWithRunner(plog.Discard(), false, run)
	ctx := context.Background()
	if err := c.Update(ctx, host); err != nil {
		t.Fatal(err)
	}
	if err := c.FullUpgrade(ctx, host); err != nil {
		t.Fatal(err)
	}
	if k, err := c.Kernel(ctx, host); err != nil || k != "7.0.14-25-pve" {
		t.Errorf("kernel %q, %v", k, err)
	}
	const env = "export DEBIAN_FRONTEND=noninteractive APT_LISTCHANGES_FRONTEND=none; "
	want := []string{
		env + "apt-get -q --error-on=any update",
		env + "dpkg --force-confdef --force-confold --configure -a && " +
			"apt-get -y -q -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold full-upgrade",
		"uname -r",
	}
	calls := run.Calls()
	var scripts []string
	for _, call := range calls {
		scripts = append(scripts, call.Script())
		if call.Name != "ssh" || !call.Stream {
			t.Errorf("%s: want a streamed ssh", call.Line())
		}
		if got := call.Args[:len(call.Args)-1]; !reflect.DeepEqual(got, remote.Args(host)) {
			t.Errorf("ssh options %v", got)
		}
	}
	if !reflect.DeepEqual(scripts, want) {
		t.Errorf("scripts:\n got %q\nwant %q", scripts, want)
	}
}

// --dry-run installs nothing, but still simulates: that is the read-only part.
func TestDryRunOnlySimulates(t *testing.T) {
	run := &remotetest.Fake{Answer: func(remotetest.Call) remotetest.Result {
		return remotetest.Result{Stdout: "Inst pve-manager [9.2.4] (9.2.5 Proxmox VE:9.x/trixie [all])\nConf pve-manager (9.2.5)\n"}
	}}
	var out bytes.Buffer
	c := NewWithRunner(plog.To(&out), true, run)
	ctx := context.Background()
	if err := c.Update(ctx, host); err != nil {
		t.Fatal(err)
	}
	if err := c.FullUpgrade(ctx, host); err != nil {
		t.Fatal(err)
	}
	p, err := c.Simulate(ctx, host)
	if err != nil || len(p.Upgrade) != 1 || p.Upgrade[0].Name != "pve-manager" {
		t.Fatalf("plan %+v, %v", p, err)
	}
	if calls := run.Calls(); len(calls) != 1 || !strings.HasSuffix(calls[0].Script(), "apt-get -s -q full-upgrade") {
		t.Errorf("calls %v", calls)
	}
	want := "  (dry-run) would run apt-get update on 192.0.2.11\n  (dry-run) would run apt-get full-upgrade on 192.0.2.11\n"
	if out.String() != want {
		t.Errorf("printed %q", out.String())
	}
}

// A command that ran and failed is reported with what it was and the tail of
// what it said on stderr; what it printed before failing is still returned.
func TestFailedCommandCarriesItsStderr(t *testing.T) {
	run := &remotetest.Fake{Answer: func(remotetest.Call) remotetest.Result {
		return remotetest.Result{
			Stdout: "Reading package lists...\npartial",
			Stderr: "W: one\nW: two\nW: three\nW: four\nW: five\r\nE: Failed to fetch https://example.invalid/dists/trixie/InRelease\n",
			Err:    errors.New("exit status 100"),
		}
	}}
	c := NewWithRunner(plog.Discard(), false, run)
	out, err := c.run(context.Background(), host, noninteractive+"apt-get -q --error-on=any update")
	want := `ssh root@192.0.2.11 "apt-get": exit status 100: W: two | W: three | W: four | W: five | ` +
		`E: Failed to fetch https://example.invalid/dists/trixie/InRelease`
	if err == nil || err.Error() != want {
		t.Errorf("error:\n got %v\nwant %s", err, want)
	}
	// Lines are re-joined with "\n", including a last one that had none.
	if out != "Reading package lists...\npartial\n" {
		t.Errorf("stdout %q", out)
	}
	if err := c.Update(context.Background(), host); err == nil {
		t.Error("Update must fail when apt-get does")
	}
}

// ssh itself not starting is a different message, without the script.
func TestSSHNotStarting(t *testing.T) {
	notFound := &exec.Error{Name: "ssh", Err: exec.ErrNotFound}
	run := &remotetest.Fake{Answer: func(remotetest.Call) remotetest.Result {
		return remotetest.Result{Err: &remote.StartError{Err: notFound}}
	}}
	c := NewWithRunner(plog.Discard(), false, run)
	_, err := c.Kernel(context.Background(), host)
	if err == nil || err.Error() != `ssh 192.0.2.11: exec: "ssh": executable file not found in $PATH` {
		t.Errorf("error %v", err)
	}
	if !errors.Is(err, exec.ErrNotFound) {
		t.Error("the cause must stay reachable")
	}
}
