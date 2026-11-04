package plan

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/TomayGo/pvectl/internal/alert"
	"github.com/TomayGo/pvectl/internal/pve"
	"github.com/TomayGo/pvectl/internal/wol"
)

// ---------- 1. lock ----------

func stepLock() Step {
	return Step{
		Name: "lock",
		Desc: "hold the cluster-wide maintenance lock",
		Do: func(ctx context.Context, e *Env) error {
			if e.Lock == nil {
				return fmt.Errorf("internal error: the plan started without the maintenance lock")
			}
			e.Log.Infof("lock held by %s for %s (%s)", e.Lock.Info.Holder, e.Lock.Info.Target, e.Lock.Info.Command)
			return nil
		},
		Undo: func(ctx context.Context, e *Env) error {
			return e.ReleaseLock()
		},
	}
}

// ---------- 2. preflight ----------

func stepPreflight(forShutdown bool) Step {
	return Step{
		Name: "preflight",
		Desc: "read-only safety checks",
		Do: func(ctx context.Context, e *Env) error {
			r := Preflight(ctx, e, forShutdown)
			if e.State != nil && e.State.Command == CmdUpgrade {
				upgradeChecks(ctx, e, r)
			}
			r.Print(e)
			if r.Failed() {
				return fmt.Errorf("preflight failed: %s (override individually with --skip-check)",
					strings.Join(r.Failures(), ", "))
			}
			return nil
		},
	}
}

// ---------- 3. silence ----------

func stepSilence() Step {
	return Step{
		Name: "silence",
		Desc: "silence Alertmanager for the node and announce on Discord",
		Skip: func(ctx context.Context, e *Env) (bool, string, error) {
			if e.Opts.NoSilence {
				return true, "--no-silence", nil
			}
			return false, "", nil
		},
		Do: func(ctx context.Context, e *Env) error {
			m := alert.NodeMatchers(e.Target, e.Node.Talos.Name,
				[]string{e.Node.LANIP, e.Node.Talos.IP, e.Node.Talos.InternalIP})
			ids, err := e.Alert.CreateSilence(ctx, m,
				fmt.Sprintf("pvectl %s %s", e.State.Command, e.Target), e.Opts.SilenceFor)
			if err != nil {
				return err
			}
			// An abandoned earlier attempt may have left silences recorded
			// here; overwriting would strand them until they expire on their
			// own, muting the node long after it is healthy.
			e.State.SetData("silence", "ids", append(toStrings(dataOf(e, "silence", "ids")), ids...))
			// A failed Discord post must not stop a maintenance that is
			// otherwise fine to run.
			if err := e.Alert.Notify(ctx, "firing", "PvectlMaintenance",
				fmt.Sprintf("%s: %s starting on %s", e.Self, e.State.Command, e.Target),
				e.impactSummary(), map[string]string{"node": e.Target}); err != nil {
				e.Log.Warnf("%v", err)
			}
			return nil
		},
		Undo: func(ctx context.Context, e *Env) error { return undoSilence(ctx, e) },
	}
}

func undoSilence(ctx context.Context, e *Env) error {
	st := e.State.Step("silence")
	var errs []string
	if st != nil && st.Data != nil {
		if raw, ok := st.Data["ids"]; ok {
			for _, id := range toStrings(raw) {
				if err := e.Alert.DeleteSilence(ctx, id); err != nil {
					errs = append(errs, err.Error())
				}
			}
		}
	}
	if !e.Opts.NoSilence {
		if err := e.Alert.Notify(ctx, "resolved", "PvectlMaintenance",
			fmt.Sprintf("%s: maintenance finished on %s", e.Self, e.Target),
			"node is back in service", map[string]string{"node": e.Target}); err != nil {
			e.Log.Warnf("%v", err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// ---------- 4. ceph flags ----------

func stepCephFlags(flags []string) Step {
	return Step{
		Name: "ceph-flags",
		Desc: "set ceph " + strings.Join(flags, ", "),
		Do: func(ctx context.Context, e *Env) error {
			return setCephFlags(ctx, e, e.Cfg.Cluster.CephFlags)
		},
		Undo: func(ctx context.Context, e *Env) error { return undoCephFlags(ctx, e) },
	}
}

// setCephFlags records which flags were already on before we touched them, so
// the rollback only clears the ones we are responsible for.
func setCephFlags(ctx context.Context, e *Env, flags []string) error {
	before, err := e.PVE.CephFlags(ctx)
	if err != nil {
		return err
	}
	// Seed from anything an earlier attempt at this same maintenance recorded.
	// Without this, re-running a command after a failure sees its own flags as
	// pre-existing, disowns them, and restore leaves noout set forever.
	ours := toStrings(dataOf(e, "ceph-flags", "set_by_us"))
	owned := map[string]bool{}
	for _, f := range ours {
		owned[f] = true
	}
	for _, f := range flags {
		if before[f] {
			if owned[f] {
				e.Log.Infof("ceph flag %s was set by an earlier attempt at this maintenance; keeping ownership", f)
			} else {
				e.Log.Infof("ceph flag %s was already set; leaving it to whoever set it", f)
			}
			continue
		}
		if err := e.PVE.SetCephFlag(ctx, f, true); err != nil {
			return fmt.Errorf("set ceph flag %s: %w", f, err)
		}
		if !e.Opts.DryRun {
			e.Log.Infof("ceph flag %s set", f)
		}
		if !owned[f] {
			owned[f] = true
			ours = append(ours, f)
		}
	}
	e.State.SetData("ceph-flags", "set_by_us", ours)
	return nil
}

func undoCephFlags(ctx context.Context, e *Env) error {
	st := e.State.Step("ceph-flags")
	if st == nil || st.Data == nil {
		return nil
	}
	var errs []string
	for _, f := range toStrings(st.Data["set_by_us"]) {
		if err := e.PVE.SetCephFlag(ctx, f, false); err != nil {
			errs = append(errs, fmt.Sprintf("unset %s: %v", f, err))
			continue
		}
		if !e.Opts.DryRun {
			e.Log.Infof("ceph flag %s cleared", f)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// ---------- 5. HA evacuation ----------

// haStep names the HA evacuation step. restore and doctor read its recorded
// ownership of the node's HA maintenance.
const haStep = "ha-evacuate"

// haOwnedKey records that this maintenance window, not someone else, put the
// node into HA maintenance -- the same split as ceph-flags' set_by_us.
const haOwnedKey = "maintenance_set_by_us"

// haAckTimeout bounds the wait for the CRM to take a request off its queue. It
// runs every ~10 s, so this only fires when the CRM is not running at all.
const haAckTimeout = 2 * time.Minute

// stepHAEvacuate puts the node into HA maintenance and waits for the CRM to
// move its resources off.
//
// pvectl used to pick a target and migrate each resource itself. That cannot
// work with PVE 9 node-affinity rules: a strict rule refuses a node outside its
// list, and a non-strict one only lets a resource sit on the highest-priority
// online node -- so a resource moved anywhere else while the node it prefers
// is up is pulled straight back. Maintenance takes the node out of the CRM's
// online set, so the CRM itself picks a destination its rules allow, and keeps
// anything from being placed on the node (failback included) until it ends.
func stepHAEvacuate() Step {
	return Step{
		Name: haStep,
		Desc: "put the node in HA maintenance; HA moves its resources off",
		Skip: func(ctx context.Context, e *Env) (bool, string, error) {
			res, err := e.PVE.HAResources(ctx)
			if err != nil {
				return false, "", err
			}
			// With nothing configured the CRM may not even be running, and
			// there is nothing to keep off the node.
			if len(res) == 0 && !haOwned(e) {
				return true, "no HA resources are configured", nil
			}
			return false, "", nil
		},
		Do: func(ctx context.Context, e *Env) error { return evacuateHA(ctx, e) },
		// Ending maintenance is what undoes it. The CRM then moves the
		// resources that left back to this node (it records where each came
		// from) -- PVE's behaviour, and the only one a preferring rule allows.
		Undo:          func(ctx context.Context, e *Env) error { return leaveHAMaintenance(ctx, e) },
		UndoIfStarted: true,
	}
}

// haOwned reports whether this window put the node into HA maintenance.
func haOwned(e *Env) bool {
	v, _ := dataOf(e, haStep, haOwnedKey).(bool)
	return v
}

func evacuateHA(ctx context.Context, e *Env) error {
	ms, err := e.PVE.HAManagerStatus(ctx)
	if err != nil {
		return err
	}
	if ms.Disarmed() {
		return fmt.Errorf("the HA stack is disarmed, and the CRM ignores maintenance requests while it is; re-arm it (ha-manager arm-ha) and `pvectl resume`")
	}
	active := ms.ActiveServicesOn(e.Target)
	switch {
	case ms.MaintenanceRequested(e.Target) || ms.NodeState(e.Target) == "maintenance":
		if haOwned(e) {
			e.Log.Infof("%s is already in HA maintenance from an earlier attempt at this maintenance; keeping ownership", e.Target)
		} else {
			e.Log.Infof("%s is already in HA maintenance; leaving it to whoever put it there", e.Target)
		}
	default:
		for _, s := range active {
			if strings.HasPrefix(s.SID, "ct:") {
				e.Log.Warnf("%s is a container: HA relocates it offline, so it will be briefly unavailable", s.SID)
			}
		}
		if len(active) > 0 {
			e.Log.Infof("HA will move %s where its rules allow", strings.Join(sids(active), ", "))
		}
		if err := e.PVE.SetHANodeMaintenance(ctx, e.Target, true); err != nil {
			return err
		}
		// Persist ownership before waiting: if this run dies now, abort and
		// restore still know the maintenance is theirs to end.
		e.State.SetData(haStep, haOwnedKey, true)
		if err := e.State.Save(); err != nil {
			e.Log.Warnf("could not persist HA maintenance ownership: %v", err)
		}
	}
	moved := toStrings(dataOf(e, haStep, "migrated"))
	for _, s := range active {
		if !contains(moved, s.SID) {
			moved = append(moved, s.SID)
		}
	}
	e.State.SetData(haStep, "migrated", moved)
	if e.Opts.DryRun {
		return nil
	}
	return waitHAEvacuated(ctx, e, e.Opts.TimeoutMigrate, moved)
}

// waitHAEvacuated blocks until the CRM has the node in maintenance and no
// active resource is left on it. Waiting here, rather than trusting
// shutdown_policy, is what gives the operation a progress report and a
// timeout.
func waitHAEvacuated(ctx context.Context, e *Env, timeout time.Duration, moved []string) error {
	deadline := time.Now().Add(timeout)
	for {
		ms, err := e.PVE.HAManagerStatus(ctx)
		if err != nil {
			return err
		}
		left := ms.ActiveServicesOn(e.Target)
		state := ms.NodeState(e.Target)
		if state == "maintenance" && len(left) == 0 {
			e.Log.Infof("%s is in HA maintenance; no active HA resources remain on it", e.Target)
			for _, s := range ms.Services() {
				if contains(moved, s.SID) {
					e.Log.Infof("  %s is on %s (%s)", s.SID, s.Node, s.State)
				}
			}
			return nil
		}
		if time.Now().After(deadline) {
			var names []string
			for _, s := range left {
				d := fmt.Sprintf("%s (%s)", s.SID, s.State)
				if s.Target != "" && s.Target != s.Node {
					d = fmt.Sprintf("%s (%s %s -> %s)", s.SID, s.State, s.Node, s.Target)
				}
				names = append(names, d)
			}
			detail := "none"
			if len(names) > 0 {
				detail = strings.Join(names, ", ")
			}
			return fmt.Errorf("after %s %s is in HA state %q (maintenance requested: %v), active resources still on it: %s"+
				" -- a strict node-affinity rule with no other available node keeps a resource in place (see the ha-affinity preflight check);"+
				" `pvectl resume` keeps waiting, `pvectl abort` ends the maintenance",
				timeout, e.Target, state, ms.MaintenanceRequested(e.Target), detail)
		}
		e.Log.Debugf("waiting for HA: %s is %q, %d active resource(s) left", e.Target, state, len(left))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// leaveHAMaintenance ends the node's HA maintenance if this window started it,
// and waits for the CRM to take the request. It does not wait for the
// resources to return: the CRM moves them back on its own, and nothing about
// the node's health depends on where they run.
func leaveHAMaintenance(ctx context.Context, e *Env) error {
	if !haOwned(e) {
		return nil
	}
	if err := e.PVE.SetHANodeMaintenance(ctx, e.Target, false); err != nil {
		return err
	}
	if !e.Opts.DryRun {
		deadline := time.Now().Add(haAckTimeout)
		for {
			ms, err := e.PVE.HAManagerStatus(ctx)
			if err != nil {
				return err
			}
			if !ms.MaintenanceRequested(e.Target) {
				e.Log.Infof("HA maintenance ended on %s (CRM state %q)", e.Target, ms.NodeState(e.Target))
				if back := ms.ReturningTo(e.Target); len(back) > 0 {
					e.Log.Infof("HA moves %s back to %s", strings.Join(back, ", "), e.Target)
				}
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("the CRM has not taken the request to end maintenance on %s after %s; check `ha-manager status`", e.Target, haAckTimeout)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
			}
		}
	}
	e.State.SetData(haStep, haOwnedKey, false)
	return nil
}

func sids(ss []pve.HAService) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, s.SID)
	}
	return out
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// ---------- 6. k8s drain ----------

func stepK8sDrain() Step {
	return Step{
		Name: "k8s-drain",
		Desc: "cordon and drain the Talos node",
		Do: func(ctx context.Context, e *Env) error {
			name := e.Node.Talos.Name
			if err := e.K8s.SetSchedulable(ctx, name, false); err != nil {
				return err
			}
			return e.K8s.Drain(ctx, name, e.Opts.TimeoutDrain)
		},
		Undo: func(ctx context.Context, e *Env) error { return undoDrain(ctx, e) },
	}
}

func undoDrain(ctx context.Context, e *Env) error {
	return e.K8s.SetSchedulable(ctx, e.Node.Talos.Name, true)
}

// ---------- 7. talos stop ----------

func stepTalosStop() Step {
	return Step{
		Name: "talos-stop",
		Desc: "shut down the Talos control-plane VM",
		Skip: func(ctx context.Context, e *Env) (bool, string, error) {
			if e.Opts.DryRun {
				return false, "", nil
			}
			alive, err := e.Talos.Alive(ctx, e.Node.Talos.IP)
			if err != nil {
				// Not "already down": we could not ask. Skipping here would
				// hand a live control-plane node to host-poweroff.
				return false, "", fmt.Errorf("cannot determine whether %s is running: %w", e.Node.Talos.Name, err)
			}
			if !alive {
				return true, e.Node.Talos.Name + " is already down", nil
			}
			return false, "", nil
		},
		Do: func(ctx context.Context, e *Env) error {
			if err := e.Talos.Shutdown(ctx, e.Node.Talos.IP); err != nil {
				return err
			}
			return e.Talos.WaitDown(ctx, e.Node.Talos.IP, e.Opts.TimeoutDown)
		},
		Undo: func(ctx context.Context, e *Env) error {
			if e.Node.Talos.VMID == 0 {
				return fmt.Errorf("no talos vmid configured for %s; start it by hand", e.Target)
			}
			upid, err := e.PVE.GuestStart(ctx, e.Target, "qemu", e.Node.Talos.VMID)
			if err != nil {
				return err
			}
			return e.PVE.WaitTask(ctx, e.Target, upid, 2*time.Minute)
		},
	}
}

// ---------- 8. guest stop ----------

func stepGuestStop() Step {
	return Step{
		Name: "guest-stop",
		Desc: "shut down passthrough guests that cannot migrate",
		Skip: func(ctx context.Context, e *Env) (bool, string, error) {
			if len(e.Node.StopGuests) == 0 {
				return true, "no guests configured for " + e.Target, nil
			}
			return false, "", nil
		},
		Do: func(ctx context.Context, e *Env) error { return stopGuests(ctx, e) },
		Undo: func(ctx context.Context, e *Env) error {
			return startGuests(ctx, e, toInts(dataOf(e, "guest-stop", "stopped")))
		},
	}
}

func stopGuests(ctx context.Context, e *Env) error {
	live, err := e.PVEAll.GuestsOn(ctx, e.Target)
	if err != nil {
		return fmt.Errorf("list guests on %s: %w", e.Target, err)
	}
	var stopped []int
	for _, g := range e.Node.StopGuests {
		lr, ok := live[g.ID]
		if !ok {
			e.Log.Warnf("guest %d is not on %s; skipping", g.ID, e.Target)
			continue
		}
		if lr.Status != "running" {
			e.Log.Infof("guest %d %s is already %s", g.ID, lr.Name, lr.Status)
			continue
		}
		if g.Warn != "" {
			e.Log.Warnf("stopping %d %s: %s", g.ID, lr.Name, g.Warn)
		}
		kind := lr.Kind()
		// --force-stop-after names the deadline, so it is the deadline: give
		// the guest that long to shut down cleanly, then pull the plug. With
		// the flag unset we wait --timeout-guest and fail rather than force.
		graceful := e.Opts.TimeoutGuest
		if e.Opts.ForceStopAfter > 0 {
			graceful = e.Opts.ForceStopAfter
		}
		upid, err := e.PVEAll.GuestShutdown(ctx, e.Target, kind, g.ID, graceful, false)
		if err != nil {
			return fmt.Errorf("shutdown guest %d: %w", g.ID, err)
		}
		if err := e.PVEAll.WaitTask(ctx, e.Target, upid, graceful+30*time.Second); err != nil {
			if e.Opts.ForceStopAfter <= 0 {
				return fmt.Errorf("guest %d did not shut down within %s: %w (use --force-stop-after to hard-stop)", g.ID, graceful, err)
			}
			e.Log.Warnf("guest %d did not shut down cleanly (%v); forcing stop", g.ID, err)
			upid, serr := e.PVEAll.GuestStop(ctx, e.Target, kind, g.ID)
			if serr != nil {
				return fmt.Errorf("force stop guest %d: %w", g.ID, serr)
			}
			if err := e.PVEAll.WaitTask(ctx, e.Target, upid, 2*time.Minute); err != nil {
				return fmt.Errorf("force stop guest %d: %w", g.ID, err)
			}
		}
		e.Log.Infof("guest %d %s stopped", g.ID, lr.Name)
		stopped = append(stopped, g.ID)
	}
	e.State.SetData("guest-stop", "stopped", stopped)
	return nil
}

func startGuests(ctx context.Context, e *Env, ids []int) error {
	var errs []string
	for _, id := range ids {
		kind := "qemu"
		for _, g := range e.Node.StopGuests {
			if g.ID == id {
				kind = g.Kind()
			}
		}
		// PVE rejects a start on a running guest, so without this a restore
		// that failed halfway can never be resumed: the guests it already
		// started would fail the retry every time.
		if st, err := e.PVE.GuestStatus(ctx, e.Target, kind, id); err == nil && st.Status == "running" {
			e.Log.Infof("guest %d is already running", id)
			continue
		}
		upid, err := e.PVE.GuestStart(ctx, e.Target, kind, id)
		if err != nil {
			errs = append(errs, fmt.Sprintf("start %d: %v", id, err))
			continue
		}
		if err := e.PVE.WaitTask(ctx, e.Target, upid, 3*time.Minute); err != nil {
			errs = append(errs, fmt.Sprintf("start %d: %v", id, err))
			continue
		}
		e.Log.Infof("guest %d started", id)
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// ---------- 9. host power ----------

func stepHostPower(command string) Step {
	desc := "power off the host"
	if command == "reboot" {
		desc = "reboot the host"
	}
	return Step{
		Name:         "host-" + command,
		Desc:         desc,
		Irreversible: true,
		Do: func(ctx context.Context, e *Env) error {
			// Record uptime first: if the host reboots faster than wait-down
			// polls, a lower uptime afterwards is the only remaining proof
			// that it actually went down.
			if ns, err := e.PVE.NodeStatus(ctx, e.Target); err == nil {
				e.State.SetData("host-"+command, "uptime_before", ns.Uptime)
			} else {
				e.Log.Debugf("could not record %s uptime before %s: %v", e.Target, command, err)
			}
			// This must go through another node's pveproxy; e.PVE already has
			// the target excluded.
			e.Log.Infof("sending %s to %s via %v", command, e.Target, e.PVE.Endpoints())
			return e.PVE.NodeCommand(ctx, e.Target, command)
		},
	}
}

// ---------- 10. wait down ----------

func stepWaitDown(powerStep string) Step {
	return Step{
		Name: "wait-down",
		Desc: "wait for the host to leave the cluster",
		Do: func(ctx context.Context, e *Env) error {
			if e.Opts.DryRun {
				e.Log.Infof("(dry-run) would wait for %s to go offline", e.Target)
				return nil
			}
			// Three signals, because a reboot can be quick enough to slip
			// between two polls of any single one:
			//   - corosync marks the node offline
			//   - the node's own API stops answering for several probes
			//   - the node answers again but with a lower uptime than the one
			//     recorded just before the command, which is proof it went
			//     down and came back
			before := int64(-1)
			if v, ok := dataOf(e, powerStep, "uptime_before").(float64); ok {
				before = int64(v)
			}
			deadline := time.Now().Add(e.Opts.TimeoutDown)
			unreachable := 0
			const unreachableProbes = 3
			for {
				if st, err := e.PVE.ClusterStatus(ctx); err == nil {
					if n, ok := st.Nodes[e.Target]; ok && n.Online != 1 {
						e.Log.Infof("%s is offline", e.Target)
						return nil
					}
				} else {
					e.Log.Debugf("cluster status unavailable while waiting: %v", err)
				}
				if e.PVE.Reachable(ctx, e.Node.LANIP) {
					unreachable = 0
					if before > 0 {
						if ns, err := e.PVE.NodeStatus(ctx, e.Target); err == nil && ns.Uptime < before {
							e.Log.Infof("%s has already rebooted (uptime %ds, was %ds)", e.Target, ns.Uptime, before)
							return nil
						}
					}
				} else {
					unreachable++
					e.Log.Debugf("%s did not answer on %s (%d/%d)", e.Target, e.Node.LANIP, unreachable, unreachableProbes)
					if unreachable >= unreachableProbes {
						e.Log.Infof("%s stopped answering on %s", e.Target, e.Node.LANIP)
						return nil
					}
				}
				if time.Now().After(deadline) {
					return fmt.Errorf("%s still online after %s", e.Target, e.Opts.TimeoutDown)
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(5 * time.Second):
				}
			}
		},
	}
}

// ---------- 11. wake ----------

func stepWake() Step {
	return Step{
		Name: "wake",
		Desc: "send Wake-on-LAN to both bond slaves",
		Skip: func(ctx context.Context, e *Env) (bool, string, error) {
			if e.Opts.DryRun {
				return false, "", nil
			}
			if e.PVE.Reachable(ctx, e.Node.LANIP) {
				return true, e.Target + " is already up", nil
			}
			return false, "", nil
		},
		Do: func(ctx context.Context, e *Env) error {
			return wol.Wake(e.Node.WOLMAC, e.Node.LANIP, e.Log, e.Opts.DryRun)
		},
	}
}

// ---------- 12. wait up ----------

func stepWaitUp() Step {
	return Step{
		Name: "wait-up",
		Desc: "wait for corosync, Ceph OSD, Talos and kubelet to come back",
		Do: func(ctx context.Context, e *Env) error {
			if e.Opts.DryRun {
				e.Log.Infof("(dry-run) would wait for %s to come back", e.Target)
				return nil
			}
			deadline := time.Now().Add(e.Opts.TimeoutUp)
			if err := waitFor(ctx, e, deadline, "corosync membership", func() (bool, string) {
				st, err := e.PVE.ClusterStatus(ctx)
				if err != nil {
					return false, err.Error()
				}
				n, ok := st.Nodes[e.Target]
				return ok && n.Online == 1, "not yet online"
			}); err != nil {
				return err
			}
			if err := waitFor(ctx, e, deadline, "ceph OSDs up+in", func() (bool, string) {
				cs, err := e.PVE.CephStatus(ctx)
				if err != nil {
					return false, err.Error()
				}
				return cs.OSDMap.NumUpOSDs == cs.OSDMap.NumOSDs && cs.OSDMap.NumInOSDs == cs.OSDMap.NumOSDs,
					fmt.Sprintf("%d/%d up, %d/%d in", cs.OSDMap.NumUpOSDs, cs.OSDMap.NumOSDs, cs.OSDMap.NumInOSDs, cs.OSDMap.NumOSDs)
			}); err != nil {
				return err
			}
			// The Talos VM has onboot set, but it still has to boot; wait for
			// its API before asking Kubernetes about the kubelet.
			if err := waitFor(ctx, e, deadline, "talos API", func() (bool, string) {
				alive, err := e.Talos.Alive(ctx, e.Node.Talos.IP)
				if err != nil {
					return false, err.Error()
				}
				return alive, "no answer from " + e.Node.Talos.IP
			}); err != nil {
				return err
			}
			left := time.Until(deadline)
			if left < time.Minute {
				left = time.Minute
			}
			return e.K8s.WaitReady(ctx, e.Node.Talos.Name, left)
		},
	}
}

func waitFor(ctx context.Context, e *Env, deadline time.Time, what string, probe func() (bool, string)) error {
	for {
		ok, detail := probe()
		if ok {
			e.Log.Infof("%s: ready", what)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s (%s)", what, detail)
		}
		e.Log.Debugf("waiting for %s: %s", what, detail)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

// ---------- 13. restore ----------

// restoreHAMaintenance ends the HA maintenance this window started. One it did
// not start is reported and left alone, like a ceph flag someone else set.
func restoreHAMaintenance(ctx context.Context, e *Env) error {
	if haOwned(e) {
		return leaveHAMaintenance(ctx, e)
	}
	if dataOf(e, haStep, haOwnedKey) != nil {
		return nil // recorded, and already ended
	}
	ms, err := e.PVE.HAManagerStatus(ctx)
	if err != nil {
		e.Log.Warnf("cannot read HA manager status: %v", err)
		return nil
	}
	if ms.MaintenanceRequested(e.Target) || ms.NodeState(e.Target) == "maintenance" {
		e.Log.Warnf("%s is in HA maintenance, but this maintenance has no record of starting it; leaving it alone -- end it with `ha-manager crm-command node-maintenance disable %s` or `pvectl doctor --fix`",
			e.Target, e.Target)
	}
	return nil
}

// stepRestore is the deliberate inverse of steps 3-6 plus the guests that have
// no onboot flag. It is a forward step rather than a rollback because it runs
// on the success path, after the node is back.
func stepRestore() Step {
	return Step{
		Name: "restore",
		Desc: "uncordon, clear ceph flags, end HA maintenance, start guests, lift the silence",
		Do: func(ctx context.Context, e *Env) error {
			var errs []string
			if err := undoDrain(ctx, e); err != nil {
				errs = append(errs, fmt.Sprintf("uncordon: %v", err))
			}
			if st := e.State.Step("ceph-flags"); st == nil || st.Data["set_by_us"] == nil {
				e.Log.Warnf("no record of which ceph flags this maintenance set; leaving them alone -- check `pvectl status` and clear with `pvectl doctor --fix` if needed")
			}
			if err := undoCephFlags(ctx, e); err != nil {
				errs = append(errs, fmt.Sprintf("ceph flags: %v", err))
			}
			if err := restoreHAMaintenance(ctx, e); err != nil {
				errs = append(errs, fmt.Sprintf("ha maintenance: %v", err))
			}
			if len(e.Node.StartAfterBoot) > 0 {
				e.Log.Infof("starting guests without onboot: %v", e.Node.StartAfterBoot)
				if err := startGuests(ctx, e, e.Node.StartAfterBoot); err != nil {
					errs = append(errs, fmt.Sprintf("start_after_boot: %v", err))
				}
			}
			// Not gated on --no-silence: that flag says "do not create
			// silences or notify", and the silences being expired here were
			// created by the command that opened the window, not this one.
			if err := undoSilence(ctx, e); err != nil {
				errs = append(errs, fmt.Sprintf("silence: %v", err))
			}
			if len(errs) > 0 {
				return fmt.Errorf("%s", strings.Join(errs, "; "))
			}
			return nil
		},
	}
}

// ---------- 14. verify ----------

func stepVerify() Step {
	return Step{
		Name: "verify",
		Desc: "confirm the cluster is fully healthy again",
		Do: func(ctx context.Context, e *Env) error {
			if e.Opts.DryRun {
				e.Log.Infof("(dry-run) would verify ceph, quorum and kubernetes")
				return nil
			}
			return Settle(ctx, e, e.Opts.TimeoutSettle)
		},
	}
}

// Settle waits until every "is the cluster whole again" condition holds. This
// is both the verify step and the gate between nodes of a rolling reboot.
func Settle(ctx context.Context, e *Env, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	if err := waitFor(ctx, e, deadline, "corosync quorum", func() (bool, string) {
		st, err := e.PVE.ClusterStatus(ctx)
		if err != nil {
			return false, err.Error()
		}
		return st.Quorate && len(st.Online) == len(st.Nodes),
			fmt.Sprintf("quorate=%v, %d/%d online", st.Quorate, len(st.Online), len(st.Nodes))
	}); err != nil {
		return err
	}
	var ignored []string
	if err := waitFor(ctx, e, deadline, "ceph HEALTH_OK and all PGs active+clean", func() (bool, string) {
		cs, err := e.PVE.CephStatus(ctx)
		if err != nil {
			return false, err.Error()
		}
		clean, detail := cs.AllPGsActiveClean()
		var ok bool
		if ok, ignored = cs.HealthGate(e.Cfg.Cluster.CephIgnoreHealth); !ok {
			return false, cs.HealthSummary()
		}
		return clean, detail
	}); err != nil {
		return err
	}
	if len(ignored) > 0 {
		// Said every time, so an ignored check can never become invisible.
		e.Log.Warnf("ceph is not HEALTH_OK, but only for ignored checks (ceph_ignore_health): %s", strings.Join(ignored, ", "))
	}
	return waitFor(ctx, e, deadline, "all kubernetes nodes Ready", func() (bool, string) {
		ok, bad, err := e.K8s.AllReady(ctx)
		if err != nil {
			return false, err.Error()
		}
		return ok, strings.Join(bad, ", ")
	})
}

// ---------- helpers ----------

func dataOf(e *Env, step, key string) any {
	st := e.State.Step(step)
	if st == nil || st.Data == nil {
		return nil
	}
	return st.Data[key]
}

// toStrings and toInts undo the round trip through JSON, where a []string
// saved into state.json comes back as []any.
func toStrings(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		var out []string
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func toInts(v any) []int {
	switch t := v.(type) {
	case []int:
		return t
	case []any:
		var out []int
		for _, e := range t {
			switch n := e.(type) {
			case float64:
				out = append(out, int(n))
			case int:
				out = append(out, n)
			}
		}
		return out
	}
	return nil
}

// impactSummary is the human-facing "what will break" line for the Discord
// announcement. It follows the command: maint enter drains and evacuates but
// stops nothing and powers nothing off.
func (e *Env) impactSummary() string {
	var parts []string
	if e.State != nil && e.State.Command == CmdMaintEnter {
		parts = append(parts, fmt.Sprintf("%s cordoned and drained", e.Node.Talos.Name))
		parts = append(parts, "HA maintenance (resources moved off)")
		parts = append(parts, "guests keep running, host stays powered")
		return strings.Join(parts, "; ")
	}
	for _, g := range e.Node.StopGuests {
		if g.Warn != "" {
			parts = append(parts, fmt.Sprintf("VM %d: %s", g.ID, g.Warn))
		}
	}
	parts = append(parts, "ceph goes degraded (size=3/min_size=2, IO continues)")
	parts = append(parts, fmt.Sprintf("kubernetes loses %s", e.Node.Talos.Name))
	if e.State != nil && e.State.Command == CmdUpgrade {
		what := "host packages upgraded"
		if v := dataString(e, "talos-upgrade", "requested_version") + dataString(e, "talos-upgrade", "requested_image"); v != "" {
			what += ", Talos -> " + v
		}
		parts = append(parts, what)
	}
	return strings.Join(parts, "; ")
}
