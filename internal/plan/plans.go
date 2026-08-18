package plan

import (
	"fmt"
	"os"

	"github.com/tamago/pvectl/internal/state"
)

// Command names as recorded in state.json. `resume` and `abort` rebuild a
// runner from this string, so they must stay stable.
const (
	CmdMaintEnter = "maint-enter"
	CmdMaintExit  = "maint-exit"
	CmdShutdown   = "shutdown"
	CmdReboot     = "reboot"
	CmdStart      = "start"
)

// maintEnterSteps is the shared prefix: everything that makes a node safe to
// stop, without stopping it. `shutdown` is this plus a poweroff, which is
// exactly why the two are one code path.
func maintEnterSteps(e *Env) []Step {
	return []Step{
		stepLock(),
		stepPreflight(true),
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
		return maintEnterSteps(e), nil
	case CmdShutdown:
		return append(maintEnterSteps(e),
			stepTalosStop(),
			stepGuestStop(),
			stepHostPower("shutdown"),
			stepWaitDown(),
		), nil
	case CmdReboot:
		return append(maintEnterSteps(e),
			stepTalosStop(),
			stepGuestStop(),
			stepHostPower("reboot"),
			stepWaitDown(),
			stepWaitUp(),
			stepRestore(),
			stepVerify(),
		), nil
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
		if existing != nil && !existing.Finished {
			e.Log.Warnf("discarding an unfinished %s %s state; run `pvectl abort` first if you meant to roll it back",
				existing.Command, existing.Target)
		}
		e.State = state.NewState(state.Dir, command, e.Target, e.Self, r.StepNames())
	}
	e.State.SetDryRun(e.Opts.DryRun)
	if err := e.State.Save(); err != nil {
		return nil, fmt.Errorf("persist run state: %w", err)
	}
	return r, nil
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
