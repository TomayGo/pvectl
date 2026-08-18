package main

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tamago/pvectl/internal/alert"
	"github.com/tamago/pvectl/internal/plan"
	"github.com/tamago/pvectl/internal/state"
	"github.com/tamago/pvectl/internal/wol"
)

// freezeFlags is the full stop used by `shutdown cluster`. Unlike a single-node
// maintenance -- which only needs noout and norebalance -- taking every node
// down means nothing should try to recover, backfill, mark OSDs down, or serve
// IO while the cluster is in pieces.
var freezeFlags = []string{"noout", "norecover", "norebalance", "nobackfill", "nodown", "pause"}

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
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] != "cluster" {
				return fmt.Errorf("expected `rolling reboot cluster`, got %q", args[0])
			}
			return runRollingReboot(cmd.Context())
		},
	}
	reboot.Flags().Bool("restart", false, "discard recorded progress and start from the first node")
	c.AddCommand(reboot)
	return c
}

func runRollingReboot(ctx context.Context) error {
	cfg, lg, err := setup()
	if err != nil {
		return err
	}
	defer lg.Close()
	self := plan.Hostname()

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
	if prog == nil {
		prog = &state.Rolling{Command: "rolling-reboot", Order: order, Holder: self, Started: time.Now()}
	} else {
		lg.Plain("continuing an earlier rolling reboot started %s (done: %s)",
			prog.Started.Format(time.RFC3339), strings.Join(prog.Done, ", "))
	}

	remaining := prog.Remaining()
	lg.Plain("pvectl %s -- rolling reboot", version)
	lg.Plain("order: %s", strings.Join(order, " -> "))
	lg.Plain("remaining: %s", strings.Join(remaining, ", "))
	lg.Plain("")

	for _, node := range remaining {
		if node == self {
			lg.Plain("")
			lg.Warnf("%s is the node running pvectl; stopping here.", node)
			lg.Plain("Every other node has been rebooted. Finish from a peer:")
			for _, other := range otherThan(order, node) {
				lg.Plain("    ssh %s 'pvectl reboot node %s --yes'", cfg.Nodes[other].LANIP, node)
			}
			lg.Plain("")
			lg.Plain("(ssh by IP, not by hostname: the tailnet name is hijacked by Tailscale SSH)")
			saveProgress(prog)
			return nil
		}
		lg.Plain("")
		lg.Plain("================ %s ================", node)
		if err := runNodeOp(ctx, node, plan.CmdReboot); err != nil {
			saveProgress(prog)
			return fmt.Errorf("rolling reboot stopped at %s: %w", node, err)
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
	lg.Plain("Rolling reboot complete: %s", strings.Join(prog.Done, ", "))
	return nil
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

	lk, _, err := lockCluster(e, "shutdown-cluster")
	if err != nil {
		return err
	}
	defer func() {
		// The lock is intentionally left in place: the cluster is off, and the
		// next thing that should happen is `pvectl start cluster`.
		_ = lk
	}()

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

	e.Log.Step("guests", "shut down Talos VMs and passthrough guests on every node")
	for _, n := range e.Cfg.NodeNames() {
		nc := e.Cfg.Nodes[n]
		if nc.Talos.VMID != 0 {
			if err := shutdownGuest(ctx, e, n, "qemu", nc.Talos.VMID); err != nil {
				return err
			}
		}
		for _, gst := range nc.StopGuests {
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

func shutdownGuest(ctx context.Context, e *plan.Env, node, kind string, vmid int) error {
	st, err := e.PVEAll.GuestStatus(ctx, node, kind, vmid)
	if err != nil {
		e.Log.Warnf("cannot read guest %d on %s: %v", vmid, node, err)
		return nil
	}
	if st.Status != "running" {
		e.Log.Infof("guest %d on %s is already %s", vmid, node, st.Status)
		return nil
	}
	graceful := e.Opts.TimeoutGuest
	if e.Opts.ForceStopAfter > 0 {
		graceful = e.Opts.ForceStopAfter
	}
	upid, err := e.PVEAll.GuestShutdown(ctx, node, kind, vmid, graceful, false)
	if err != nil {
		return fmt.Errorf("shutdown guest %d on %s: %w", vmid, node, err)
	}
	if err := e.PVEAll.WaitTask(ctx, node, upid, graceful+30*time.Second); err != nil {
		if e.Opts.ForceStopAfter <= 0 {
			return fmt.Errorf("guest %d on %s did not shut down within %s: %w (use --force-stop-after)", vmid, node, graceful, err)
		}
		e.Log.Warnf("guest %d did not stop cleanly (%v); forcing", vmid, err)
		upid, serr := e.PVEAll.GuestStop(ctx, node, kind, vmid)
		if serr != nil {
			return serr
		}
		return e.PVEAll.WaitTask(ctx, node, upid, 2*time.Minute)
	}
	e.Log.Infof("guest %d on %s stopped", vmid, node)
	return nil
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
	if err := state.ForceRelease(state.Dir); err != nil {
		e.Log.Warnf("could not release the maintenance lock: %v", err)
	}
	_ = state.RemoveState(state.Dir)
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

var _ = url.Values{}
