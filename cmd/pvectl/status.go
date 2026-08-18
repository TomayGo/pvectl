package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tamago/pvectl/internal/alert"
	"github.com/tamago/pvectl/internal/plan"
	"github.com/tamago/pvectl/internal/state"
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

	held, err := state.Peek(state.Dir)
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

	st, serr := state.LoadState(state.Dir)
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
	if roll, err := state.LoadRolling(state.Dir); err == nil && roll != nil {
		lg.Plain("rolling reboot   : done %s; remaining %s",
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
			leftovers++
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
		// HA resources are never moved back after a maintenance -- moving a
		// container costs an outage. Any imbalance is reported, not corrected.
		for _, n := range e.Cfg.NodeNames() {
			if len(byNode[n]) > 0 {
				lg.Plain("                   %s: %s", n, strings.Join(byNode[n], " "))
			}
		}
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
			st, err := state.LoadState(state.Dir)
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
			if err := e.ReleaseLock(); err != nil {
				lg.Warnf("%v", err)
			}
			if !plan.KeepsLockOnSuccess(st.Command) && !e.Opts.DryRun {
				_ = state.RemoveState(state.Dir)
			}
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
Walks the completed steps backwards and undoes each one: uncordon, clear the
ceph flags pvectl set, restart guests it stopped, expire its silences and
release the lock.

Steps that cannot be undone -- a host that has already been powered off -- are
reported rather than silently skipped, because the operator still has to deal
with them.`),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := state.LoadState(state.Dir)
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
			if held, _ := state.Peek(state.Dir); held != nil {
				e.Lock = &state.Lock{Info: *held}
				lg.Plain("lock held for %s by %s", held.Target, held.Holder)
			}
			lg.Plain("rolling back %s %s (%d completed step(s))", st.Command, st.Target, len(st.CompletedSteps()))
			if err := e.Confirm(cmd.Context(), fmt.Sprintf("About to roll back %s %s.", st.Command, st.Target)); err != nil {
				return err
			}
			rbErr := r.Rollback(cmd.Context())
			if !e.Opts.DryRun {
				if err := state.ForceRelease(state.Dir); err != nil {
					lg.Warnf("could not release the lock: %v", err)
				}
				if err := state.RemoveState(state.Dir); err != nil {
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

func runDoctor(ctx context.Context, e *plan.Env, fix bool) error {
	lg := e.Log
	lg.Plain("pvectl doctor (from %s)%s", e.Self, map[bool]string{true: " --fix", false: ""}[fix])
	lg.Plain("")

	inProgress := false
	held, _ := state.Peek(state.Dir)
	st, _ := state.LoadState(state.Dir)
	if held != nil || (st != nil && !st.Finished) {
		inProgress = true
	}
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
		var set []string
		for n, on := range flags {
			if on {
				set = append(set, n)
			}
		}
		sort.Strings(set)
		if len(set) > 0 {
			found++
			lg.Plain("[warn] ceph flags   : %s still set", strings.Join(set, " "))
			if fix {
				for _, f := range set {
					if err := e.PVE.SetCephFlag(ctx, f, false); err != nil {
						lg.Plain("       -> clearing %s failed: %v", f, err)
						continue
					}
					lg.Plain("       -> cleared %s", f)
				}
			}
		} else {
			lg.Plain("[ok]   ceph flags   : none set")
		}
	}

	// cordoned kubernetes nodes
	if cordoned, err := e.K8s.Cordoned(ctx); err != nil {
		lg.Plain("[fail] kubernetes   : %v", err)
	} else if len(cordoned) > 0 {
		found++
		lg.Plain("[warn] kubernetes   : cordoned: %s", strings.Join(cordoned, " "))
		if fix {
			for _, n := range cordoned {
				if err := e.K8s.SetSchedulable(ctx, n, true); err != nil {
					lg.Plain("       -> uncordon %s failed: %v", n, err)
					continue
				}
				lg.Plain("       -> uncordoned %s", n)
			}
		}
	} else {
		lg.Plain("[ok]   kubernetes   : no node is cordoned")
	}

	// pvectl silences
	if sil, err := e.Alert.OwnSilences(ctx); err != nil {
		lg.Plain("[fail] silences     : %v", err)
	} else if len(sil) > 0 {
		found++
		lg.Plain("[warn] silences     : %d pvectl silence(s) still active", len(sil))
		if fix {
			for _, s := range sil {
				if err := e.Alert.DeleteSilence(ctx, s.ID); err != nil {
					lg.Plain("       -> expiring %s failed: %v", s.ID, err)
					continue
				}
				lg.Plain("       -> expired %s", s.ID)
			}
		}
	} else {
		lg.Plain("[ok]   silences     : none")
	}

	// stale lock with a finished run
	if !inProgress && held != nil {
		found++
		lg.Plain("[warn] lock         : held by %s with no run in progress", held.Holder)
		if fix {
			if err := state.ForceRelease(state.Dir); err != nil {
				lg.Plain("       -> release failed: %v", err)
			} else {
				lg.Plain("       -> released")
			}
		}
	}
	if !inProgress && st != nil && st.Finished {
		lg.Plain("[warn] state.json   : a finished %s %s run is still recorded", st.Command, st.Target)
		if fix {
			if err := state.RemoveState(state.Dir); err != nil {
				lg.Plain("       -> remove failed: %v", err)
			} else {
				lg.Plain("       -> removed")
			}
		}
	}

	// offline nodes
	if cs, err := e.PVE.ClusterStatus(ctx); err == nil && len(cs.Offline) > 0 {
		found++
		lg.Plain("[warn] cluster      : offline node(s): %s -- start with `pvectl start node <name>`",
			strings.Join(cs.Offline, " "))
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

var _ = context.Background
