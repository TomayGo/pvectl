package plan

import (
	"fmt"
	"os"

	"github.com/TomayGo/pvectl/internal/state"
)

// Command names as recorded in state.json. `resume` and `abort` rebuild a
// runner from this string, so they must stay stable.
const (
	CmdMaintEnter = "maint-enter"
	CmdMaintExit  = "maint-exit"
	CmdShutdown   = "shutdown"
	CmdReboot     = "reboot"
	CmdStart      = "start"
	CmdUpgrade    = "upgrade"
)

// maintEnterSteps is the shared prefix: everything that makes a node safe to
// stop, without stopping it. `shutdown` is this plus a poweroff, which is
// exactly why the two are one code path.
//
// forShutdown says whether a poweroff follows. It gates the checks that only
// make sense when the host is about to disappear -- running pvectl on the
// target, or over the Wi-Fi its AP guest serves. `maint enter` leaves the host
// up and its guests running, so blocking on those would be a false alarm.
func maintEnterSteps(e *Env, forShutdown bool) []Step {
	return []Step{
		stepLock(),
		stepPreflight(forShutdown),
		stepSilence(),
		stepCephFlags(e.Cfg.Cluster.CephFlags),
		stepHAEvacuate(),
		stepK8sDrain(),
	}
}

// StepsFor returns the plan for a command.
func StepsFor(command string, e *Env) ([]Step, error) {
	switch command {
	case CmdMaintEnter:
		return maintEnterSteps(e, false), nil
	case CmdShutdown:
		steps := append(maintEnterSteps(e, true), stepTalosStop(), stepGuestStop())
		return append(steps, hostPowerAndWait("shutdown")...), nil
	case CmdReboot:
		steps := append(maintEnterSteps(e, true), stepTalosStop(), stepGuestStop())
		steps = append(steps, hostPowerAndWait("reboot")...)
		return append(steps, stepWaitUp(), stepRestore(), stepVerify()), nil
	case CmdUpgrade:
		// The installs sit between the drain and talos-stop; see upgrade.go.
		steps := append(maintEnterSteps(e, true), stepTalosUpgrade(), stepHostUpgrade(), stepTalosStop(), stepGuestStop())
		steps = append(steps, hostPowerAndWait("reboot")...)
		return append(steps, stepWaitUp(), stepRestore(), stepVerify(), stepUpgradeCheck()), nil
	case CmdStart:
		return []Step{
			stepLock(),
			stepWake(),
			stepWaitUp(),
			stepRestore(),
			stepVerify(),
		}, nil
	case CmdMaintExit:
		return []Step{
			stepLock(),
			stepRestore(),
			stepVerify(),
		}, nil
	}
	return nil, fmt.Errorf("unknown command %q in state.json", command)
}

// hostPowerAndWait keeps the power step and its waiter together. wait-down
// reads the uptime that the power step recorded, so the two must agree on the
// step name -- building them from one string is what guarantees that.
func hostPowerAndWait(command string) []Step {
	name := "host-" + command
	return []Step{stepHostPower(command), stepWaitDown(name)}
}

// KeepsLockOnSuccess reports whether the node is still under maintenance when
// the command finishes. shutdown and maint enter leave the node in a state
// that a later start / maint exit has to close out, so the lock stays held --
// that is what stops a second node from being taken down in between.
func KeepsLockOnSuccess(command string) bool {
	switch command {
	case CmdShutdown, CmdMaintEnter:
		return true
	}
	return false
}

// NewRunner builds a runner and prepares the run state.
//
// Commands that close a maintenance window (start, maint exit) continue the
// existing state rather than starting a new one: the rollback data they need --
// which ceph flags we set, which silences we created -- was recorded by the
// command that opened the window.
func NewRunner(e *Env, command string) (*Runner, error) {
	steps, err := StepsFor(command, e)
	if err != nil {
		return nil, err
	}
	r := &Runner{Env: e, Steps: steps, Name: command}
	e.KeepLock = KeepsLockOnSuccess(command)

	continues := command == CmdStart || command == CmdMaintExit
	existing, lerr := state.LoadState(state.Dir)
	if lerr != nil && existing == nil {
		return nil, lerr
	}
	if lerr != nil {
		e.Log.Warnf("existing state.json is unusable (%v); starting a new one", lerr)
		existing = nil
	}

	switch {
	case continues && existing != nil && existing.Target == e.Target:
		r.Continued = true
		e.Log.Infof("continuing the maintenance window opened by `%s %s`", existing.Command, existing.Target)
		existing.Command = command
		for _, s := range r.StepNames() {
			if existing.Step(s) == nil {
				existing.Steps = append(existing.Steps, state.StepState{Name: s, Status: state.StepPending})
			}
		}
		existing.Finished = false
		e.State = existing
	case continues && existing == nil:
		e.Log.Warnf("no earlier state found for %s; restore will only do what it can infer", e.Target)
		e.State = state.NewState(state.Dir, command, e.Target, e.Self, r.StepNames())
	default:
		fresh := state.NewState(state.Dir, command, e.Target, e.Self, r.StepNames())
		if existing != nil && !existing.Finished {
			e.Log.Warnf("discarding an unfinished %s %s state; run `pvectl abort` first if you meant to roll it back",
				existing.Command, existing.Target)
			if existing.Target == e.Target {
				// The steps re-run from the start, but what the abandoned
				// attempt already changed is still changed. Carry its rollback
				// data over, or this run disowns the ceph flags and silences
				// the last one created and restore leaves them behind.
				carryRollbackData(existing, fresh)
			}
		}
		e.State = fresh
	}
	if command == CmdUpgrade {
		recordUpgradeRequest(e)
	}
	e.State.SetDryRun(e.Opts.DryRun)
	if err := e.State.Save(); err != nil {
		return nil, fmt.Errorf("persist run state: %w", err)
	}
	return r, nil
}

// carryRollbackData copies each step's recorded undo data from an abandoned
// run into a fresh one, for steps the new plan also has.
func carryRollbackData(from, to *state.State) {
	for _, old := range from.Steps {
		if len(old.Data) == 0 {
			continue
		}
		if to.Step(old.Name) == nil {
			continue
		}
		for k, v := range old.Data {
			to.SetData(old.Name, k, v)
		}
	}
}

// ResumeRunner rebuilds the runner for whatever run is recorded in state.json.
func ResumeRunner(e *Env, st *state.State) (*Runner, error) {
	steps, err := StepsFor(st.Command, e)
	if err != nil {
		return nil, err
	}
	e.State = st
	e.KeepLock = KeepsLockOnSuccess(st.Command)
	return &Runner{Env: e, Steps: steps, Name: st.Command}, nil
}

// Hostname is the node identity recorded in the lock.
func Hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}
