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
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeNodeOptional,
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
		withTalosFlags(nodeOpCmd("upgrade", plan.CmdUpgrade, "Plan a node upgrade")),
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

func newUpgradeCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade a node's packages (and optionally Talos), then reboot it",
		Long: strings.TrimSpace(`
upgrade is reboot with two installs after the drain: apt-get update and
full-upgrade on the PVE host, and -- with --talos-version -- the new Talos
release written to the Talos VM's inactive boot slot. Neither reboots anything
itself; the host reboot that follows boots both, so the node goes down once.

The run refuses a full-upgrade that would remove packages or change the major
version of Ceph or pve-manager: those need a human and their own procedure.`),
	}
	c.AddCommand(withTalosFlags(nodeOpCmd("node", plan.CmdUpgrade, "Upgrade one node and reboot it")))
	return c
}

// withTalosFlags binds the Talos target flags straight into the global
// options, which is where plan.NewRunner reads them from.
func withTalosFlags(c *cobra.Command) *cobra.Command {
	c.Flags().StringVar(&g.opts.TalosVersion, "talos-version", "",
		"also upgrade Talos to this release (e.g. v1.13.7), keeping the node's Image Factory schematic")
	c.Flags().StringVar(&g.opts.TalosImage, "talos-image", "",
		"also upgrade Talos using exactly this installer image (instead of --talos-version)")
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

// nodeOpCmd builds a node operation. The subcommand appears at two different
// depths -- `pvectl reboot node pve-3` names it "node", while
// `pvectl plan reboot node pve-3` names it "reboot" and leaves "node" in the
// arguments -- so it accepts either shape and takes the node name from the end.
func nodeOpCmd(use, command, short string) *cobra.Command {
	usage := use + " <name>"
	if use != "node" {
		usage = use + " node <name>"
	}
	return &cobra.Command{
		Use:               usage,
		Short:             short,
		ValidArgsFunction: completeNodeOp(use),
		Args: func(cmd *cobra.Command, args []string) error {
			switch {
			case len(args) == 0:
				return fmt.Errorf("which node? usage: %s%s", cmd.CommandPath(), strings.TrimPrefix(usage, use))
			case len(args) > 2:
				return fmt.Errorf("expected one node name, got %d arguments", len(args))
			case len(args) == 2 && args[0] != "node":
				return fmt.Errorf("expected `node <name>`, got %q", args[0])
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runNodeOp(cmd.Context(), args[len(args)-1], command, false)
		},
	}
}

// fromRolling says the call comes from `rolling reboot`, which keeps its own
// progress record; a standalone reboot has to update that record itself.
func runNodeOp(ctx context.Context, target, command string, fromRolling bool) error {
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
		abandonRun(e, false)
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
		abandonRun(e, !r.Continued)
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
		if err := state.RemoveState(e.StateDir); err != nil {
			e.Log.Warnf("could not clear state.json: %v", err)
		}
	}
	if !fromRolling && !e.Opts.DryRun {
		closeRollingEntry(e, command, target)
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

// closeRollingEntry marks a node done in an in-flight rolling reboot.
//
// `rolling reboot` cannot reboot the node it runs on, so it hands the last one
// to the operator. Without this the record would never be completed: the next
// rolling reboot would resume it, treat the already-done nodes as done again,
// and skip them while reporting success.
// Only the commands that leave a node back in service close its entry;
// `shutdown` and `maint enter` leave the window open, and the `start` /
// `maint exit` that closes it is what counts.
//
// A rolling upgrade is only advanced by an upgrade: a plain reboot of that node
// installs nothing, and counting it would leave the node on the old packages
// while the record says it is done.
func closesRollingEntry(rolling, command string) bool {
	if rolling == rollingUpgrade {
		return command == plan.CmdUpgrade
	}
	switch command {
	case plan.CmdReboot, plan.CmdUpgrade, plan.CmdStart, plan.CmdMaintExit:
		return true
	}
	return false
}

func closeRollingEntry(e *plan.Env, command, node string) {
	prog, err := state.LoadRolling(e.StateDir)
	if err != nil || prog == nil {
		return
	}
	if !closesRollingEntry(prog.Command, command) {
		return
	}
	for _, d := range prog.Done {
		if d == node {
			return
		}
	}
	prog.Done = append(prog.Done, node)
	if len(prog.Remaining()) == 0 {
		if err := state.RemoveRolling(e.StateDir); err != nil {
			e.Log.Warnf("could not clear rolling.json: %v", err)
			return
		}
		e.Log.Plain("")
		e.Log.Plain("This completes the %s of %s.", rollingWhat(prog.Command), strings.Join(prog.Order, ", "))
		return
	}
	if err := prog.Save(e.StateDir); err != nil {
		e.Log.Warnf("could not record rolling progress: %v", err)
	}
}

// abandonRun backs out of a run that never executed a step.
//
// It must not release a lock this invocation adopted: `start node X` declining
// its own confirmation would otherwise drop the lock that `shutdown node X`
// left holding, while X is still powered off and the window still open.
func abandonRun(e *plan.Env, removeState bool) {
	if e.Adopted {
		e.Log.Infof("leaving the adopted maintenance lock in place: %s is still under maintenance", e.Target)
	} else {
		e.KeepLock = false
		if err := e.ReleaseLock(); err != nil {
			e.Log.Warnf("%v", err)
		}
	}
	// NewRunner has already written state.json. Nothing ran, so leaving it
	// behind would make `status` and `doctor` report a phantom interrupted
	// run -- but only discard state this run actually created.
	if removeState && !e.Opts.DryRun {
		if err := state.RemoveState(e.StateDir); err != nil {
			e.Log.Warnf("could not clear state.json: %v", err)
		}
	}
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
	lk, adopted, err := state.AcquireOrAdopt(e.StateDir, e.Self, e.Target, command)
	if err != nil {
		return err
	}
	if adopted {
		e.Log.Infof("adopting the maintenance lock held for %s since %s",
			lk.Info.Target, lk.Info.Started.Format(time.RFC3339))
	}
	e.Adopted = adopted
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
	if command == plan.CmdUpgrade {
		fmt.Fprintf(&b, "  - apt-get full-upgrade runs on %s (refused if it would remove packages)\n", e.Target)
		switch {
		case e.Opts.TalosVersion != "":
			fmt.Fprintf(&b, "  - %s is upgraded to Talos %s\n", e.Node.Talos.Name, e.Opts.TalosVersion)
		case e.Opts.TalosImage != "":
			fmt.Fprintf(&b, "  - %s is upgraded to %s\n", e.Node.Talos.Name, e.Opts.TalosImage)
		default:
			fmt.Fprintf(&b, "  - Talos is not upgraded (no --talos-version)\n")
		}
	}
	if command == plan.CmdShutdown {
		fmt.Fprintf(&b, "  - The host stays off until `pvectl start node %s`\n", e.Target)
	}
	return b.String()
}

func confirmPrompt(e *plan.Env, command string) string {
	return fmt.Sprintf("About to %s %s.", command, e.Target)
}
