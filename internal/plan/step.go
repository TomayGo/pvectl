package plan

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tamago/pvectl/internal/state"
)

// Step is one unit of a maintenance operation.
//
// Skip is what makes resume and idempotency work: before doing anything a step
// asks whether the cluster is already in the state it would produce. Undo is
// what makes abort work. A step with a nil Undo is one that cannot be reversed
// (you cannot un-poweroff a host), and the runner says so rather than pretending.
type Step struct {
	Name string
	Desc string
	// Skip returns true when the step's effect is already in place.
	Skip func(ctx context.Context, e *Env) (bool, string, error)
	Do   func(ctx context.Context, e *Env) error
	Undo func(ctx context.Context, e *Env) error
	// Irreversible marks steps that Undo cannot address, so `abort` can report
	// honestly instead of silently skipping them.
	Irreversible bool
}

type Runner struct {
	Env   *Env
	Steps []Step
	// Name is the command this plan implements, recorded in state.json.
	Name string
	// Continued reports that this run adopted an earlier run's state rather
	// than creating its own. A caller that abandons the run before it starts
	// must not delete state it did not write.
	Continued bool
}

func (r *Runner) StepNames() []string {
	out := make([]string, 0, len(r.Steps))
	for _, s := range r.Steps {
		out = append(out, s.Name)
	}
	return out
}

func (r *Runner) step(name string) *Step {
	for i := range r.Steps {
		if r.Steps[i].Name == name {
			return &r.Steps[i]
		}
	}
	return nil
}

// Describe prints the plan without touching anything. This is what
// `pvectl plan ...` shows.
func (r *Runner) Describe() {
	e := r.Env
	e.Log.Plain("Plan: %s %s", r.Name, e.Target)
	e.Log.Plain("%-3s %-16s %-8s %s", "#", "step", "undo", "description")
	e.Log.Plain("%s", strings.Repeat("-", 78))
	for i, s := range r.Steps {
		undo := "yes"
		switch {
		case s.Irreversible:
			undo = "NO"
		case s.Undo == nil:
			undo = "-"
		}
		e.Log.Plain("%-3d %-16s %-8s %s", i+1, s.Name, undo, s.Desc)
	}
}

// Run executes the plan, resuming from the first step that has not completed.
func (r *Runner) Run(ctx context.Context) error {
	e := r.Env
	st := e.State
	for i := range r.Steps {
		s := &r.Steps[i]
		prev := st.Step(s.Name)
		if prev != nil && (prev.Status == state.StepDone || prev.Status == state.StepSkipped) {
			e.Log.Debugf("step %s already %s, resuming past it", s.Name, prev.Status)
			continue
		}
		if err := r.runStep(ctx, s); err != nil {
			st.Set(s.Name, state.StepFailed, err)
			_ = st.Save()
			e.Log.Errorf("step %s failed: %v", s.Name, err)
			e.Log.Plain("")
			e.Log.Plain("The run stopped at step %q. Nothing further was attempted.", s.Name)
			e.Log.Plain("  fix the cause and continue:  pvectl resume")
			e.Log.Plain("  or roll back what was done:  pvectl abort")
			return fmt.Errorf("step %s: %w", s.Name, err)
		}
	}
	st.Finished = true
	_ = st.Save()
	return nil
}

func (r *Runner) runStep(ctx context.Context, s *Step) error {
	e := r.Env
	st := e.State
	e.Log.Step(s.Name, s.Desc)
	if s.Skip != nil {
		skip, reason, err := s.Skip(ctx, e)
		if err != nil {
			return err
		}
		if skip {
			e.Log.Infof("already satisfied: %s", reason)
			st.Set(s.Name, state.StepSkipped, nil)
			return st.Save()
		}
	}
	st.Set(s.Name, state.StepRunning, nil)
	if err := st.Save(); err != nil {
		e.Log.Warnf("could not persist state before %s: %v", s.Name, err)
	}
	if s.Do == nil {
		st.Set(s.Name, state.StepDone, nil)
		return st.Save()
	}
	start := time.Now()
	if err := s.Do(ctx, e); err != nil {
		return err
	}
	e.Log.Debugf("step %s completed in %s", s.Name, time.Since(start).Round(time.Second))
	st.Set(s.Name, state.StepDone, nil)
	return st.Save()
}

// Rollback walks the completed steps backwards and calls their Undo.
//
// It keeps going after a failure on purpose: leaving noout set because
// uncordon failed would be the worse outcome, and the caller gets every error.
func (r *Runner) Rollback(ctx context.Context) error {
	e := r.Env
	st := e.State
	done := st.CompletedSteps()
	if len(done) == 0 {
		e.Log.Infof("nothing to roll back")
		return nil
	}
	var errs []error
	for i := len(done) - 1; i >= 0; i-- {
		name := done[i]
		s := r.step(name)
		if s == nil {
			e.Log.Warnf("step %s is not part of this plan; skipping its rollback", name)
			continue
		}
		if s.Irreversible {
			e.Log.Warnf("step %s cannot be undone (%s); leaving it as-is", name, s.Desc)
			continue
		}
		if s.Undo == nil {
			e.Log.Debugf("step %s has nothing to undo", name)
			continue
		}
		e.Log.Step("undo:"+name, "roll back "+s.Desc)
		if err := s.Undo(ctx, e); err != nil {
			e.Log.Errorf("rollback of %s failed: %v", name, err)
			errs = append(errs, fmt.Errorf("undo %s: %w", name, err))
			continue
		}
		st.Set(name, state.StepUndone, nil)
		_ = st.Save()
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}
