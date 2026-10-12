// Command pvectl performs graceful maintenance on a Proxmox VE cluster that
// also runs Ceph, a Talos Kubernetes control plane, and guests with passthrough
// hardware that cannot migrate.
//
// It exists because doing that by hand means touching four subsystems in the
// right order, and the failure mode of getting it wrong -- noout left set, a
// node left cordoned, two hosts down at once -- is silent until it is not.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/tamago/pvectl/internal/config"
	plog "github.com/tamago/pvectl/internal/log"
	"github.com/tamago/pvectl/internal/plan"
)

var version = "dev"

type globals struct {
	configPath string
	logDir     string
	verbose    bool
	opts       plan.Options
}

var g = globals{opts: plan.DefaultOptions()}

func main() {
	root := newRoot()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "\npvectl: %v\n", err)
		os.Exit(1)
	}
}

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "pvectl",
		Short: "Graceful shutdown, reboot and recovery for the pve/ceph/talos cluster",
		Long: strings.TrimSpace(`
pvectl folds the manual node-maintenance procedure -- ceph flags, HA evacuation,
kubernetes drain, Talos shutdown, passthrough guests, host poweroff and the
cleanup afterwards -- into one command per operation.

Every operation is the same list of steps executed by the same runner, so
--dry-run, resume and abort all describe the same plan. A cluster-wide lock in
/etc/pve makes it structurally impossible to take two nodes down at once.`),
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version,
	}

	pf := root.PersistentFlags()
	pf.StringVar(&g.configPath, "config", config.DefaultPath, "cluster definition")
	pf.StringVar(&g.logDir, "log-dir", plog.DefaultDir, "directory for the run transcript")
	pf.BoolVarP(&g.verbose, "verbose", "v", false, "show every API call and wait probe")
	pf.BoolVar(&g.opts.DryRun, "dry-run", false, "read everything, change nothing")
	pf.BoolVar(&g.opts.Yes, "yes", false, "do not ask for confirmation")
	pf.StringSliceVar(&g.opts.SkipChecks, "skip-check", nil,
		"preflight checks to skip ("+strings.Join(plan.CheckNames, ", ")+")")
	pf.BoolVar(&g.opts.NoSilence, "no-silence", false, "do not create Alertmanager silences or notify Discord")
	pf.DurationVar(&g.opts.ForceStopAfter, "force-stop-after", 0,
		"give a guest this long to shut down cleanly, then hard-stop it (0 = never force)")
	pf.DurationVar(&g.opts.TimeoutDrain, "timeout-drain", g.opts.TimeoutDrain, "kubernetes drain timeout")
	pf.DurationVar(&g.opts.TimeoutGuest, "timeout-guest", g.opts.TimeoutGuest, "guest shutdown timeout")
	pf.DurationVar(&g.opts.TimeoutMigrate, "timeout-migrate", g.opts.TimeoutMigrate, "HA evacuation timeout")
	pf.DurationVar(&g.opts.TimeoutDown, "timeout-down", g.opts.TimeoutDown, "how long to wait for a host to go down")
	pf.DurationVar(&g.opts.TimeoutUp, "timeout-up", g.opts.TimeoutUp, "how long to wait for a host to come back")
	pf.DurationVar(&g.opts.TimeoutSettle, "timeout-settle", g.opts.TimeoutSettle,
		"how long to wait for the cluster to become fully healthy again")
	pf.DurationVar(&g.opts.SilenceFor, "silence-for", g.opts.SilenceFor, "how long Alertmanager silences last")

	root.AddCommand(
		newPreflightCmd(),
		newPlanCmd(),
		newShutdownCmd(),
		newRebootCmd(),
		newUpgradeCmd(),
		newStartCmd(),
		newMaintCmd(),
		newRollingCmd(),
		newStatusCmd(),
		newResumeCmd(),
		newAbortCmd(),
		newDoctorCmd(),
		newConfigCmd(),
	)

	installCompletions(root)
	return root
}

// setup loads the config, opens the transcript and validates the skip list.
// Every command starts here so that a typo in --skip-check fails before
// anything is touched, rather than silently skipping nothing.
func setup() (*config.Config, *plog.Logger, error) {
	for _, s := range g.opts.SkipChecks {
		if !plan.ValidCheckName(s) {
			return nil, nil, fmt.Errorf("unknown check %q; valid names: %s", s, strings.Join(plan.CheckNames, ", "))
		}
	}
	if err := plan.ValidateUpgradeOptions(g.opts); err != nil {
		return nil, nil, err
	}
	cfg, err := config.Load(g.configPath)
	if err != nil {
		return nil, nil, err
	}
	if errs := cfg.Validate(); len(errs) > 0 {
		var b strings.Builder
		b.WriteString("config is invalid:\n")
		for _, e := range errs {
			fmt.Fprintf(&b, "  - %v\n", e)
		}
		return nil, nil, errors.New(strings.TrimRight(b.String(), "\n"))
	}
	lg := plog.New(g.logDir, g.verbose)
	lg.DryRun = g.opts.DryRun
	if cfg.Cluster.Rolling.SettleTimeout.D() > 0 && g.opts.TimeoutSettle == plan.DefaultOptions().TimeoutSettle {
		g.opts.TimeoutSettle = cfg.Cluster.Rolling.SettleTimeout.D()
	}
	return cfg, lg, nil
}

// newEnv is the common path for commands that act on one node.
func newEnv(target string) (*plan.Env, *plog.Logger, error) {
	cfg, lg, err := setup()
	if err != nil {
		return nil, nil, err
	}
	e, err := plan.NewEnv(cfg, target, lg, g.opts)
	if err != nil {
		lg.Close()
		return nil, nil, err
	}
	if lg.Path != "" {
		lg.Debugf("transcript: %s", lg.Path)
	}
	return e, lg, nil
}

// nodeArg validates the node name against the config before anything runs.
func nodeArg(cmd *cobra.Command, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("expected exactly one node name")
	}
	return nil
}

func banner(e *plan.Env, what string) {
	e.Log.Plain("pvectl %s -- %s on %s (from %s)", version, what, e.Target, e.Self)
	if e.Opts.DryRun {
		e.Log.Plain("DRY RUN: nothing will be changed.")
	}
	e.Log.Plain("")
}

func elapsed(start time.Time) string { return time.Since(start).Round(time.Second).String() }
