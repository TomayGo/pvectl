// Package plan turns a maintenance operation into an ordered list of steps and
// runs them.
//
// Every operation -- shutdown, reboot, start, maint enter/exit, rolling reboot
// -- is the same []Step structure executed by the same runner. --dry-run,
// resume and abort are not separate code paths but three ways of walking that
// one structure, which is what keeps them from drifting apart.
package plan

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/tamago/pvectl/internal/alert"
	"github.com/tamago/pvectl/internal/config"
	"github.com/tamago/pvectl/internal/k8s"
	plog "github.com/tamago/pvectl/internal/log"
	"github.com/tamago/pvectl/internal/pve"
	"github.com/tamago/pvectl/internal/state"
	"github.com/tamago/pvectl/internal/talos"
)

type Options struct {
	DryRun         bool
	Yes            bool
	SkipChecks     []string
	NoSilence      bool
	ForceStopAfter time.Duration

	TimeoutDrain   time.Duration
	TimeoutGuest   time.Duration
	TimeoutMigrate time.Duration
	TimeoutDown    time.Duration
	TimeoutUp      time.Duration
	TimeoutSettle  time.Duration

	// SilenceFor bounds the Alertmanager silence so a crashed run cannot
	// leave the cluster permanently muted.
	SilenceFor time.Duration
}

func DefaultOptions() Options {
	return Options{
		TimeoutDrain:   5 * time.Minute,
		TimeoutGuest:   3 * time.Minute,
		TimeoutMigrate: 5 * time.Minute,
		TimeoutDown:    5 * time.Minute,
		TimeoutUp:      15 * time.Minute,
		TimeoutSettle:  15 * time.Minute,
		SilenceFor:     2 * time.Hour,
	}
}

// Env is everything a step needs. Clients are built once and shared, so the
// endpoint exclusion and the dry-run flag are decided in exactly one place.
type Env struct {
	Cfg    *config.Config
	Target string
	Node   config.Node
	Self   string
	Log    *plog.Logger
	Opts   Options
	State  *state.State
	PVE    *pve.Client
	K8s    *k8s.Client
	Talos  *talos.Client
	Alert  *alert.Client
	// PVEAll keeps the unfiltered client, for the few calls that must be able
	// to reach the target itself (reading its guest list before it goes down).
	PVEAll *pve.Client
	// Lock is the cluster-wide maintenance lock. It is taken by the command
	// layer before any state is written, and outlives a single invocation:
	// `shutdown` leaves it held so a later `start` continues the same window.
	Lock *state.Lock
	// KeepLock stops the plan from releasing the lock on completion, for the
	// commands that end with the node still under maintenance.
	KeepLock bool
}

// ReleaseLock drops the maintenance lock unless the command asked to keep it.
func (e *Env) ReleaseLock() error {
	if e.Opts.DryRun {
		return nil
	}
	if e.KeepLock {
		e.Log.Infof("leaving the maintenance lock held: %s is still under maintenance", e.Target)
		return nil
	}
	if e.Lock == nil {
		return nil
	}
	if err := e.Lock.Release(); err != nil {
		return err
	}
	e.Log.Infof("maintenance lock released")
	e.Lock = nil
	return nil
}

// NewEnv wires up every client. target may be "" for cluster-wide commands,
// in which case no endpoint is excluded.
func NewEnv(cfg *config.Config, target string, lg *plog.Logger, opts Options) (*Env, error) {
	e := &Env{Cfg: cfg, Target: target, Log: lg, Opts: opts}
	host, _ := os.Hostname()
	e.Self = host

	if target != "" {
		n, err := cfg.Node(target)
		if err != nil {
			return nil, err
		}
		e.Node = n
	}

	cl, err := pve.New(cfg.Cluster.Endpoints, cfg.SecretPath("token"), lg, pve.WithDryRun(opts.DryRun))
	if err != nil {
		return nil, err
	}
	e.PVEAll = cl
	// Commands aimed at a node must not be sent to that node: the reply never
	// arrives once it starts going down.
	if target != "" {
		e.PVE = cl.Excluded(e.Node.LANIP)
		if len(e.PVE.Endpoints()) == 0 {
			return nil, fmt.Errorf("excluding %s leaves no API endpoint; add the other nodes to cluster.endpoints", e.Node.LANIP)
		}
	} else {
		e.PVE = cl
	}

	kc, err := k8s.New(cfg.SecretPath("kubeconfig"), lg, opts.DryRun)
	if err != nil {
		return nil, err
	}
	e.K8s = kc
	e.Talos = talos.New(cfg.SecretPath("talosconfig"), lg, opts.DryRun)
	e.Alert = alert.New(kc,
		alert.Endpoint{Namespace: cfg.Cluster.Alertmanager.Namespace, Service: cfg.Cluster.Alertmanager.Service, Port: cfg.Cluster.Alertmanager.Port},
		alert.Endpoint{Namespace: cfg.Cluster.DiscordBridge.Namespace, Service: cfg.Cluster.DiscordBridge.Service, Port: cfg.Cluster.DiscordBridge.Port},
		lg, opts.DryRun)
	return e, nil
}

// TalosIPs lists every Talos node address, used for cluster-wide etcd checks.
func (e *Env) TalosIPs() []string {
	var out []string
	for _, name := range e.Cfg.NodeNames() {
		out = append(out, e.Cfg.Nodes[name].Talos.IP)
	}
	return out
}

// OtherNodes lists the cluster nodes that are not the target.
func (e *Env) OtherNodes() []string {
	var out []string
	for _, n := range e.Cfg.NodeNames() {
		if n != e.Target {
			out = append(out, n)
		}
	}
	return out
}

// Confirm asks for interactive approval unless --yes or --dry-run.
func (e *Env) Confirm(ctx context.Context, prompt string) error {
	if e.Opts.Yes || e.Opts.DryRun {
		return nil
	}
	fmt.Printf("\n%s\nType 'yes' to continue: ", prompt)
	var answer string
	if _, err := fmt.Fscanln(os.Stdin, &answer); err != nil {
		return fmt.Errorf("aborted (no answer read: %v)", err)
	}
	if answer != "yes" {
		return fmt.Errorf("aborted by user")
	}
	return nil
}
