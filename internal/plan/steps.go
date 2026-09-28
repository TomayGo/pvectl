package plan

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tamago/pvectl/internal/alert"
	"github.com/tamago/pvectl/internal/wol"
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

func stepHAEvacuate() Step {
	return Step{
		Name: "ha-evacuate",
		Desc: "migrate HA resources off the node",
		Skip: func(ctx context.Context, e *Env) (bool, string, error) {
			ha, err := e.PVE.HAStatus(ctx)
			if err != nil {
				return false, "", err
			}
			if len(ha.ServicesOn(e.Target)) == 0 {
				return true, "no HA resources on " + e.Target, nil
			}
			return false, "", nil
		},
		Do: func(ctx context.Context, e *Env) error { return evacuateHA(ctx, e) },
		// Deliberately not reversed: moving a service back costs another
		// outage (CTs migrate offline), and nothing is safer for having been
		// put back where it started.
		Undo: nil,
	}
}

func evacuateHA(ctx context.Context, e *Env) error {
	ha, err := e.PVE.HAStatus(ctx)
	if err != nil {
		return err
	}
	svcs := ha.ServicesOn(e.Target)
	if len(svcs) == 0 {
		return nil
	}
	targets, err := e.migrationTargets(ctx)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return fmt.Errorf("no other node is available to receive HA resources")
	}
	var moved []string
	for i, s := range svcs {
		dest := targets[i%len(targets)]
		if strings.HasPrefix(s.SID, "ct:") {
			e.Log.Warnf("%s is a container: HA migration is offline, so it will be briefly unavailable", s.SID)
		}
		e.Log.Infof("migrating %s -> %s", s.SID, dest)
		if err := e.PVE.HAMigrate(ctx, s.SID, dest); err != nil {
			return fmt.Errorf("migrate %s to %s: %w", s.SID, dest, err)
		}
		moved = append(moved, s.SID)
	}
	e.State.SetData("ha-evacuate", "migrated", moved)
	if e.Opts.DryRun {
		return nil
	}
	return waitHAOff(ctx, e, e.Opts.TimeoutMigrate)
}

// waitHAOff blocks until no HA service is left on the target. Driving this
// explicitly (rather than trusting shutdown_policy=migrate) is what gives the
// operation a progress report and a timeout.
func waitHAOff(ctx context.Context, e *Env, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		ha, err := e.PVE.HAStatus(ctx)
		if err != nil {
			return err
		}
		left := ha.ServicesOn(e.Target)
		if len(left) == 0 {
			e.Log.Infof("no HA resources remain on %s", e.Target)
			return nil
		}
		if time.Now().After(deadline) {
			var names []string
			for _, s := range left {
				names = append(names, fmt.Sprintf("%s (%s)", s.SID, s.State))
			}
			return fmt.Errorf("HA resources still on %s after %s: %s", e.Target, timeout, strings.Join(names, ", "))
		}
		e.Log.Debugf("waiting for %d HA resource(s) to leave %s", len(left), e.Target)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// migrationTargets ranks the other nodes by free memory, most free first.
func (e *Env) migrationTargets(ctx context.Context) ([]string, error) {
	type cand struct {
		name string
		free int64
	}
	var cs []cand
	for _, n := range e.OtherNodes() {
		ns, err := e.PVE.NodeStatus(ctx, n)
		if err != nil {
			e.Log.Warnf("cannot read %s status, skipping as a migration target: %v", n, err)
			continue
		}
		cs = append(cs, cand{n, ns.Memory.Total - ns.Memory.Used})
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].free > cs[j].free })
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.name)
	}
	return out, nil
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

// stepRestore is the deliberate inverse of steps 3-6 plus the guests that have
// no onboot flag. It is a forward step rather than a rollback because it runs
// on the success path, after the node is back.
func stepRestore() Step {
	return Step{
		Name: "restore",
		Desc: "uncordon, clear ceph flags, start guests, lift the silence",
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
	if err := waitFor(ctx, e, deadline, "ceph HEALTH_OK and all PGs active+clean", func() (bool, string) {
		cs, err := e.PVE.CephStatus(ctx)
		if err != nil {
			return false, err.Error()
		}
		clean, detail := cs.AllPGsActiveClean()
		if cs.Health.Status != "HEALTH_OK" {
			return false, cs.HealthSummary()
		}
		return clean, detail
	}); err != nil {
		return err
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
		parts = append(parts, "HA resources evacuated")
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
