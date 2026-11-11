package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/TomayGo/pvectl/internal/plan"
	"github.com/TomayGo/pvectl/internal/state"
)

// These run whole commands in-process -- `pvectl maint exit node node-a` and
// so on, through newRoot and cobra -- against the fake cluster, with state in
// a temporary directory. They pin down what happens around the plan: the lock,
// state.json, rolling.json and the closing messages.

// openWindow leaves the fake cluster the way `maint enter node node-a` does:
// node-a locked, cordoned, noout set and recorded as ours, in HA maintenance
// that this window owns, one silence.
func openWindow(t *testing.T, c *cli) {
	t.Helper()
	w := c.w
	w.healthy()
	w.pve.Static("PUT", "/cluster/ceph/flags/noout", nil)
	w.proxy.addSilence("s1", "pvectl")
	if _, err := state.Acquire(c.stateDir, "node-b", "node-a", plan.CmdMaintEnter); err != nil {
		t.Fatal(err)
	}
	names := []string{"lock", "preflight", "silence", "ceph-flags", "ha-evacuate", "k8s-drain"}
	st := state.NewState(c.stateDir, plan.CmdMaintEnter, "node-a", "node-b", names)
	for _, n := range names {
		st.Set(n, state.StepDone, nil)
	}
	st.SetData("silence", "ids", []string{"s1"})
	st.SetData("ceph-flags", "set_by_us", []string{"noout"})
	st.SetData("ha-evacuate", "maintenance_set_by_us", true)
	st.Finished = true
	saveState(t, st)
}

func wantClosed(t *testing.T, c *cli) {
	t.Helper()
	w := c.w
	if got, want := w.pve.Writes(), []string{"PUT /cluster/ceph/flags/noout value=0"}; !reflect.DeepEqual(got, want) {
		t.Errorf("API writes %q, want %q", got, want)
	}
	if got, want := w.commands(), []string{"ha-manager crm-command node-maintenance disable node-a"}; !reflect.DeepEqual(got, want) {
		t.Errorf("commands %q, want %q", got, want)
	}
	if got, want := w.proxy.writes(), []string{
		"DELETE monitoring/alertmanager:9093/api/v2/silence/s1",
		"POST monitoring/discord-bridge:9095/",
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("alertmanager / bridge writes %q, want %q", got, want)
	}
	if got := w.cordoned(); len(got) != 0 {
		t.Errorf("still cordoned: %v", got)
	}
	if exists(t, c.stateDir, "maintenance.lock") {
		t.Error("the maintenance lock is still held")
	}
	if exists(t, c.stateDir, "state.json") {
		t.Error("state.json is still there")
	}
}

// tail returns the transcript from the first line equal to from, or fails.
func tail(t *testing.T, lines []string, from string) []string {
	t.Helper()
	for i, l := range lines {
		if l == from {
			return lines[i:]
		}
	}
	t.Fatalf("no line %q in:\n%s", from, strings.Join(lines, "\n"))
	return nil
}

func TestMaintExitClosesTheWindow(t *testing.T) {
	c := newCLI(t, newWorld(t, "talos-a"))
	openWindow(t, c)
	// node-a is the last node of a rolling reboot that stopped at it.
	prog := &state.Rolling{Command: rollingReboot, Order: []string{"node-c", "node-b", "node-a"}, Done: []string{"node-c", "node-b"}}
	if err := prog.Save(c.stateDir); err != nil {
		t.Fatal(err)
	}

	if err := c.run("maint", "exit", "node", "node-a", "--yes"); err != nil {
		t.Fatal(err)
	}
	wantClosed(t, c)
	if exists(t, c.stateDir, "rolling.json") {
		t.Error("the rolling record was not closed")
	}
	got := c.transcript()
	wantLines(t, got[:8],
		"| pvectl "+version+" -- maint-exit on node-a (from node-b)",
		"| ",
		"adopting the maintenance lock held for node-a since "+lockStarted(t, got),
		"continuing the maintenance window opened by `maint-enter node-a`",
		"| Plan: maint-exit node-a",
		"| #   step             undo     description",
		"| "+strings.Repeat("-", 78),
		"| 1   lock             yes      hold the cluster-wide maintenance lock",
	)
	// After the plan: the lock, then the rolling record, then the summary.
	end := tail(t, got, "[verify] maintenance lock released")
	if len(end) != 6 || end[1] != "| " || end[2] != "| This completes the rolling reboot of node-c, node-b, node-a." ||
		end[3] != "| " || !strings.HasPrefix(end[4], "| maint-exit node-a completed in ") || !strings.HasPrefix(end[5], "| transcript: "+c.logDir) {
		t.Errorf("closing lines:\n%s", strings.Join(end, "\n"))
	}
	for _, want := range []string{
		"=== step restore: uncordon, clear ceph flags, end HA maintenance, start guests, lift the silence",
		"[restore] node talos-a marked schedulable",
		"[restore] ceph flag noout cleared",
		"[restore] running ha-manager crm-command node-maintenance disable node-a",
		"[restore] silence s1 expired",
		"[restore] discord notified: [resolved] PvectlMaintenance",
		"[verify] all kubernetes nodes Ready: ready",
	} {
		if !containsLine(got, want) {
			t.Errorf("transcript is missing %q", want)
		}
	}
	if len(c.built) != 1 || c.built[0].target != "node-a" || !c.built[0].opts.Yes || c.built[0].opts.DryRun {
		t.Errorf("env built with %+v", c.built)
	}
}

func lockStarted(t *testing.T, lines []string) string {
	t.Helper()
	const p = "adopting the maintenance lock held for node-a since "
	for _, l := range lines {
		if strings.HasPrefix(l, p) {
			return strings.TrimPrefix(l, p)
		}
	}
	t.Fatalf("no adoption line in:\n%s", strings.Join(lines, "\n"))
	return ""
}

func containsLine(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

// A rolling upgrade is only advanced by an upgrade; maint exit closes the
// window and leaves that record as it is.
func TestMaintExitLeavesARollingUpgradeOpen(t *testing.T) {
	c := newCLI(t, newWorld(t, "talos-a"))
	openWindow(t, c)
	prog := &state.Rolling{Command: rollingUpgrade, Order: []string{"node-c", "node-b", "node-a"}, Done: []string{"node-c", "node-b"}}
	if err := prog.Save(c.stateDir); err != nil {
		t.Fatal(err)
	}
	if err := c.run("maint", "exit", "node", "node-a", "--yes"); err != nil {
		t.Fatal(err)
	}
	wantClosed(t, c)
	left, err := state.LoadRolling(c.stateDir)
	if err != nil || left == nil || len(left.Done) != 2 {
		t.Errorf("rolling record: %+v, %v", left, err)
	}
}

// A step failing leaves everything for `resume`: the lock, state.json, the
// rolling record.
func TestMaintExitFailureKeepsTheWindow(t *testing.T) {
	c := newCLI(t, newWorld(t, "talos-a"))
	openWindow(t, c)
	c.w.cmd.Answer = failing("exit status 2")
	err := c.run("maint", "exit", "node", "node-a", "--yes")
	want := "step restore: ha maintenance: ha-manager crm-command node-maintenance disable node-a: exit status 2"
	if err == nil || err.Error() != want {
		t.Fatalf("error %v", err)
	}
	if !exists(t, c.stateDir, "maintenance.lock") || !exists(t, c.stateDir, "state.json") {
		t.Error("a failed run must keep its lock and state")
	}
	st, _ := state.LoadState(c.stateDir)
	if st == nil || st.Finished || st.FirstIncomplete() != "restore" || st.Command != plan.CmdMaintExit {
		t.Errorf("state: %+v", st)
	}
}

// resume picks the failed run up at its failed step and, when it gets
// through, closes out exactly as the original command would have.
func TestResumeFinishesAndClosesTheWindow(t *testing.T) {
	c := newCLI(t, newWorld(t, "talos-a"))
	openWindow(t, c)
	prog := &state.Rolling{Command: rollingReboot, Order: []string{"node-c", "node-b", "node-a"}, Done: []string{"node-c", "node-b"}}
	if err := prog.Save(c.stateDir); err != nil {
		t.Fatal(err)
	}
	c.w.cmd.Answer = failing("exit status 2")
	if err := c.run("maint", "exit", "node", "node-a", "--yes"); err == nil {
		t.Fatal("the first attempt was meant to fail")
	}
	c.w.cmd.Answer = nil

	if err := c.run("resume", "--yes"); err != nil {
		t.Fatal(err)
	}
	if exists(t, c.stateDir, "maintenance.lock") || exists(t, c.stateDir, "state.json") || exists(t, c.stateDir, "rolling.json") {
		t.Error("resume left the lock, state.json or rolling.json behind")
	}
	if got := c.w.cordoned(); len(got) != 0 {
		t.Errorf("still cordoned: %v", got)
	}
	got := c.transcript()
	start := tail(t, got, "| resuming maint-exit node-a from step restore")
	if !strings.HasPrefix(start[1], "adopting the maintenance lock held for node-a since ") {
		t.Errorf("after the resume line: %q", start[1])
	}
	// Unlike the original command, resume ends on its summary line: it does
	// not print where the transcript is.
	end := tail(t, start, "[verify] maintenance lock released")
	if len(end) != 5 || end[1] != "| " || end[2] != "| This completes the rolling reboot of node-c, node-b, node-a." ||
		end[3] != "| " || !strings.HasPrefix(end[4], "| resume completed in ") {
		t.Errorf("closing lines:\n%s", strings.Join(end, "\n"))
	}
}

func TestResumeRefusals(t *testing.T) {
	c := newCLI(t, newWorld(t))
	if err := c.run("resume", "--yes"); err == nil || err.Error() != "no interrupted run to resume" {
		t.Errorf("no state: %v", err)
	}
	done := state.NewState(c.stateDir, plan.CmdShutdown, "node-a", "node-b", []string{"lock"})
	done.Finished = true
	saveState(t, done)
	if err := c.run("resume", "--yes"); err == nil || err.Error() != "the recorded shutdown node-a run already finished" {
		t.Errorf("finished run: %v", err)
	}
	if len(c.built) != 0 {
		t.Error("an Env was built for a resume that had nothing to do")
	}
}

// abort rolls the open window back and removes the lock and the state.
func TestAbortRollsBackAndClears(t *testing.T) {
	c := newCLI(t, newWorld(t, "talos-a"))
	openWindow(t, c)
	if err := c.run("abort", "--yes"); err != nil {
		t.Fatal(err)
	}
	wantClosed(t, c)
	got := c.transcript()
	for _, want := range []string{
		"| lock held for node-a by node-b",
		"| rolling back maint-enter node-a (6 completed step(s))",
		"=== step undo:k8s-drain: roll back cordon and drain the Talos node",
		"=== step undo:lock: roll back hold the cluster-wide maintenance lock",
		"| Rollback complete.",
	} {
		if !containsLine(got, want) {
			t.Errorf("transcript is missing %q:\n%s", want, strings.Join(got, "\n"))
		}
	}
}

// Declining the confirmation backs out: an adopted lock stays (the window is
// still open), and state this run did not create is not removed.
func TestDecliningLeavesAnAdoptedWindowAlone(t *testing.T) {
	c := newCLI(t, newWorld(t, "talos-a"))
	openWindow(t, c)
	// No --yes, and stdin is not a terminal that says yes.
	err := c.run("maint", "exit", "node", "node-a")
	if !errors.Is(err, errNotStarted) {
		t.Fatalf("error %v, want errNotStarted", err)
	}
	if !exists(t, c.stateDir, "maintenance.lock") || !exists(t, c.stateDir, "state.json") {
		t.Error("declining dropped the lock or state of a window another command opened")
	}
	if len(c.w.pve.Writes()) != 0 || len(c.w.cordoned()) != 1 {
		t.Error("declining changed the cluster")
	}
}

// ---------- command line -> options ----------

// stop makes every Env build fail, so a command ends right after its options
// were handed over.
var errStop = errors.New("stop: this test only looks at the options")

func optsFor(t *testing.T, args ...string) (builtEnv, error) {
	t.Helper()
	c := newCLI(t, newWorld(t))
	c.refuse = errStop
	err := c.run(args...)
	if len(c.built) == 0 {
		return builtEnv{}, err
	}
	if !errors.Is(err, errStop) {
		t.Fatalf("%v: %v", args, err)
	}
	return c.built[0], err
}

func TestFlagsReachTheOptions(t *testing.T) {
	b, _ := optsFor(t, "reboot", "node", "node-a", "--yes", "--skip-check=etcd,tasks", "--skip-check", "guests",
		"--no-silence", "--force-stop-after=30s", "--timeout-drain=1m", "--timeout-guest=2m", "--timeout-migrate=3m",
		"--timeout-down=4m", "--timeout-up=5m", "--silence-for=6h")
	want := plan.Options{
		Yes: true, SkipChecks: []string{"etcd", "tasks", "guests"}, NoSilence: true, ForceStopAfter: 30 * time.Second,
		TimeoutDrain: time.Minute, TimeoutGuest: 2 * time.Minute, TimeoutMigrate: 3 * time.Minute,
		TimeoutDown: 4 * time.Minute, TimeoutUp: 5 * time.Minute, SilenceFor: 6 * time.Hour,
		// Not given on the command line: taken from cluster.rolling.settle_timeout.
		TimeoutSettle: 20 * time.Minute,
	}
	if b.target != "node-a" || !reflect.DeepEqual(b.opts, want) {
		t.Errorf("target %q, options:\n got %+v\nwant %+v", b.target, b.opts, want)
	}

	// With nothing given, the defaults -- and the config's settle timeout.
	b, _ = optsFor(t, "shutdown", "node", "node-c")
	def := plan.DefaultOptions()
	def.TimeoutSettle = 20 * time.Minute
	if b.target != "node-c" || !reflect.DeepEqual(b.opts, def) {
		t.Errorf("defaults: target %q, options %+v", b.target, b.opts)
	}

	// An explicit --timeout-settle wins over the config.
	b, _ = optsFor(t, "start", "node", "node-a", "--timeout-settle=45m")
	if b.opts.TimeoutSettle != 45*time.Minute {
		t.Errorf("--timeout-settle: %s", b.opts.TimeoutSettle)
	}
}

// `plan ...` is the same command with --dry-run forced.
func TestPlanForcesDryRun(t *testing.T) {
	for _, args := range [][]string{
		{"plan", "reboot", "node", "node-a"},
		{"plan", "maint", "enter", "node", "node-a"},
		{"reboot", "node", "node-a", "--dry-run"},
	} {
		if b, _ := optsFor(t, args...); !b.opts.DryRun || b.target != "node-a" {
			t.Errorf("%v: dry-run=%v target=%q", args, b.opts.DryRun, b.target)
		}
	}
	if b, _ := optsFor(t, "reboot", "node", "node-a"); b.opts.DryRun {
		t.Error("a plain reboot must not be a dry run")
	}
}

func TestTalosFlagsReachTheOptions(t *testing.T) {
	b, _ := optsFor(t, "upgrade", "node", "node-a", "--talos-version", "v1.13.7")
	if b.opts.TalosVersion != "v1.13.7" || b.opts.TalosImage != "" {
		t.Errorf("upgrade: %+v", b.opts)
	}
	b, _ = optsFor(t, "plan", "upgrade", "node", "node-a", "--talos-image", "factory.talos.dev/installer/abc:v1.13.7")
	if b.opts.TalosImage != "factory.talos.dev/installer/abc:v1.13.7" || !b.opts.DryRun {
		t.Errorf("plan upgrade: %+v", b.opts)
	}
}

// Mistakes on the command line are refused before any Env is built.
func TestBadCommandLinesBuildNothing(t *testing.T) {
	cases := map[string][]string{
		`unknown check "nope"; valid names: `:                        {"reboot", "node", "node-a", "--skip-check=nope"},
		"--talos-version and --talos-image are mutually exclusive":   {"upgrade", "node", "node-a", "--talos-version=v1.13.7", "--talos-image=x:v1.13.7"},
		`--talos-version "1.13" is not a release tag like v1.13.7, `: {"upgrade", "node", "node-a", "--talos-version=1.13"},
		"which node? usage: pvectl reboot node <name>":               {"reboot", "node"},
		"expected `pvectl rolling reboot cluster`":                   {"rolling", "reboot"},
	}
	for want, args := range cases {
		b, err := optsFor(t, args...)
		if err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("%v: error %v, want %s", args, err, want)
		}
		if b.target != "" {
			t.Errorf("%v: an Env was built", args)
		}
	}
}

// A rolling run marks its node operations as part of one, records the Talos
// target, and saves its progress when a node fails.
func TestRollingMarksItsNodeOperations(t *testing.T) {
	c := newCLI(t, newWorld(t))
	c.refuse = errStop
	err := c.run("rolling", "upgrade", "cluster", "--yes", "--talos-version", "v1.13.7")
	if !errors.Is(err, errStop) || !strings.HasPrefix(err.Error(), "rolling upgrade stopped at node-c: ") {
		t.Fatalf("error %v", err)
	}
	if len(c.built) != 1 || c.built[0].target != "node-c" || !c.built[0].opts.Rolling || c.built[0].opts.TalosVersion != "v1.13.7" {
		t.Errorf("env built with %+v", c.built)
	}
	prog, err := state.LoadRolling(c.stateDir)
	if err != nil || prog == nil {
		t.Fatalf("rolling.json: %v", err)
	}
	if prog.Command != rollingUpgrade || prog.TalosVersion != "v1.13.7" || len(prog.Done) != 0 ||
		!reflect.DeepEqual(prog.Order, []string{"node-c", "node-b", "node-a"}) {
		t.Errorf("rolling.json %+v", prog)
	}
	got := c.transcript()
	wantLines(t, got[:5],
		"| pvectl "+version+" -- rolling upgrade",
		"| order: node-c -> node-b -> node-a",
		"| remaining: node-c, node-b, node-a",
		"| talos: v1.13.7 (image derived from each node's machine.install.image)",
		"| ",
	)

	// A second invocation continues it, and refuses another release.
	err = c.run("rolling", "upgrade", "cluster", "--yes", "--talos-version", "v1.14.0")
	if err == nil || err.Error() != "this rolling upgrade was started with Talos v1.13.7; refusing to continue with v1.14.0 (use --restart to start over)" {
		t.Errorf("error %v", err)
	}
	// And a rolling reboot refuses to run over an unfinished rolling upgrade.
	err = c.run("rolling", "reboot", "cluster", "--yes")
	if err == nil || !strings.HasPrefix(err.Error(), "an unfinished rolling upgrade is recorded (done: ); finish it with `pvectl rolling upgrade cluster`") {
		t.Errorf("error %v", err)
	}
}
