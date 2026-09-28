package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tamago/pvectl/internal/alert"
	"github.com/tamago/pvectl/internal/plan"
	"github.com/tamago/pvectl/internal/state"
	"github.com/tamago/pvectl/internal/talos"
	"github.com/tamago/pvectl/internal/wol"
)

// freezeFlags is the full stop used by `shutdown cluster`. Unlike a single-node
// maintenance -- which only needs noout and norebalance -- taking every node
// down means nothing should try to recover, backfill, mark OSDs down, or serve
// IO while the cluster is in pieces.
var freezeFlags = []string{"noout", "norecover", "norebalance", "nobackfill", "nodown", "pause"}

// Values of Rolling.Command. rolling-reboot predates the upgrade and is what
// existing rolling.json files say, so it must not change.
const (
	rollingReboot  = "rolling-reboot"
	rollingUpgrade = "rolling-upgrade"
)

func rollingWhat(command string) string {
	if command == rollingUpgrade {
		return "rolling upgrade"
	}
	return "rolling reboot"
}

func newRollingCmd() *cobra.Command {
	c := &cobra.Command{Use: "rolling", Short: "Cluster-wide sequential operations"}
	reboot := &cobra.Command{
		Use:   "reboot cluster",
		Short: "Reboot every node one at a time, waiting for full health in between",
		Long: strings.TrimSpace(`
Reboots nodes in the configured order, running the complete single-node reboot
plan for each and waiting for Ceph HEALTH_OK with every PG active+clean,
corosync quorum and all kubernetes nodes Ready before starting the next.

It cannot reboot the node it is running on -- the process would die before the
restore steps -- so it stops there and prints the command to run from a peer.
Progress is recorded, so re-running it continues where it left off.`),
		Args:              rollingArgs,
		ValidArgsFunction: completeCluster,
		RunE: func(cmd *cobra.Command, args []string) error {
			restart, err := cmd.Flags().GetBool("restart")
			if err != nil {
				return err
			}
			return runRolling(cmd.Context(), plan.CmdReboot, restart)
		},
	}
	reboot.Flags().Bool("restart", false, "discard recorded progress and start from the first node")

	upgrade := &cobra.Command{
		Use:   "upgrade cluster",
		Short: "Upgrade and reboot every node one at a time (apt full-upgrade, optionally Talos)",
		Long: strings.TrimSpace(`
Runs the single-node upgrade plan on each node in the configured order: drain,
apt-get update && full-upgrade on the host, the new Talos release installed
into the Talos VM (with --talos-version), one reboot that boots both, restore,
and a full-health wait before the next node.

--talos-version takes a tag, "latest" (newest stable release) or
"latest-patch" (newest patch of the minor the cluster runs). An alias is
resolved to a tag once, at the start, and the tag is recorded with the
progress, so a continuation upgrades the remaining nodes to the same release;
passing a different tag then is an error rather than a mixed cluster. Like rolling reboot, it stops at the node it
is running on and prints the command to finish from a peer.

Kubernetes itself (talosctl upgrade-k8s) is not part of this.`),
		Args:              rollingArgs,
		ValidArgsFunction: completeCluster,
		RunE: func(cmd *cobra.Command, args []string) error {
			restart, err := cmd.Flags().GetBool("restart")
			if err != nil {
				return err
			}
			return runRolling(cmd.Context(), plan.CmdUpgrade, restart)
		},
	}
	upgrade.Flags().Bool("restart", false, "discard recorded progress and start from the first node")
	withTalosFlags(upgrade)

	c.AddCommand(reboot, upgrade)
	return c
}

func rollingArgs(cmd *cobra.Command, args []string) error {
	if len(args) != 1 || args[0] != "cluster" {
		return fmt.Errorf("expected `%s cluster`", cmd.CommandPath())
	}
	return nil
}

// runRolling applies a single-node command (reboot or upgrade) to every node in
// turn. Each node gets the full plan, verify included, so the next node only
// starts once the cluster is whole again.
func runRolling(ctx context.Context, command string, restart bool) error {
	cfg, lg, err := setup()
	if err != nil {
		return err
	}
	defer lg.Close()
	self := plan.Hostname()
	g.opts.Rolling = true
	kind := rollingReboot
	if command == plan.CmdUpgrade {
		kind = rollingUpgrade
	}

	order := cfg.RollingOrder()
	saveProgress := func(p *state.Rolling) {
		if g.opts.DryRun {
			return
		}
		if err := p.Save(state.Dir); err != nil {
			lg.Warnf("could not record progress: %v", err)
		}
	}
	prog, err := state.LoadRolling(state.Dir)
	if err != nil {
		lg.Warnf("%v", err)
		prog = nil
	}
	if restart && prog != nil {
		lg.Plain("--restart: discarding recorded progress of the %s (%s)", rollingWhat(prog.Command), strings.Join(prog.Done, ", "))
		prog = nil
	}
	if prog != nil && prog.Command != kind {
		return fmt.Errorf("an unfinished %s is recorded (done: %s); finish it with `pvectl %s`, or discard it with --restart",
			rollingWhat(prog.Command), strings.Join(prog.Done, ", "), rollingWhat(prog.Command)+" cluster")
	}
	fresh := prog == nil
	if prog == nil && command == plan.CmdUpgrade {
		if err := resolveTalosAlias(ctx, cfg, lg); err != nil {
			return err
		}
	}
	if prog == nil {
		prog = &state.Rolling{Command: kind, Order: order, Holder: self, Started: time.Now(),
			TalosVersion: g.opts.TalosVersion, TalosImage: g.opts.TalosImage}
	} else {
		lg.Plain("continuing an earlier %s started %s (done: %s)",
			rollingWhat(kind), prog.Started.Format(time.RFC3339), strings.Join(prog.Done, ", "))
		if err := adoptRecordedTalosTarget(prog); err != nil {
			return err
		}
	}

	remaining := prog.Remaining()
	lg.Plain("pvectl %s -- %s", version, rollingWhat(kind))
	lg.Plain("order: %s", strings.Join(order, " -> "))
	lg.Plain("remaining: %s", strings.Join(remaining, ", "))
	if command == plan.CmdUpgrade {
		lg.Plain("talos: %s", talosTargetLabel())
	}
	lg.Plain("")

	for _, node := range remaining {
		if node == self {
			lg.Plain("")
			lg.Warnf("%s is the node running pvectl; stopping here.", node)
			lg.Plain("Every other node is done. Finish from a peer:")
			for _, other := range otherThan(order, node) {
				lg.Plain("    ssh %s 'pvectl %s node %s%s --yes'", cfg.Nodes[other].LANIP, command, node, talosFlagsFor(command))
			}
			lg.Plain("")
			lg.Plain("(ssh by IP, not by hostname: the tailnet name is hijacked by Tailscale SSH)")
			saveProgress(prog)
			return nil
		}
		lg.Plain("")
		lg.Plain("================ %s ================", node)
		if err := runNodeOp(ctx, node, command, true); err != nil {
			// Declining the first node's confirmation means this rolling run
			// never began. Recording it would make the next invocation
			// "continue" it -- with whatever Talos target this one had --
			// instead of starting the run the operator actually asks for.
			if fresh && len(prog.Done) == 0 && errors.Is(err, errNotStarted) {
				return err
			}
			saveProgress(prog)
			return fmt.Errorf("%s stopped at %s: %w", rollingWhat(kind), node, err)
		}
		prog.Done = append(prog.Done, node)
		saveProgress(prog)
	}
	if !g.opts.DryRun {
		if err := state.RemoveRolling(state.Dir); err != nil {
			lg.Warnf("could not clear rolling.json: %v", err)
		}
	}
	lg.Plain("")
	lg.Plain("%s complete: %s", rollingWhat(kind), strings.Join(prog.Done, ", "))
	return nil
}

// adoptRecordedTalosTarget makes a continued rolling upgrade use the release it
// started with. Omitting the flag on the second invocation is the normal case
// and inherits it; naming a different one is refused.
func adoptRecordedTalosTarget(prog *state.Rolling) error {
	if prog.Command != rollingUpgrade {
		return nil
	}
	// An alias on the continuation means "whatever this run is upgrading to";
	// re-resolving it could name a release published since, and refusing it
	// would make the obvious re-run of the same command line fail. But a run
	// that was recorded without any Talos target cannot satisfy it: silently
	// continuing host-only is exactly what the operator did not ask for.
	if talos.IsAlias(g.opts.TalosVersion) {
		if prog.TalosVersion == "" && prog.TalosImage == "" {
			return fmt.Errorf("this rolling upgrade was started without a Talos target; --talos-version %s "+
				"cannot join it (finish it host-only, or use --restart)", g.opts.TalosVersion)
		}
		g.opts.TalosVersion = ""
	}
	given := g.opts.TalosVersion != "" || g.opts.TalosImage != ""
	if given && (g.opts.TalosVersion != prog.TalosVersion || g.opts.TalosImage != prog.TalosImage) {
		return fmt.Errorf("this rolling upgrade was started with Talos %s; refusing to continue with %s (use --restart to start over)",
			orNone(prog.TalosVersion+prog.TalosImage), g.opts.TalosVersion+g.opts.TalosImage)
	}
	g.opts.TalosVersion, g.opts.TalosImage = prog.TalosVersion, prog.TalosImage
	return nil
}

func talosTargetLabel() string {
	switch {
	case g.opts.TalosVersion != "":
		return g.opts.TalosVersion + " (image derived from each node's machine.install.image)"
	case g.opts.TalosImage != "":
		return g.opts.TalosImage
	}
	return "not upgraded (no --talos-version)"
}

// talosFlagsFor renders the Talos target for the hand-off command, so the last
// node is upgraded to the same release as the others.
func talosFlagsFor(command string) string {
	if command != plan.CmdUpgrade {
		return ""
	}
	switch {
	case g.opts.TalosVersion != "":
		return " --talos-version " + g.opts.TalosVersion
	case g.opts.TalosImage != "":
		return " --talos-image " + g.opts.TalosImage
	}
	return ""
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func otherThan(list []string, v string) []string {
	var out []string
	for _, s := range list {
		if s != v {
			out = append(out, s)
		}
	}
	return out
}

// ---------- shutdown cluster ----------

func shutdownClusterCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cluster",
		Short: "Stop every node (full Ceph freeze)",
		Long: strings.TrimSpace(`
Freezes Ceph completely (noout norecover norebalance nobackfill nodown pause),
shuts down every Talos VM and passthrough guest, then powers off the hosts --
peers first, the node running pvectl last.

There is no drain and no HA evacuation: with the whole cluster going down there
is nowhere to move anything to.`),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error { return runClusterShutdown(cmd.Context()) },
	}
}

func runClusterShutdown(ctx context.Context) error {
	e, lg, err := newEnv("")
	if err != nil {
		return err
	}
	defer lg.Close()
	lg.Plain("pvectl %s -- shutdown cluster (from %s)", version, e.Self)
	if e.Opts.DryRun {
		lg.Plain("DRY RUN: nothing will be changed.")
	}
	lg.Plain("")

	r := plan.Preflight(ctx, e, false)
	r.Print(e)
	if r.Failed() {
		return fmt.Errorf("preflight failed: %s", strings.Join(r.Failures(), ", "))
	}
	lg.Plain("")
	lg.Plain("This stops the entire cluster: every VM, every container, all storage.")
	for _, n := range e.Cfg.NodeNames() {
		for _, gst := range e.Cfg.Nodes[n].StopGuests {
			if gst.Warn != "" {
				lg.Plain("  - %s VM %d: %s", n, gst.ID, gst.Warn)
			}
		}
	}
	if err := e.Confirm(ctx, "About to shut down every node."); err != nil {
		return err
	}

	// The lock is taken and deliberately never released here: the cluster ends
	// up off, and `pvectl start cluster` is what closes the window.
	if _, _, err := lockCluster(e, "shutdown-cluster"); err != nil {
		return err
	}

	if !e.Opts.NoSilence {
		e.Log.Step("silence", "silence Alertmanager cluster-wide")
		m := []alert.Matcher{{Name: "alertname", Value: ".+", IsRegex: true, IsEqual: true}}
		if _, err := e.Alert.CreateSilence(ctx, m, "pvectl shutdown cluster", e.Opts.SilenceFor); err != nil {
			e.Log.Warnf("could not silence Alertmanager: %v", err)
		}
		if err := e.Alert.Notify(ctx, "firing", "PvectlClusterShutdown",
			fmt.Sprintf("%s: shutting down the whole cluster", e.Self),
			"every node is going down; ceph is fully frozen", nil); err != nil {
			e.Log.Warnf("%v", err)
		}
	}

	e.Log.Step("ceph-freeze", "set "+strings.Join(freezeFlags, " "))
	for _, f := range freezeFlags {
		if err := e.PVE.SetCephFlag(ctx, f, true); err != nil {
			return fmt.Errorf("set ceph flag %s: %w", f, err)
		}
		e.Log.Infof("ceph flag %s set", f)
	}

	// Every Talos shutdown is issued before any of them is waited on. Stopping
	// them one at a time means the third one is asked to shut down gracefully
	// after etcd has already lost quorum, so it blocks on an unreachable
	// control plane, blows past the timeout, and aborts the whole run with
	// Ceph frozen and half the cluster down.
	e.Log.Step("talos", "shut down every Talos control-plane VM together")
	type pending struct {
		node string
		vmid int
		upid string
	}
	var inflight []pending
	for _, n := range e.Cfg.NodeNames() {
		nc := e.Cfg.Nodes[n]
		if nc.Talos.VMID == 0 {
			continue
		}
		upid, running, err := requestGuestShutdown(ctx, e, n, "qemu", nc.Talos.VMID)
		if err != nil {
			return err
		}
		if running {
			inflight = append(inflight, pending{n, nc.Talos.VMID, upid})
		}
	}
	for _, p := range inflight {
		if err := awaitGuestShutdown(ctx, e, p.node, "qemu", p.vmid, p.upid); err != nil {
			return err
		}
	}

	e.Log.Step("guests", "shut down passthrough guests on every node")
	for _, n := range e.Cfg.NodeNames() {
		for _, gst := range e.Cfg.Nodes[n].StopGuests {
			if err := shutdownGuest(ctx, e, n, gst.Kind(), gst.ID); err != nil {
				return err
			}
		}
	}

	e.Log.Step("poweroff", "power off the hosts, this node last")
	order := append(otherThan(e.Cfg.NodeNames(), e.Self), e.Self)
	for _, n := range order {
		e.Log.Infof("powering off %s", n)
		cl := e.PVEAll.Excluded(e.Cfg.Nodes[n].LANIP)
		if n == e.Self || len(cl.Endpoints()) == 0 {
			// Nothing else is left to proxy through; talk to the local
			// pveproxy and accept that the reply will not arrive.
			cl = e.PVEAll
		}
		if err := cl.NodeCommand(ctx, n, "shutdown"); err != nil {
			if n == e.Self {
				e.Log.Infof("connection to %s dropped, as expected while it powers off", n)
				continue
			}
			return fmt.Errorf("shutdown %s: %w", n, err)
		}
	}
	lg.Plain("")
	lg.Plain("All nodes are going down. Bring them back with `pvectl start cluster` from any node once one is up.")
	return nil
}

func gracefulWindow(e *plan.Env) time.Duration {
	if e.Opts.ForceStopAfter > 0 {
		return e.Opts.ForceStopAfter
	}
	return e.Opts.TimeoutGuest
}

// requestGuestShutdown sends the ACPI shutdown and returns its worker UPID.
// running is false when there was nothing to stop.
func requestGuestShutdown(ctx context.Context, e *plan.Env, node, kind string, vmid int) (string, bool, error) {
	st, err := e.PVEAll.GuestStatus(ctx, node, kind, vmid)
	if err != nil {
		e.Log.Warnf("cannot read guest %d on %s: %v", vmid, node, err)
		return "", false, nil
	}
	if st.Status != "running" {
		e.Log.Infof("guest %d on %s is already %s", vmid, node, st.Status)
		return "", false, nil
	}
	upid, err := e.PVEAll.GuestShutdown(ctx, node, kind, vmid, gracefulWindow(e), false)
	if err != nil {
		return "", false, fmt.Errorf("shutdown guest %d on %s: %w", vmid, node, err)
	}
	return upid, true, nil
}

func awaitGuestShutdown(ctx context.Context, e *plan.Env, node, kind string, vmid int, upid string) error {
	graceful := gracefulWindow(e)
	if err := e.PVEAll.WaitTask(ctx, node, upid, graceful+30*time.Second); err != nil {
		if e.Opts.ForceStopAfter <= 0 {
			return fmt.Errorf("guest %d on %s did not shut down within %s: %w (use --force-stop-after)", vmid, node, graceful, err)
		}
		e.Log.Warnf("guest %d did not stop cleanly (%v); forcing", vmid, err)
		supid, serr := e.PVEAll.GuestStop(ctx, node, kind, vmid)
		if serr != nil {
			return serr
		}
		return e.PVEAll.WaitTask(ctx, node, supid, 2*time.Minute)
	}
	e.Log.Infof("guest %d on %s stopped", vmid, node)
	return nil
}

func shutdownGuest(ctx context.Context, e *plan.Env, node, kind string, vmid int) error {
	upid, running, err := requestGuestShutdown(ctx, e, node, kind, vmid)
	if err != nil || !running {
		return err
	}
	return awaitGuestShutdown(ctx, e, node, kind, vmid, upid)
}

// ---------- start cluster ----------

func startClusterCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cluster",
		Short: "Wake every node and lift the freeze",
		Long: strings.TrimSpace(`
Wakes every node that is not already up, waits for corosync, Ceph and
kubernetes, then clears the freeze flags -- pause last, so IO only resumes once
the OSDs are back and the cluster can serve it.`),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error { return runClusterStart(cmd.Context()) },
	}
}

func runClusterStart(ctx context.Context) error {
	e, lg, err := newEnv("")
	if err != nil {
		return err
	}
	defer lg.Close()
	lg.Plain("pvectl %s -- start cluster (from %s)", version, e.Self)
	if e.Opts.DryRun {
		lg.Plain("DRY RUN: nothing will be changed.")
	}

	// Take the lock before touching anything. AcquireOrAdopt refuses a lock
	// held for a single node, which is what stops `start cluster` from
	// bulldozing a `shutdown node pve-2` window that is still open.
	lk, adopted, err := lockCluster(e, "start-cluster")
	if err != nil {
		return err
	}
	if adopted {
		lg.Plain("adopting the cluster maintenance lock held since %s", lk.Info.Started.Format(time.RFC3339))
	}

	e.Log.Step("wake", "send Wake-on-LAN to every node that is down")
	for _, n := range e.Cfg.NodeNames() {
		nc := e.Cfg.Nodes[n]
		if n == e.Self {
			continue
		}
		if e.PVEAll.Reachable(ctx, nc.LANIP) {
			e.Log.Infof("%s is already up", n)
			continue
		}
		if err := wol.Wake(nc.WOLMAC, nc.LANIP, e.Log, e.Opts.DryRun); err != nil {
			e.Log.Warnf("wake %s: %v", n, err)
		}
	}

	e.Log.Step("wait-up", "wait for every node to rejoin")
	if !e.Opts.DryRun {
		deadline := time.Now().Add(e.Opts.TimeoutUp)
		for {
			st, err := e.PVEAll.ClusterStatus(ctx)
			if err == nil && st.Quorate && len(st.Online) == len(st.Nodes) {
				e.Log.Infof("all %d nodes online and quorate", len(st.Online))
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("not every node came back within %s", e.Opts.TimeoutUp)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(10 * time.Second):
			}
		}
	}

	e.Log.Step("ceph-thaw", "clear the freeze flags, pause last")
	// pause is cleared last on purpose: everything else has to be able to
	// recover before client IO is allowed back in.
	var ordered []string
	for _, f := range freezeFlags {
		if f != "pause" {
			ordered = append(ordered, f)
		}
	}
	ordered = append(ordered, "pause")
	for _, f := range ordered {
		if f == "pause" && !e.Opts.DryRun {
			e.Log.Infof("waiting for OSDs to come back before clearing pause")
			if err := waitOSDs(ctx, e); err != nil {
				return err
			}
		}
		if err := e.PVE.SetCephFlag(ctx, f, false); err != nil {
			return fmt.Errorf("clear ceph flag %s: %w", f, err)
		}
		e.Log.Infof("ceph flag %s cleared", f)
	}

	e.Log.Step("start-guests", "start guests that have no onboot flag")
	for _, n := range e.Cfg.NodeNames() {
		for _, id := range e.Cfg.Nodes[n].StartAfterBoot {
			kind := "qemu"
			for _, gst := range e.Cfg.Nodes[n].StopGuests {
				if gst.ID == id {
					kind = gst.Kind()
				}
			}
			upid, err := e.PVEAll.GuestStart(ctx, n, kind, id)
			if err != nil {
				e.Log.Warnf("start guest %d on %s: %v", id, n, err)
				continue
			}
			if err := e.PVEAll.WaitTask(ctx, n, upid, 3*time.Minute); err != nil {
				e.Log.Warnf("start guest %d on %s: %v", id, n, err)
				continue
			}
			e.Log.Infof("guest %d started on %s", id, n)
		}
	}

	e.Log.Step("verify", "wait for full health")
	if !e.Opts.DryRun {
		if err := plan.Settle(ctx, e, e.Opts.TimeoutSettle); err != nil {
			return err
		}
	}
	if !e.Opts.NoSilence {
		if sil, err := e.Alert.OwnSilences(ctx); err == nil {
			for _, s := range sil {
				if err := e.Alert.DeleteSilence(ctx, s.ID); err != nil {
					e.Log.Warnf("%v", err)
				}
			}
		}
		if err := e.Alert.Notify(ctx, "resolved", "PvectlClusterShutdown",
			fmt.Sprintf("%s: cluster is back", e.Self), "all nodes online, ceph healthy", nil); err != nil {
			e.Log.Warnf("%v", err)
		}
	}
	if err := lk.Release(); err != nil {
		e.Log.Warnf("could not release the maintenance lock: %v", err)
	}
	lg.Plain("")
	lg.Plain("Cluster is back in service.")
	return nil
}

func waitOSDs(ctx context.Context, e *plan.Env) error {
	deadline := time.Now().Add(e.Opts.TimeoutUp)
	for {
		cs, err := e.PVE.CephStatus(ctx)
		if err == nil && cs.OSDMap.NumUpOSDs == cs.OSDMap.NumOSDs && cs.OSDMap.NumInOSDs == cs.OSDMap.NumOSDs {
			e.Log.Infof("%d/%d OSDs up and in", cs.OSDMap.NumUpOSDs, cs.OSDMap.NumOSDs)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("OSDs did not all come back within %s", e.Opts.TimeoutUp)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

func lockCluster(e *plan.Env, command string) (*state.Lock, bool, error) {
	if e.Opts.DryRun {
		return &state.Lock{}, false, nil
	}
	return state.AcquireOrAdopt(state.Dir, e.Self, "cluster", command)
}
