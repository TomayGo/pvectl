package plan

import (
	"context"
	"time"

	"github.com/TomayGo/pvectl/internal/config"
	"github.com/TomayGo/pvectl/internal/pve"
)

// Shutting a guest down and starting it again is done in two places: the
// guest-stop step of a node's plan, and `shutdown cluster` / `start cluster`,
// which go through every node without a plan. They report differently -- one
// names the guest, the other the node -- but what they ask of Proxmox, and for
// how long they wait, is the same and is here.

const (
	// shutdownSlack is added to the graceful window when waiting for the
	// shutdown task: PVE enforces the window itself and needs a moment to
	// report that it ran out.
	shutdownSlack = 30 * time.Second
	forceStopWait = 2 * time.Minute
	startWait     = 3 * time.Minute
)

// GracefulWindow is how long a guest gets to shut down cleanly.
//
// --force-stop-after names the deadline, so it is the deadline: give the
// guest that long, then pull the plug. With the flag unset the window is
// --timeout-guest, and running out of it is a failure rather than a reason to
// force.
func (e *Env) GracefulWindow() time.Duration {
	if e.Opts.ForceStopAfter > 0 {
		return e.Opts.ForceStopAfter
	}
	return e.Opts.TimeoutGuest
}

// MayForceStop reports whether --force-stop-after allows a hard stop.
func (e *Env) MayForceStop() bool { return e.Opts.ForceStopAfter > 0 }

// RequestGuestShutdown sends the ACPI shutdown, with the graceful window as
// PVE's own timeout, and returns the worker task. It goes through any node,
// the guest's own included: reading and stopping a guest needs its node up.
func (e *Env) RequestGuestShutdown(ctx context.Context, node, kind string, vmid int) (string, error) {
	return e.PVEAll.GuestShutdown(ctx, node, kind, vmid, e.GracefulWindow(), false)
}

// AwaitGuestShutdown waits for the task RequestGuestShutdown returned.
func (e *Env) AwaitGuestShutdown(ctx context.Context, node, upid string) error {
	return e.PVEAll.WaitTask(ctx, node, upid, e.GracefulWindow()+shutdownSlack)
}

// ForceStopGuest hard-stops a guest and waits for it to be gone.
func (e *Env) ForceStopGuest(ctx context.Context, node, kind string, vmid int) error {
	upid, err := e.PVEAll.GuestStop(ctx, node, kind, vmid)
	if err != nil {
		return err
	}
	return e.PVEAll.WaitTask(ctx, node, upid, forceStopWait)
}

// GuestKind is the API kind ("qemu" or "lxc") of a guest named only by id, as
// start_after_boot names them: the kind its stop_guests entry gives, and a VM
// when it has none.
func GuestKind(n config.Node, id int) string {
	kind := "qemu"
	for _, g := range n.StopGuests {
		if g.ID == id {
			kind = g.Kind()
		}
	}
	return kind
}

// StartGuest starts a guest through c and waits for the task.
func StartGuest(ctx context.Context, c *pve.Client, node, kind string, id int) error {
	upid, err := c.GuestStart(ctx, node, kind, id)
	if err != nil {
		return err
	}
	return c.WaitTask(ctx, node, upid, startWait)
}
