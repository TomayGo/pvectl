package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tamago/pvectl/internal/plan"
	"github.com/tamago/pvectl/internal/state"
)

func newPreflightCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "preflight [node]",
		Short: "Run the safety checks and stop (changes nothing)",
		Long: strings.TrimSpace(`
Runs every read-only check that a shutdown would run. With a node name it also
runs the node-specific ones: drain preview, HA capacity on the remaining nodes,
running backups, passthrough guests and whether your own connection depends on
the node you are asking about.`),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := ""
			if len(args) == 1 {
				target = args[0]
			}
			e, lg, err := newEnv(target)
			if err != nil {
				return err
			}
			defer lg.Close()
			if target != "" {
				e.Log.Plain("preflight for %s (running on %s)\n", target, e.Self)
			} else {
				e.Log.Plain("cluster-wide preflight (running on %s)\n", e.Self)
			}
			r := plan.Preflight(cmd.Context(), e, target != "")
			r.Print(e)
			e.Log.Plain("")
			if r.Failed() {
				return fmt.Errorf("preflight failed: %s", strings.Join(r.Failures(), ", "))
			}
			e.Log.Plain("All checks passed.")
			return nil
		},
	}
}

// newPlanCmd mirrors the mutating commands but forces --dry-run, so that
// `plan` and the real thing cannot describe different sequences.
func newPlanCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "plan",
		Short: "Show the plan and its impact without changing anything",
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			g.opts.DryRun = true
		},
	}
	c.AddCommand(
		nodeOpCmd("shutdown", plan.CmdShutdown, "Plan a node shutdown"),
		nodeOpCmd("reboot", plan.CmdReboot, "Plan a node reboot"),
		nodeOpCmd("start", plan.CmdStart, "Plan a node start"),
	)
	maint := &cobra.Command{Use: "maint", Short: "Plan entering or leaving maintenance"}
	maint.AddCommand(
		nodeOpCmd("enter", plan.CmdMaintEnter, "Plan entering maintenance"),
		nodeOpCmd("exit", plan.CmdMaintExit, "Plan leaving maintenance"),
	)
	c.AddCommand(maint)
	return c
}

func newShutdownCmd() *cobra.Command {
	c := &cobra.Command{Use: "shutdown", Short: "Shut down a node or the whole cluster"}
	c.AddCommand(nodeOpCmd("node", plan.CmdShutdown, "Shut down one node gracefully"))
	c.AddCommand(shutdownClusterCmd())
	return c
}

func newRebootCmd() *cobra.Command {
	c := &cobra.Command{Use: "reboot", Short: "Reboot a node and put it back in service"}
	c.AddCommand(nodeOpCmd("node", plan.CmdReboot, "Reboot one node and restore it"))
	return c
}

func newStartCmd() *cobra.Command {
	c := &cobra.Command{Use: "start", Short: "Wake a node or the whole cluster"}
	c.AddCommand(nodeOpCmd("node", plan.CmdStart, "Wake one node and take it out of maintenance"))
	c.AddCommand(startClusterCmd())
	return c
}

func newMaintCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "maint",
		Short: "Enter or leave maintenance without stopping the host",
		Long: strings.TrimSpace(`
maint enter does everything a shutdown does except the poweroff: ceph flags,
HA evacuation and the kubernetes drain. Use it for physical work on a machine
that stays powered, and maint exit to put it back.`),
	}
	c.AddCommand(nodeOpCmd("enter", plan.CmdMaintEnter, "Put a node into maintenance without stopping it"))
	c.AddCommand(nodeOpCmd("exit", plan.CmdMaintExit, "Take a node out of maintenance"))
	return c
}

// nodeOpCmd builds `<verb> node <name>`. Every node operation is the same
// three lines -- build env, take the lock, run the plan -- because they are
// genuinely the same operation with a different step list.
func nodeOpCmd(use, command, short string) *cobra.Command {
	return &cobra.Command{
		Use:       use + " node <name>",
		Short:     short,
		Args:      cobra.ExactArgs(2),
		ValidArgs: []string{"node"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] != "node" {
				return fmt.Errorf("expected `%s node <name>`, got %q", use, args[0])
			}
			return runNodeOp(cmd.Context(), args[1], command)
		},
	}
}

func runNodeOp(ctx context.Context, target, command string) error {
	e, lg, err := newEnv(target)
	if err != nil {
		return err
	}
	defer lg.Close()
	banner(e, command)

	if err := acquireLock(e, command); err != nil {
		return err
	}
	r, err := plan.NewRunner(e, command)
	if err != nil {
		e.ReleaseLock()
		return err
	}
	r.Describe()
	e.Log.Plain("")
	if impact := impactLines(e, command); impact != "" {
		e.Log.Plain("%s", impact)
	}
	if e.Opts.DryRun {
		// `plan` stops here: the step-by-step read-only walk happens only when
		// the operator asks for it with --dry-run on the real command.
		e.Log.Plain("(dry run) walking the plan read-only:")
	}
	if err := e.Confirm(ctx, confirmPrompt(e, command)); err != nil {
		e.ReleaseLock()
		return err
	}
	start := time.Now()
	if err := r.Run(ctx); err != nil {
		return err
	}
	if err := e.ReleaseLock(); err != nil {
		e.Log.Warnf("%v", err)
	}
	if !plan.KeepsLockOnSuccess(command) && !e.Opts.DryRun {
		if err := state.RemoveState(state.Dir); err != nil {
			e.Log.Warnf("could not clear state.json: %v", err)
		}
	}
	e.Log.Plain("")
	e.Log.Plain("%s %s completed in %s.", command, target, elapsed(start))
	if plan.KeepsLockOnSuccess(command) {
		e.Log.Plain("%s stays under maintenance; finish with `pvectl %s node %s`.",
			target, closingCommand(command), target)
	}
	if lg.Path != "" {
		e.Log.Plain("transcript: %s", lg.Path)
	}
	return nil
}

func closingCommand(command string) string {
	if command == plan.CmdShutdown {
		return "start"
	}
	return "maint exit"
}

func acquireLock(e *plan.Env, command string) error {
	if e.Opts.DryRun {
		// Do not take a real lock for a dry run, but give the plan something
		// to report so the lock step is exercised like any other.
		e.Lock = &state.Lock{Info: state.LockInfo{
			Holder: e.Self, Target: e.Target, Command: command + " (dry-run)", Started: time.Now(),
		}}
		return nil
	}
	lk, adopted, err := state.AcquireOrAdopt(state.Dir, e.Self, e.Target, command)
	if err != nil {
		return err
	}
	if adopted {
		e.Log.Infof("adopting the maintenance lock held for %s since %s",
			lk.Info.Target, lk.Info.Started.Format(time.RFC3339))
	}
	e.Lock = lk
	return nil
}

// impactLines states what the operator loses, and states it per command:
// maint enter leaves every guest running and the OSD up, so claiming otherwise
// would train people to ignore the warning.
func impactLines(e *plan.Env, command string) string {
	if command == plan.CmdStart || command == plan.CmdMaintExit {
		return ""
	}
	var b strings.Builder
	b.WriteString("Impact:\n")
	if command == plan.CmdMaintEnter {
		fmt.Fprintf(&b, "  - %s is cordoned and drained; its pods move to the other two nodes\n", e.Node.Talos.Name)
		fmt.Fprintf(&b, "  - HA resources leave %s (containers migrate offline: a brief outage each)\n", e.Target)
		fmt.Fprintf(&b, "  - ceph noout/norebalance set: no recovery starts while you work\n")
		fmt.Fprintf(&b, "  - guests keep running and the host stays powered\n")
		return b.String()
	}
	for _, g := range e.Node.StopGuests {
		if g.Warn != "" {
			fmt.Fprintf(&b, "  - VM %d will be stopped: %s\n", g.ID, g.Warn)
		} else {
			fmt.Fprintf(&b, "  - VM %d will be stopped\n", g.ID)
		}
	}
	fmt.Fprintf(&b, "  - Ceph goes degraded (size=3, min_size=2): IO continues, recovery is deferred by noout\n")
	fmt.Fprintf(&b, "  - Kubernetes loses %s; its pods move to the other two nodes\n", e.Node.Talos.Name)
	if command == plan.CmdShutdown {
		fmt.Fprintf(&b, "  - The host stays off until `pvectl start node %s`\n", e.Target)
	}
	return b.String()
}

func confirmPrompt(e *plan.Env, command string) string {
	return fmt.Sprintf("About to %s %s.", command, e.Target)
}
