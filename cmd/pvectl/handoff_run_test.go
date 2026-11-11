package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	plog "github.com/TomayGo/pvectl/internal/log"
	"github.com/TomayGo/pvectl/internal/plan"
	"github.com/TomayGo/pvectl/internal/remote"
	"github.com/TomayGo/pvectl/internal/remote/remotetest"
	"github.com/TomayGo/pvectl/internal/state"
)

// handoffFixture is a rolling reboot run from node-a that has done node-c and
// node-b and has reached itself. peers answers the ssh calls.
type handoffFixture struct {
	h   *handoffCtx
	run *remotetest.Fake
	out *bytes.Buffer
	exe string
}

func newHandoff(t *testing.T, answer func(remotetest.Call) remotetest.Result) *handoffFixture {
	t.Helper()
	opts := plan.DefaultOptions()
	opts.Yes = true
	dir := t.TempDir()

	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		t.Fatal(err)
	}
	f := &handoffFixture{run: &remotetest.Fake{Answer: answer}, out: &bytes.Buffer{}, exe: exe}
	fs := pflag.NewFlagSet("rolling", pflag.ContinueOnError)
	fs.Duration("timeout-up", 0, "")
	fs.Bool("yes", false, "")
	if err := fs.Parse([]string{"--timeout-up=20m", "--yes"}); err != nil {
		t.Fatal(err)
	}
	f.h = &handoffCtx{
		cfg: fixtureConfig(t), lg: plog.To(f.out), fs: fs, command: plan.CmdReboot, self: "node-a",
		prog: &state.Rolling{Command: rollingReboot, Order: []string{"node-c", "node-b", "node-a"}, Done: []string{"node-c", "node-b"}},
		opts: opts, stateDir: dir, run: f.run,
	}
	return f
}

// peerOK is what a healthy peer prints for the probe.
func peerOK(c remotetest.Call) remotetest.Result {
	if strings.Contains(c.Script(), "--version") {
		return remotetest.Result{Stdout: "pvectl version " + version + "\nprogress-ok\n"}
	}
	return remotetest.Result{}
}

func (f *handoffFixture) lines() []string {
	return strings.Split(strings.TrimRight(f.out.String(), "\n"), "\n")
}

// The probe is one ssh: the peer's version, whether it has systemd-run, and
// whether it already sees rolling.json.
func TestHandoffProbe(t *testing.T) {
	f := newHandoff(t, peerOK)
	if err := f.h.probe(context.Background(), "node-b", "/usr/local/bin/pvectl"); err != nil {
		t.Fatal(err)
	}
	calls := f.run.Calls()
	if len(calls) != 1 || calls[0].Name != "ssh" || calls[0].Stream {
		t.Fatalf("calls %v", calls)
	}
	host := remote.Host{Name: "node-b", IP: "192.0.2.12"}
	if got := calls[0].Args[:len(calls[0].Args)-1]; !reflect.DeepEqual(got, remote.Args(host)) {
		t.Errorf("ssh options %v", got)
	}
	want := "/usr/local/bin/pvectl --version; command -v systemd-run >/dev/null || echo no-systemd-run; " +
		"test -s " + filepath.Join(f.h.stateDir, "rolling.json") + " && echo progress-ok; true"
	if got := calls[0].Script(); got != want {
		t.Errorf("script:\n got %s\nwant %s", got, want)
	}
}

func TestHandoffProbeRefusals(t *testing.T) {
	cases := []struct {
		name   string
		result remotetest.Result
		dryRun bool
		want   string
	}{
		{"unreachable", remotetest.Result{Stderr: "ssh: connect to host 192.0.2.12 port 22: No route to host\n", Err: errors.New("exit status 255")}, false,
			"ssh: exit status 255: ssh: connect to host 192.0.2.12 port 22: No route to host"},
		{"other build", remotetest.Result{Stdout: "pvectl version 0.0.1\nprogress-ok\n"}, false,
			`runs "pvectl version 0.0.1", this node runs "pvectl version ` + version + `" (deploy the same build everywhere first)`},
		{"no systemd-run", remotetest.Result{Stdout: "pvectl version " + version + "\nno-systemd-run\nprogress-ok\n"}, false,
			"systemd-run not found"},
		{"no progress", remotetest.Result{Stdout: "pvectl version " + version + "\n"}, false,
			"does not see the recorded progress in "},
		// A dry run saved no progress, so it cannot ask the peer for it.
		{"no progress, dry-run", remotetest.Result{Stdout: "pvectl version " + version + "\n"}, true, ""},
	}
	for _, c := range cases {
		f := newHandoff(t, func(remotetest.Call) remotetest.Result { return c.result })
		f.h.opts.DryRun = c.dryRun
		err := f.h.probe(context.Background(), "node-b", "/usr/local/bin/pvectl")
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.want != "" && (err == nil || !strings.HasPrefix(err.Error(), c.want)):
			t.Errorf("%s: error %v, want %s", c.name, err, c.want)
		}
	}
}

// The hand-off: save progress, pick the peer that finished most recently,
// start the same command there under systemd-run, follow its journal.
func TestHandOffStartsTheRunOnAPeer(t *testing.T) {
	f := newHandoff(t, peerOK)
	if err := f.h.handOff(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved, err := state.LoadRolling(f.h.stateDir)
	if err != nil || saved == nil || !reflect.DeepEqual(saved.Done, []string{"node-c", "node-b"}) {
		t.Fatalf("rolling.json: %+v, %v", saved, err)
	}

	calls := f.run.Calls()
	if len(calls) != 3 {
		t.Fatalf("%d ssh call(s), want probe, start, follow: %v", len(calls), calls)
	}
	for i, c := range calls {
		// node-b finished last, so it is asked first -- and it accepts.
		if c.Name != "ssh" || c.Args[len(c.Args)-2] != "root@192.0.2.12" {
			t.Errorf("call %d went to %s", i, c.Args[len(c.Args)-2])
		}
	}
	start := calls[1].Script()
	unit := regexpFind(t, `--unit=(pvectl-rolling-reboot-\d{8}-\d{6}) `, start)
	wantStart := "systemd-run --quiet --collect --uid=root --unit=" + unit +
		" '--description=pvectl rolling reboot (handed off from node-a)' -- " +
		f.exe + " rolling reboot cluster --yes --handoff-from=node-a --timeout-up=20m0s"
	if start != wantStart {
		t.Errorf("start command:\n got %s\nwant %s", start, wantStart)
	}
	if calls[1].Stream || !calls[2].Stream {
		t.Error("the start is run to completion; only the follow streams")
	}
	wantFollow := "journalctl -o cat -n all -fu " + unit + " & j=$!; sleep 2; " +
		"while systemctl -q is-active " + unit + "; do sleep 2; done; sleep 1; kill $j"
	if got := calls[2].Script(); got != wantFollow {
		t.Errorf("follow command:\n got %s\nwant %s", got, wantFollow)
	}
	wantLines(t, f.lines(),
		"",
		"================ node-a ================",
		"node-a is the node running pvectl; handing the run over to a peer.",
		"  rolling reboot continues on node-b as systemd unit "+unit,
		"Its output follows; Ctrl-C (or this node going down) only stops following.",
		"Follow it again with:  ssh 192.0.2.12 journalctl -o cat -fu "+unit,
	)
}

func regexpFind(t *testing.T, pattern, s string) string {
	t.Helper()
	m := regexpMust(pattern).FindStringSubmatch(s)
	if m == nil {
		t.Fatalf("%q does not match %s", s, pattern)
	}
	return m[1]
}

// A peer that fails the probe is passed over with a warning; the next one in
// line takes the run.
func TestHandOffFallsBackToTheNextPeer(t *testing.T) {
	f := newHandoff(t, func(c remotetest.Call) remotetest.Result {
		if strings.Contains(c.Line(), "root@192.0.2.12") {
			return remotetest.Result{Err: errors.New("exit status 255")}
		}
		return peerOK(c)
	})
	if err := f.h.handOff(context.Background()); err != nil {
		t.Fatal(err)
	}
	var hosts []string
	for _, c := range f.run.Calls() {
		hosts = append(hosts, c.Args[len(c.Args)-2])
	}
	if want := []string{"root@192.0.2.12", "root@192.0.2.13", "root@192.0.2.13", "root@192.0.2.13"}; !reflect.DeepEqual(hosts, want) {
		t.Errorf("ssh went to %v, want %v", hosts, want)
	}
	if !strings.Contains(f.out.String(), "  ! cannot hand off to node-b: ssh: exit status 255: \n") ||
		!strings.Contains(f.out.String(), "rolling reboot continues on node-c as systemd unit ") {
		t.Errorf("printed:\n%s", f.out)
	}
}

func TestHandOffWithNoPeerPrintsTheManualWayOut(t *testing.T) {
	f := newHandoff(t, func(remotetest.Call) remotetest.Result {
		return remotetest.Result{Err: errors.New("exit status 255")}
	})
	err := f.h.handOff(context.Background())
	if err == nil || err.Error() != "rolling reboot stopped at node-a: no peer could take over" {
		t.Errorf("error %v", err)
	}
	if n := len(f.run.Calls()); n != 2 {
		t.Errorf("%d ssh call(s), want one probe per peer", n)
	}
	wantLines(t, f.lines()[3:],
		"  ! cannot hand off to node-b: ssh: exit status 255: ",
		"  ! cannot hand off to node-c: ssh: exit status 255: ",
		"",
		"Every node before node-a is done. Finish from a peer:",
		"    ssh 192.0.2.13 'pvectl rolling reboot cluster --yes'",
		"    ssh 192.0.2.12 'pvectl rolling reboot cluster --yes'",
		"",
		"(ssh by IP, not by hostname: the tailnet name is hijacked by Tailscale SSH)",
	)
}

// If starting the run on the peer fails, no other peer is tried: the run may
// be going after all, and two of them must not be.
func TestHandOffDoesNotRetryAFailedStart(t *testing.T) {
	f := newHandoff(t, func(c remotetest.Call) remotetest.Result {
		if strings.HasPrefix(c.Script(), "systemd-run ") {
			return remotetest.Result{Stderr: "Connection to 192.0.2.12 closed by remote host.\n", Err: errors.New("exit status 255")}
		}
		return peerOK(c)
	})
	err := f.h.handOff(context.Background())
	if err == nil || err.Error() != "rolling reboot stopped at node-a: hand-off to node-b failed" {
		t.Errorf("error %v", err)
	}
	if n := len(f.run.Calls()); n != 2 {
		t.Errorf("%d ssh call(s), want the probe and the one start", n)
	}
	out := f.out.String()
	if !strings.Contains(out, "  ! starting the run on node-b failed: exit status 255: Connection to 192.0.2.12 closed by remote host.\n") ||
		!strings.Contains(out, "Check `ssh 192.0.2.12 systemctl status pvectl-rolling-reboot-") ||
		!strings.Contains(out, "Every node before node-a is done. Finish from a peer:") {
		t.Errorf("printed:\n%s", out)
	}
}

// --dry-run probes, says what it would run, and neither saves progress nor
// starts anything.
func TestHandOffDryRun(t *testing.T) {
	f := newHandoff(t, peerOK)
	f.h.opts.DryRun = true
	if err := f.h.handOff(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(f.run.Calls()); n != 1 {
		t.Errorf("%d ssh call(s), want only the probe", n)
	}
	if prog, _ := state.LoadRolling(f.h.stateDir); prog != nil {
		t.Error("dry-run saved rolling.json")
	}
	l := f.lines()
	if len(l) != 5 || l[3] != "(dry-run) would continue on node-b:" || !strings.HasPrefix(l[4], "    ssh 192.0.2.12 systemd-run --quiet --collect --uid=root --unit=pvectl-rolling-reboot-") {
		t.Errorf("printed:\n%s", f.out)
	}
}

// Progress that cannot be saved stops the hand-off before any peer is asked:
// the peer would start the whole run over.
func TestHandOffNeedsProgressSaved(t *testing.T) {
	f := newHandoff(t, peerOK)
	f.h.stateDir = filepath.Join(f.h.stateDir, "gone")
	err := f.h.handOff(context.Background())
	if err == nil || !strings.HasPrefix(err.Error(), "record progress before hand-off: ") {
		t.Errorf("error %v", err)
	}
	if n := len(f.run.Calls()); n != 0 {
		t.Errorf("%d ssh call(s) after failing to save", n)
	}
}
