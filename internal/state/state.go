// Package state holds the cluster-wide maintenance lock and the resumable
// run state.
//
// Both live in /etc/pve, which is pmxcfs: a corosync-replicated filesystem, so
// an O_EXCL create is atomic across the whole cluster. That is what makes the
// lock a structural guarantee rather than a convention -- two nodes cannot both
// believe they hold it, and therefore two nodes cannot be taken down at once.
// (flock would not do: on a FUSE mount it is node-local and says nothing about
// what the other two nodes are doing.)
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Dir is the shared state directory. Deliberately in /etc/pve so every node
// sees the same lock; deliberately holding no secrets, because pveproxy runs as
// www-data and can read this tree.
const Dir = "/etc/pve/pvectl"

const (
	lockName  = "maintenance.lock"
	stateName = "state.json"
)

type LockInfo struct {
	// Holder is the node running pvectl.
	Holder string `json:"holder"`
	// Target is the node being worked on.
	Target  string    `json:"target"`
	Command string    `json:"command"`
	PID     int       `json:"pid"`
	Started time.Time `json:"started"`
}

func (l LockInfo) String() string {
	return fmt.Sprintf("%s %s (started by %s, pid %d, held for %s)",
		l.Command, l.Target, l.Holder, l.PID,
		time.Since(l.Started).Round(time.Second))
}

type Lock struct {
	path string
	Info LockInfo
}

var ErrLocked = errors.New("another maintenance operation holds the cluster lock")

// Acquire takes the cluster-wide maintenance lock.
func Acquire(dir, holder, target, command string) (*Lock, error) {
	if dir == "" {
		dir = Dir
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	p := filepath.Join(dir, lockName)
	info := LockInfo{Holder: holder, Target: target, Command: command, PID: os.Getpid(), Started: time.Now()}
	b, _ := json.MarshalIndent(info, "", "  ")

	fd, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		if errors.Is(err, os.ErrExist) || errors.Is(err, syscall.EEXIST) {
			if held, perr := Peek(dir); perr == nil {
				return nil, fmt.Errorf("%w: %s", ErrLocked, held)
			}
			return nil, fmt.Errorf("%w (holder unreadable at %s)", ErrLocked, p)
		}
		return nil, fmt.Errorf("acquire lock %s: %w", p, err)
	}
	if _, err := fd.Write(append(b, '\n')); err != nil {
		fd.Close()
		os.Remove(p)
		return nil, fmt.Errorf("write lock %s: %w", p, err)
	}
	fd.Close()
	return &Lock{path: p, Info: info}, nil
}

// Release drops the lock. Safe to call twice.
func (l *Lock) Release() error {
	if l == nil || l.path == "" {
		return nil
	}
	err := os.Remove(l.path)
	l.path = ""
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("release lock: %w", err)
	}
	return nil
}

// Peek reads the current lock holder, or (nil, nil) if the lock is free.
func Peek(dir string) (*LockInfo, error) {
	if dir == "" {
		dir = Dir
	}
	b, err := os.ReadFile(filepath.Join(dir, lockName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var info LockInfo
	if err := json.Unmarshal(b, &info); err != nil {
		return nil, fmt.Errorf("lock file is corrupt: %w", err)
	}
	return &info, nil
}

// ForceRelease removes the lock regardless of holder. Only `doctor --fix` and
// `abort` should call it, and only after telling the user whose lock it is.
func ForceRelease(dir string) error {
	if dir == "" {
		dir = Dir
	}
	err := os.Remove(filepath.Join(dir, lockName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// ---------- run state ----------

type StepStatus string

const (
	StepPending StepStatus = "pending"
	StepRunning StepStatus = "running"
	StepDone    StepStatus = "done"
	StepFailed  StepStatus = "failed"
	StepSkipped StepStatus = "skipped"
	StepUndone  StepStatus = "undone"
)

type StepState struct {
	Name    string     `json:"name"`
	Status  StepStatus `json:"status"`
	Started time.Time  `json:"started,omitempty"`
	Ended   time.Time  `json:"ended,omitempty"`
	Error   string     `json:"error,omitempty"`
	// Data carries whatever a step must remember for its own rollback, e.g.
	// which ceph flags were already set before we touched them.
	Data map[string]any `json:"data,omitempty"`
}

type State struct {
	Version  int         `json:"version"`
	Command  string      `json:"command"`
	Target   string      `json:"target"`
	Holder   string      `json:"holder"`
	Started  time.Time   `json:"started"`
	Updated  time.Time   `json:"updated"`
	Finished bool        `json:"finished"`
	Steps    []StepState `json:"steps"`
	// Queue is the remaining node list for cluster-wide operations, so a
	// rolling reboot interrupted after node 2 resumes at node 3.
	Queue []string `json:"queue,omitempty"`
	Done  []string `json:"done,omitempty"`

	dir string
	// dryRun keeps a --dry-run run from writing to /etc/pve at all. The step
	// machine still tracks status in memory, so the plan walks exactly as it
	// would for real.
	dryRun bool
}

// SetDryRun makes Save a no-op.
func (s *State) SetDryRun(v bool) { s.dryRun = v }

const stateVersion = 1

func NewState(dir, command, target, holder string, steps []string) *State {
	if dir == "" {
		dir = Dir
	}
	s := &State{
		Version: stateVersion,
		Command: command,
		Target:  target,
		Holder:  holder,
		Started: time.Now(),
		dir:     dir,
	}
	for _, n := range steps {
		s.Steps = append(s.Steps, StepState{Name: n, Status: StepPending})
	}
	return s
}

func LoadState(dir string) (*State, error) {
	if dir == "" {
		dir = Dir
	}
	b, err := os.ReadFile(filepath.Join(dir, stateName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("state.json is corrupt: %w", err)
	}
	s.dir = dir
	if s.Version != stateVersion {
		return &s, fmt.Errorf("state.json has version %d, this pvectl writes %d", s.Version, stateVersion)
	}
	return &s, nil
}

// Save writes atomically: a torn state.json during a shutdown would leave
// `resume` with nothing to work from.
func (s *State) Save() error {
	if s.dryRun {
		s.Updated = time.Now()
		return nil
	}
	if s.dir == "" {
		s.dir = Dir
	}
	s.Updated = time.Now()
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, ".state.json.tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), 0o640); err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(s.dir, stateName)); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("commit state: %w", err)
	}
	return nil
}

func RemoveState(dir string) error {
	if dir == "" {
		dir = Dir
	}
	err := os.Remove(filepath.Join(dir, stateName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (s *State) Step(name string) *StepState {
	for i := range s.Steps {
		if s.Steps[i].Name == name {
			return &s.Steps[i]
		}
	}
	return nil
}

func (s *State) Set(name string, status StepStatus, err error) {
	st := s.Step(name)
	if st == nil {
		s.Steps = append(s.Steps, StepState{Name: name})
		st = &s.Steps[len(s.Steps)-1]
	}
	st.Status = status
	switch status {
	case StepRunning:
		st.Started = time.Now()
		st.Error = ""
	case StepDone, StepFailed, StepSkipped, StepUndone:
		st.Ended = time.Now()
	}
	if err != nil {
		st.Error = err.Error()
	}
}

func (s *State) SetData(name, key string, value any) {
	st := s.Step(name)
	if st == nil {
		return
	}
	if st.Data == nil {
		st.Data = map[string]any{}
	}
	st.Data[key] = value
}

// CompletedSteps returns the steps that ran to completion, oldest first. Undo
// walks this list backwards.
func (s *State) CompletedSteps() []string {
	var out []string
	for _, st := range s.Steps {
		if st.Status == StepDone {
			out = append(out, st.Name)
		}
	}
	return out
}

// FirstIncomplete is where `resume` picks up.
func (s *State) FirstIncomplete() string {
	for _, st := range s.Steps {
		if st.Status == StepPending || st.Status == StepRunning || st.Status == StepFailed {
			return st.Name
		}
	}
	return ""
}

func (s *State) Summary() string {
	done, failed, pending := 0, 0, 0
	for _, st := range s.Steps {
		switch st.Status {
		case StepDone, StepSkipped:
			done++
		case StepFailed:
			failed++
		default:
			pending++
		}
	}
	return fmt.Sprintf("%s %s: %d done, %d failed, %d pending (started %s ago)",
		s.Command, s.Target, done, failed, pending, time.Since(s.Started).Round(time.Second))
}

// AcquireOrAdopt takes the lock, or adopts an existing one that is already
// held for the same target.
//
// A maintenance window outlives a single pvectl invocation: `shutdown node X`
// leaves the node off and the lock held, and a later `start node X` continues
// the same window. Only a lock held for a *different* target is a conflict.
func AcquireOrAdopt(dir, holder, target, command string) (*Lock, bool, error) {
	l, err := Acquire(dir, holder, target, command)
	if err == nil {
		return l, false, nil
	}
	if !errors.Is(err, ErrLocked) {
		return nil, false, err
	}
	held, perr := Peek(dir)
	if perr != nil || held == nil {
		return nil, false, err
	}
	if held.Target != target {
		return nil, false, fmt.Errorf("%w: %s is under maintenance, refusing to also touch %s", ErrLocked, held.Target, target)
	}
	if dir == "" {
		dir = Dir
	}
	return &Lock{path: filepath.Join(dir, lockName), Info: *held}, true, nil
}

// ---------- cluster-wide progress ----------

// Rolling tracks a multi-node operation so it can be picked up where it
// stopped. It is separate from State because each node's reboot owns its own
// state.json, which is deleted when that node completes.
type Rolling struct {
	Command string    `json:"command"`
	Order   []string  `json:"order"`
	Done    []string  `json:"done"`
	Holder  string    `json:"holder"`
	Started time.Time `json:"started"`
	// TalosVersion / TalosImage pin a rolling upgrade to one release, so a
	// continuation cannot quietly upgrade the remaining nodes to another.
	TalosVersion string `json:"talos_version,omitempty"`
	TalosImage   string `json:"talos_image,omitempty"`
}

const rollingName = "rolling.json"

func (r *Rolling) Remaining() []string {
	done := map[string]bool{}
	for _, d := range r.Done {
		done[d] = true
	}
	var out []string
	for _, n := range r.Order {
		if !done[n] {
			out = append(out, n)
		}
	}
	return out
}

func (r *Rolling) Save(dir string) error {
	if dir == "" {
		dir = Dir
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".rolling.json.tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, rollingName))
}

func LoadRolling(dir string) (*Rolling, error) {
	if dir == "" {
		dir = Dir
	}
	b, err := os.ReadFile(filepath.Join(dir, rollingName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var r Rolling
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("rolling.json is corrupt: %w", err)
	}
	return &r, nil
}

func RemoveRolling(dir string) error {
	if dir == "" {
		dir = Dir
	}
	err := os.Remove(filepath.Join(dir, rollingName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
