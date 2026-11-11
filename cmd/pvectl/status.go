package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/TomayGo/pvectl/internal/alert"
	plog "github.com/TomayGo/pvectl/internal/log"
	"github.com/TomayGo/pvectl/internal/plan"
	"github.com/TomayGo/pvectl/internal/state"
)

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show maintenance state, leftover flags, cordons and HA placement",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			e, lg, err := newEnv("")
			if err != nil {
				return err
			}
			defer lg.Close()
			return showStatus(cmd.Context(), e)
		},
	}
}

func showStatus(ctx context.Context, e *plan.Env) error {
	lg := e.Log
	lg.Plain("pvectl status (from %s)", e.Self)
	lg.Plain("")

	held, err := state.Peek(e.StateDir)
	switch {
	case err != nil:
		lg.Plain("maintenance lock : unreadable (%v)", err)
	case held == nil:
		lg.Plain("maintenance lock : free")
	default:
		lg.Plain("maintenance lock : HELD for %s -- %s %s, since %s (%s ago), holder %s pid %d",
			held.Target, held.Command, held.Target,
			held.Started.Format(time.RFC3339), time.Since(held.Started).Round(time.Second),
			held.Holder, held.PID)
	}

	st, serr := state.LoadState(e.StateDir)
	if serr != nil {
		lg.Plain("run state        : unreadable (%v)", serr)
	} else if st == nil {
		lg.Plain("run state        : none")
	} else {
		lg.Plain("run state        : %s", st.Summary())
		for _, s := range st.Steps {
			mark := " "
			switch s.Status {
			case state.StepDone:
				mark = "+"
			case state.StepFailed:
				mark = "x"
			case state.StepSkipped:
				mark = "-"
			case state.StepUndone:
				mark = "~"
			}
			detail := string(s.Status)
			if s.Error != "" {
				detail += ": " + s.Error
			}
			lg.Plain("    %s %-14s %s", mark, s.Name, detail)
		}
		if next := st.FirstIncomplete(); next != "" {
			lg.Plain("    next step: %s (`pvectl resume` continues, `pvectl abort` rolls back)", next)
		}
	}
	if roll, err := state.LoadRolling(e.StateDir); err == nil && roll != nil {
		lg.Plain("%-17s: done %s; remaining %s", rollingWhat(roll.Command),
			strings.Join(roll.Done, ", "), strings.Join(roll.Remaining(), ", "))
	}
	lg.Plain("")

	cs, err := e.PVE.ClusterStatus(ctx)
	if err != nil {
		lg.Plain("cluster          : unreachable (%v)", err)
	} else {
		lg.Plain("cluster          : %s, quorate=%v, online %s", cs.Name, cs.Quorate, strings.Join(cs.Online, " "))
		if len(cs.Offline) > 0 {
			lg.Plain("                   OFFLINE: %s", strings.Join(cs.Offline, " "))
		}
	}

	if ceph, err := e.PVE.CephStatus(ctx); err != nil {
		lg.Plain("ceph             : unreachable (%v)", err)
	} else {
		_, pg := ceph.AllPGsActiveClean()
		lg.Plain("ceph             : %s, %d/%d OSDs up, %s",
			ceph.Health.Status, ceph.OSDMap.NumUpOSDs, ceph.OSDMap.NumOSDs, pg)
	}

	leftovers := 0
	if flags, err := e.PVE.CephFlags(ctx); err != nil {
		lg.Plain("ceph flags       : unreadable (%v)", err)
	} else {
		var set []string
		for n, on := range flags {
			if on {
				set = append(set, n)
			}
		}
		sort.Strings(set)
		if len(set) == 0 {
			lg.Plain("ceph flags       : none set")
		} else {
			// Only the flags a node maintenance sets are a leftover that
			// `doctor --fix` clears; the hint below must not promise more.
			if ours, _ := plan.SplitCephFlags(e.Cfg.Cluster.CephFlags, flags); len(ours) > 0 {
				leftovers++
			}
			lg.Plain("ceph flags       : SET %s", strings.Join(set, " "))
		}
	}

	if nodes, err := e.K8s.Nodes(ctx); err != nil {
		lg.Plain("kubernetes       : unreachable (%v)", err)
	} else {
		var parts []string
		for _, n := range nodes {
			s := n.Name
			if !n.Ready {
				s += "(NotReady)"
			}
			if n.Unschedulable {
				s += "(cordoned)"
				leftovers++
			}
			parts = append(parts, s)
		}
		lg.Plain("kubernetes       : %s", strings.Join(parts, " "))
	}

	if ha, err := e.PVE.HAStatus(ctx); err != nil {
		lg.Plain("ha               : unreadable (%v)", err)
	} else {
		byNode := map[string][]string{}
		for _, s := range ha.Services {
			byNode[s.Node] = append(byNode[s.Node], s.SID)
		}
		var parts []string
		for _, n := range e.Cfg.NodeNames() {
			parts = append(parts, fmt.Sprintf("%s=%d", n, len(byNode[n])))
		}
		lg.Plain("ha               : master %s, placement %s", ha.Master, strings.Join(parts, " "))
		// Placement is the CRM's: node-affinity rules and failback decide it,
		// and it returns what a maintenance moved away once that ends.
		for _, n := range e.Cfg.NodeNames() {
			if len(byNode[n]) > 0 {
				lg.Plain("                   %s: %s", n, strings.Join(byNode[n], " "))
			}
		}
	}
	if ms, err := e.PVE.HAManagerStatus(ctx); err != nil {
		lg.Plain("ha maintenance   : unreadable (%v)", err)
	} else if nodes := ms.MaintenanceNodes(); len(nodes) == 0 {
		lg.Plain("ha maintenance   : none")
	} else {
		leftovers++
		lg.Plain("ha maintenance   : %s", strings.Join(nodes, " "))
	}

	if sil, err := e.Alert.OwnSilences(ctx); err != nil {
		lg.Plain("silences         : unreadable (%v)", err)
	} else if len(sil) == 0 {
		lg.Plain("silences         : none created by pvectl")
	} else {
		leftovers++
		lg.Plain("silences         : %d active pvectl silence(s)", len(sil))
		for _, s := range sil {
			lg.Plain("                   %s %s (until %s)", s.ID, matcherSummary(s.Matchers), s.EndsAt.Format(time.RFC3339))
		}
	}

	lg.Plain("")
	if leftovers > 0 && held == nil {
		lg.Plain("There are leftovers with no maintenance in progress. `pvectl doctor --fix` clears them.")
	}
	return nil
}

func matcherSummary(ms []alert.Matcher) string {
	var parts []string
	for _, m := range ms {
		op := "="
		if m.IsRegex {
			op = "=~"
		}
		parts = append(parts, m.Name+op+m.Value)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// ---------- resume / abort ----------

func newResumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resume",
		Short: "Continue the interrupted run recorded in state.json",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := state.LoadState(g.stateDir)
			if err != nil {
				return err
			}
			if st == nil {
				return fmt.Errorf("no interrupted run to resume")
			}
			if st.Finished {
				return fmt.Errorf("the recorded %s %s run already finished", st.Command, st.Target)
			}
			e, lg, err := newEnv(st.Target)
			if err != nil {
				return err
			}
			defer lg.Close()
			lg.Plain("resuming %s %s from step %s", st.Command, st.Target, st.FirstIncomplete())
			if err := acquireLock(e, st.Command); err != nil {
				return err
			}
			r, err := plan.ResumeRunner(e, st)
			if err != nil {
				return err
			}
			e.State.SetDryRun(e.Opts.DryRun)
			if err := e.Confirm(cmd.Context(), fmt.Sprintf("About to resume %s %s.", st.Command, st.Target)); err != nil {
				return err
			}
			start := time.Now()
			if err := r.Run(cmd.Context()); err != nil {
				return err
			}
			// A rolling reboot that was rescued with `resume` still has to
			// close its entry, or the next roll reboots this node again.
			closeRun(e, st.Command, st.Target, false)
			lg.Plain("")
			lg.Plain("resume completed in %s.", elapsed(start))
			return nil
		},
	}
}

func newAbortCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "abort",
		Short: "Roll back the interrupted run recorded in state.json",
		Long: strings.TrimSpace(`
Walks the completed steps backwards and undoes each one: uncordon, end the HA
node maintenance pvectl started (HA then moves the resources back), clear the
ceph flags pvectl set, restart guests it stopped, expire its silences and
release the lock. The HA maintenance is ended even when the evacuation itself
was what failed.

Steps that cannot be undone -- a host that has already been powered off -- are
reported rather than silently skipped, because the operator still has to deal
with them.`),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := state.LoadState(g.stateDir)
			if err != nil {
				return err
			}
			if st == nil {
				return fmt.Errorf("no run to abort")
			}
			e, lg, err := newEnv(st.Target)
			if err != nil {
				return err
			}
			defer lg.Close()
			r, err := plan.ResumeRunner(e, st)
			if err != nil {
				return err
			}
			e.State.SetDryRun(e.Opts.DryRun)
			// Rollback must be able to release the lock even though the
			// original command wanted to keep it.
			e.KeepLock = false
			if held, _ := state.Peek(e.StateDir); held != nil {
				e.Lock = &state.Lock{Info: *held}
				lg.Plain("lock held for %s by %s", held.Target, held.Holder)
			}
			lg.Plain("rolling back %s %s (%d completed step(s))", st.Command, st.Target, len(st.CompletedSteps()))
			if err := e.Confirm(cmd.Context(), fmt.Sprintf("About to roll back %s %s.", st.Command, st.Target)); err != nil {
				return err
			}
			rbErr := r.Rollback(cmd.Context())
			if !e.Opts.DryRun {
				if err := state.ForceRelease(e.StateDir); err != nil {
					lg.Warnf("could not release the lock: %v", err)
				}
				if err := state.RemoveState(e.StateDir); err != nil {
					lg.Warnf("could not clear state.json: %v", err)
				}
			}
			if rbErr != nil {
				return fmt.Errorf("rollback finished with errors: %w", rbErr)
			}
			lg.Plain("")
			lg.Plain("Rollback complete.")
			return nil
		},
	}
}

// ---------- doctor ----------

func newDoctorCmd() *cobra.Command {
	var fix bool
	c := &cobra.Command{
		Use:   "doctor",
		Short: "Find and optionally clear leftovers from an interrupted maintenance",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			e, lg, err := newEnv("")
			if err != nil {
				return err
			}
			defer lg.Close()
			return runDoctor(cmd.Context(), e, fix)
		},
	}
	c.Flags().BoolVar(&fix, "fix", false, "clear what is found instead of only reporting it")
	return c
}

// cephFlagSetter is the one PVE call doctor needs to clear a flag.
type cephFlagSetter interface {
	SetCephFlag(ctx context.Context, flag string, on bool) error
}

// doctorCephFlags reports the ceph flags that are set and, with fix, clears
// the ones a node maintenance sets (cluster.ceph_flags). It returns how many
// issues it found that --fix addresses.
//
// Any other flag is reported and left alone. doctor cannot tell a leftover
// from a decision: nodown or pause set by hand, or by `shutdown cluster`
// whose `start cluster` has not run yet, is there on purpose, and clearing it
// lets Ceph start marking OSDs down or serving IO in the middle of someone
// else's work.
func doctorCephFlags(ctx context.Context, lg *plog.Logger, pc cephFlagSetter, managed []string, flags map[string]bool, fix bool) int {
	ours, others := plan.SplitCephFlags(managed, flags)
	if len(ours) == 0 && len(others) == 0 {
		lg.Plain("[ok]   ceph flags   : none set")
		return 0
	}
	found := 0
	if len(ours) > 0 {
		found++
		lg.Plain("[warn] ceph flags   : %s still set", strings.Join(ours, " "))
		if fix {
			for _, f := range ours {
				if err := pc.SetCephFlag(ctx, f, false); err != nil {
					lg.Plain("       -> clearing %s failed: %v", f, err)
					continue
				}
				lg.Plain("       -> cleared %s", f)
			}
		}
	}
	if len(others) > 0 {
		lg.Plain("[warn] ceph flags   : %s set, but not by a node maintenance (cluster.ceph_flags is %s); leaving %s alone",
			strings.Join(others, " "), strings.Join(managed, " "), map[bool]string{true: "it", false: "them"}[len(others) == 1])
		lg.Plain("       `pvectl start cluster` lifts a cluster freeze; `ceph osd unset <flag>` clears one by hand")
	}
	return found
}

// leftover is one kind of thing an interrupted maintenance leaves behind:
// how doctor finds it, how it words the finding, and how --fix clears it.
type leftover struct {
	// label is the left-hand column, padded to the width of the report.
	label string
	// find lists what is left over; nothing means clean.
	find func(ctx context.Context) ([]string, error)
	// found words the warning from what find returned.
	found func(items []string) string
	// clean is what to say when nothing is left; "" says nothing.
	clean string
	// uncounted keeps the finding out of the "N issue(s)" total.
	uncounted bool
	// fix clears one item. nil means doctor can only report it.
	fix func(ctx context.Context, item string) error
	// doing and done word the attempt and its success; %s is the item.
	doing, done string
}

// report prints the finding, applies the fix when asked, and returns how many
// issues it adds to the total.
func (l leftover) report(ctx context.Context, lg *plog.Logger, fix bool) int {
	items, err := l.find(ctx)
	if err != nil {
		lg.Plain("[fail] %s: %v", l.label, err)
		return 0
	}
	if len(items) == 0 {
		if l.clean != "" {
			lg.Plain("[ok]   %s: %s", l.label, l.clean)
		}
		return 0
	}
	lg.Plain("[warn] %s: %s", l.label, l.found(items))
	if fix && l.fix != nil {
		for _, item := range items {
			if err := l.fix(ctx, item); err != nil {
				lg.Plain("       -> %s failed: %v", strings.Replace(l.doing, "%s", item, 1), err)
				continue
			}
			lg.Plain("       -> %s", strings.Replace(l.done, "%s", item, 1))
		}
	}
	if l.uncounted {
		return 0
	}
	return 1
}

// doctorLeftovers lists what doctor looks for after the ceph flags, in the
// order it reports them.
func doctorLeftovers(e *plan.Env, held *state.LockInfo, st *state.State, staleLock, inProgress bool) []leftover {
	one := func(present bool) func(context.Context) ([]string, error) {
		return func(context.Context) ([]string, error) {
			if present {
				return []string{""}, nil
			}
			return nil, nil
		}
	}
	return []leftover{
		{
			label: "kubernetes   ",
			find:  e.K8s.Cordoned,
			found: func(nodes []string) string { return "cordoned: " + strings.Join(nodes, " ") },
			clean: "no node is cordoned",
			fix:   func(ctx context.Context, n string) error { return e.K8s.SetSchedulable(ctx, n, true) },
			doing: "uncordon %s", done: "uncordoned %s",
		},
		{
			label: "silences     ",
			find: func(ctx context.Context) ([]string, error) {
				sil, err := e.Alert.OwnSilences(ctx)
				ids := make([]string, 0, len(sil))
				for _, s := range sil {
					ids = append(ids, s.ID)
				}
				return ids, err
			},
			found: func(ids []string) string { return fmt.Sprintf("%d pvectl silence(s) still active", len(ids)) },
			clean: "none",
			fix:   e.Alert.DeleteSilence,
			doing: "expiring %s", done: "expired %s",
		},
		{
			// HA node maintenance left on. An open run owns its own and ends
			// it in restore or abort; with none open, nothing else will.
			label: "ha maint.    ",
			find: func(ctx context.Context) ([]string, error) {
				ms, err := e.PVE.HAManagerStatus(ctx)
				if err != nil {
					return nil, err
				}
				return ms.MaintenanceNodes(), nil
			},
			found: func(nodes []string) string { return strings.Join(nodes, " ") + " in HA maintenance" },
			clean: "no node in HA maintenance",
			fix:   func(ctx context.Context, n string) error { return e.PVE.SetHANodeMaintenance(ctx, n, false) },
			doing: "ending maintenance on %s", done: "maintenance end requested for %s (HA moves its resources back)",
		},
		{
			// a lock with no run behind it
			label: "lock         ",
			find:  one(staleLock),
			found: func([]string) string {
				return fmt.Sprintf("held by %s for %s with no matching run", held.Holder, held.Target)
			},
			fix:   func(context.Context, string) error { return state.ForceRelease(e.StateDir) },
			doing: "release", done: "released",
		},
		{
			label: "state.json   ",
			find:  one(!inProgress && st != nil && st.Finished),
			found: func([]string) string {
				return fmt.Sprintf("a finished %s %s run is still recorded", st.Command, st.Target)
			},
			uncounted: true,
			fix:       func(context.Context, string) error { return state.RemoveState(e.StateDir) },
			doing:     "remove", done: "removed",
		},
		{
			// Offline nodes are reported, not fixed: starting one is a
			// maintenance of its own. A cluster status that cannot be read
			// is passed over without a line.
			label: "cluster      ",
			find: func(ctx context.Context) ([]string, error) {
				cs, err := e.PVE.ClusterStatus(ctx)
				if err != nil {
					return nil, nil
				}
				return cs.Offline, nil
			},
			found: func(nodes []string) string {
				return "offline node(s): " + strings.Join(nodes, " ") + " -- start with `pvectl start node <name>`"
			},
		},
	}
}

func runDoctor(ctx context.Context, e *plan.Env, fix bool) error {
	lg := e.Log
	lg.Plain("pvectl doctor (from %s)%s", e.Self, map[bool]string{true: " --fix", false: ""}[fix])
	lg.Plain("")

	held, _ := state.Peek(e.StateDir)
	st, _ := state.LoadState(e.StateDir)
	// A lock paired with a run for the same node is an open maintenance
	// window, even when that run finished: `shutdown node X` leaves the node
	// off and the lock held until `start node X` closes it. A lock with no run
	// behind it is a leftover from a crash between Acquire and the first state
	// write, and clearing that is exactly what doctor is for.
	staleLock := held != nil && (st == nil || st.Target != held.Target)
	inProgress := (held != nil && !staleLock) || (st != nil && !st.Finished)
	if inProgress {
		lg.Plain("A maintenance is in progress; doctor will report but not touch anything.")
		if held != nil {
			lg.Plain("  lock: %s", held)
		}
		if st != nil {
			lg.Plain("  state: %s", st.Summary())
			lg.Plain("  use `pvectl resume` or `pvectl abort` -- not doctor -- while a run is open.")
		}
		lg.Plain("")
		fix = false
	}

	found := 0

	// leftover ceph flags
	if flags, err := e.PVE.CephFlags(ctx); err != nil {
		lg.Plain("[fail] ceph flags   : %v", err)
	} else {
		found += doctorCephFlags(ctx, lg, e.PVE, e.Cfg.Cluster.CephFlags, flags, fix)
	}

	for _, l := range doctorLeftovers(e, held, st, staleLock, inProgress) {
		found += l.report(ctx, lg, fix)
	}

	lg.Plain("")
	switch {
	case found == 0:
		lg.Plain("Nothing to clean up.")
	case fix:
		lg.Plain("%d issue(s) addressed. Re-run `pvectl doctor` to confirm.", found)
	default:
		lg.Plain("%d issue(s) found. Re-run with --fix to clear them.", found)
	}
	return nil
}
