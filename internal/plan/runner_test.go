package plan

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	plog "github.com/TomayGo/pvectl/internal/log"
	"github.com/TomayGo/pvectl/internal/state"
)

// runnerEnv is the little NewRunner needs: a config, a target and somewhere
// to keep state.json.
func runnerEnv(t *testing.T) (*Env, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	return &Env{Cfg: fixtureConfig(), Target: "pve-1", Self: "pve-2", Log: plog.To(&out),
		Opts: DefaultOptions(), StateDir: t.TempDir()}, &out
}

func stepStatuses(st *state.State) []string {
	var out []string
	for _, s := range st.Steps {
		out = append(out, s.Name+"="+string(s.Status))
	}
	return out
}

func loadState(t *testing.T, dir string) *state.State {
	t.Helper()
	st, err := state.LoadState(dir)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	return st
}

// ---------- NewRunner ----------

func TestNewRunnerStartsAFreshRun(t *testing.T) {
	e, out := runnerEnv(t)
	r, err := NewRunner(e, CmdShutdown)
	if err != nil {
		t.Fatal(err)
	}
	if r.Name != CmdShutdown || r.Continued || !e.KeepLock {
		t.Errorf("name=%q continued=%v keepLock=%v", r.Name, r.Continued, e.KeepLock)
	}
	st := loadState(t, e.StateDir)
	if st == nil {
		t.Fatal("state.json was not written")
	}
	if st.Command != CmdShutdown || st.Target != "pve-1" || st.Holder != "pve-2" || st.Finished || st.Version != 1 {
		t.Errorf("state %+v", st)
	}
	want := []string{"lock=pending", "preflight=pending", "silence=pending", "ceph-flags=pending", "ha-evacuate=pending",
		"k8s-drain=pending", "talos-stop=pending", "guest-stop=pending", "host-shutdown=pending", "wait-down=pending"}
	if got := stepStatuses(st); !reflect.DeepEqual(got, want) {
		t.Errorf("steps %v", got)
	}
	if out.Len() != 0 {
		t.Errorf("a fresh run printed: %s", out)
	}

	// A command that ends with the node back in service gives the lock up.
	e, _ = runnerEnv(t)
	if _, err := NewRunner(e, CmdReboot); err != nil || e.KeepLock {
		t.Errorf("reboot: err=%v keepLock=%v", err, e.KeepLock)
	}
	if _, err := NewRunner(e, "nonsense"); err == nil || err.Error() != `unknown command "nonsense" in state.json` {
		t.Errorf("unknown command: %v", err)
	}
}

// start and maint exit close the window another command opened: they keep
// its state -- and with it the record of what to put back -- and add their
// own steps to it.
func TestNewRunnerContinuesTheOpenWindow(t *testing.T) {
	e, out := runnerEnv(t)
	opened := state.NewState(e.StateDir, CmdShutdown, "pve-1", "pve-3", []string{"lock", "ceph-flags", "host-shutdown"})
	for _, s := range []string{"lock", "ceph-flags", "host-shutdown"} {
		opened.Set(s, state.StepDone, nil)
	}
	opened.SetData("ceph-flags", "set_by_us", []string{"noout"})
	opened.Finished = true
	if err := opened.Save(); err != nil {
		t.Fatal(err)
	}

	r, err := NewRunner(e, CmdStart)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Continued || e.KeepLock {
		t.Errorf("continued=%v keepLock=%v", r.Continued, e.KeepLock)
	}
	if got := strings.TrimSpace(out.String()); got != "continuing the maintenance window opened by `shutdown pve-1`" {
		t.Errorf("printed %q", got)
	}
	st := loadState(t, e.StateDir)
	if st.Command != CmdStart || st.Finished || st.Holder != "pve-3" {
		t.Errorf("command=%q finished=%v holder=%q", st.Command, st.Finished, st.Holder)
	}
	// lock is already done from the shutdown, so start resumes past it.
	want := []string{"lock=done", "ceph-flags=done", "host-shutdown=done", "wake=pending", "wait-up=pending",
		"restore=pending", "verify=pending"}
	if got := stepStatuses(st); !reflect.DeepEqual(got, want) {
		t.Errorf("steps %v\nwant  %v", got, want)
	}
	if got := toStrings(st.Step("ceph-flags").Data["set_by_us"]); !reflect.DeepEqual(got, []string{"noout"}) {
		t.Errorf("rollback data lost: %v", got)
	}
}

func TestNewRunnerClosingWithoutAWindow(t *testing.T) {
	e, out := runnerEnv(t)
	r, err := NewRunner(e, CmdMaintExit)
	if err != nil {
		t.Fatal(err)
	}
	if r.Continued {
		t.Error("nothing to continue")
	}
	if got := strings.TrimSpace(out.String()); got != "! no earlier state found for pve-1; restore will only do what it can infer" {
		t.Errorf("printed %q", got)
	}
	if got := stepStatuses(loadState(t, e.StateDir)); !reflect.DeepEqual(got, []string{"lock=pending", "restore=pending", "verify=pending"}) {
		t.Errorf("steps %v", got)
	}

	// A window open for another node is not this one's to continue. As the
	// code stands the closing command then replaces that state with its own.
	e, out = runnerEnv(t)
	other := state.NewState(e.StateDir, CmdShutdown, "pve-3", "pve-2", []string{"lock"})
	other.Finished = true
	if err := other.Save(); err != nil {
		t.Fatal(err)
	}
	r, err = NewRunner(e, CmdStart)
	if err != nil || r.Continued {
		t.Fatalf("err=%v continued=%v", err, r.Continued)
	}
	if out.Len() != 0 {
		t.Errorf("printed %q", out)
	}
	if st := loadState(t, e.StateDir); st.Target != "pve-1" || st.Command != CmdStart {
		t.Errorf("state is %s %s", st.Command, st.Target)
	}
}

// Re-running a command over an unfinished run of the same node starts every
// step again but keeps what the abandoned run recorded for its undo.
func TestNewRunnerDiscardsAnUnfinishedRunButKeepsItsRollbackData(t *testing.T) {
	e, out := runnerEnv(t)
	old := state.NewState(e.StateDir, CmdShutdown, "pve-1", "pve-2", []string{"lock", "silence", "ceph-flags", "guest-stop"})
	old.Set("lock", state.StepDone, nil)
	old.Set("ceph-flags", state.StepFailed, errors.New("boom"))
	old.SetData("silence", "ids", []string{"abc"})
	old.SetData("ceph-flags", "set_by_us", []string{"noout"})
	old.SetData("guest-stop", "stopped", []int{103})
	if err := old.Save(); err != nil {
		t.Fatal(err)
	}

	r, err := NewRunner(e, CmdMaintEnter)
	if err != nil || r.Continued {
		t.Fatalf("err=%v continued=%v", err, r.Continued)
	}
	if got := strings.TrimSpace(out.String()); got != "! discarding an unfinished shutdown pve-1 state; run `pvectl abort` first if you meant to roll it back" {
		t.Errorf("printed %q", got)
	}
	st := loadState(t, e.StateDir)
	for _, s := range st.Steps {
		if s.Status != state.StepPending {
			t.Errorf("%s is %s, want pending", s.Name, s.Status)
		}
	}
	if got := toStrings(st.Step("silence").Data["ids"]); !reflect.DeepEqual(got, []string{"abc"}) {
		t.Errorf("silence ids: %v", got)
	}
	if got := toStrings(st.Step("ceph-flags").Data["set_by_us"]); !reflect.DeepEqual(got, []string{"noout"}) {
		t.Errorf("ceph flags: %v", got)
	}
	if st.Step("guest-stop") != nil {
		t.Error("maint-enter has no guest-stop step and must not gain one")
	}

	// An unfinished run of another node is discarded with the same warning,
	// and nothing of it is carried over.
	e, out = runnerEnv(t)
	other := state.NewState(e.StateDir, CmdReboot, "pve-3", "pve-2", []string{"ceph-flags"})
	other.SetData("ceph-flags", "set_by_us", []string{"noout"})
	if err := other.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRunner(e, CmdMaintEnter); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "discarding an unfinished reboot pve-3 state") {
		t.Errorf("printed %q", out)
	}
	if d := loadState(t, e.StateDir).Step("ceph-flags").Data; len(d) != 0 {
		t.Errorf("another node's rollback data was carried over: %v", d)
	}

	// A finished run is simply replaced.
	e, out = runnerEnv(t)
	done := state.NewState(e.StateDir, CmdReboot, "pve-1", "pve-2", []string{"ceph-flags"})
	done.SetData("ceph-flags", "set_by_us", []string{"noout"})
	done.Finished = true
	if err := done.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRunner(e, CmdReboot); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("printed %q", out)
	}
	if d := loadState(t, e.StateDir).Step("ceph-flags").Data; len(d) != 0 {
		t.Errorf("a finished run's data was carried over: %v", d)
	}
}

func TestNewRunnerAndABrokenStateFile(t *testing.T) {
	// Unparseable: refuse, rather than overwrite what might be the only
	// record of an open window.
	e, _ := runnerEnv(t)
	if err := os.WriteFile(filepath.Join(e.StateDir, "state.json"), []byte("{not json"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRunner(e, CmdReboot); err == nil || !strings.HasPrefix(err.Error(), "state.json is corrupt: ") {
		t.Errorf("error %v", err)
	}

	// Readable but from another version: warn and start over.
	e, out := runnerEnv(t)
	if err := os.WriteFile(filepath.Join(e.StateDir, "state.json"),
		[]byte(`{"version":99,"command":"shutdown","target":"pve-1","steps":[]}`), 0o640); err != nil {
		t.Fatal(err)
	}
	r, err := NewRunner(e, CmdStart)
	if err != nil || r.Continued {
		t.Fatalf("err=%v continued=%v", err, r.Continued)
	}
	want := "  ! existing state.json is unusable (state.json has version 99, this pvectl writes 1); starting a new one\n" +
		"  ! no earlier state found for pve-1; restore will only do what it can infer\n"
	if out.String() != want {
		t.Errorf("printed %q", out)
	}
	if st := loadState(t, e.StateDir); st.Version != 1 || st.Command != CmdStart {
		t.Errorf("state %+v", st)
	}

	// Nowhere to write: the run does not start.
	e, _ = runnerEnv(t)
	e.StateDir = filepath.Join(e.StateDir, "gone")
	if _, err := NewRunner(e, CmdReboot); err == nil || !strings.HasPrefix(err.Error(), "persist run state: write state: ") {
		t.Errorf("error %v", err)
	}
}

// An upgrade records what was asked for, because resume and abort never see
// the flags; and it clears what an earlier attempt resolved.
func TestNewRunnerRecordsTheUpgradeRequest(t *testing.T) {
	e, _ := runnerEnv(t)
	e.Opts.TalosVersion = "v1.13.7"
	if _, err := NewRunner(e, CmdUpgrade); err != nil {
		t.Fatal(err)
	}
	d := loadState(t, e.StateDir).Step("talos-upgrade").Data
	want := map[string]any{"requested_version": "v1.13.7", "requested_image": "", "image": "", "version": ""}
	if !reflect.DeepEqual(d, want) {
		t.Errorf("talos-upgrade data %v", d)
	}
}

// --dry-run walks the same plan and writes nothing.
func TestNewRunnerDryRunWritesNothing(t *testing.T) {
	e, _ := runnerEnv(t)
	e.Opts.DryRun = true
	r, err := NewRunner(e, CmdReboot)
	if err != nil {
		t.Fatal(err)
	}
	if st := loadState(t, e.StateDir); st != nil {
		t.Errorf("dry-run wrote state.json: %+v", st)
	}
	if e.State == nil || len(e.State.Steps) != len(r.Steps) {
		t.Error("the in-memory state must still be there for the walk")
	}
}

func TestResumeRunnerRebuildsFromState(t *testing.T) {
	e, _ := runnerEnv(t)
	st := state.NewState(e.StateDir, CmdShutdown, "pve-1", "pve-2", nil)
	r, err := ResumeRunner(e, st)
	if err != nil {
		t.Fatal(err)
	}
	if r.Name != CmdShutdown || e.State != st || !e.KeepLock || r.Continued {
		t.Errorf("name=%q keepLock=%v continued=%v", r.Name, e.KeepLock, r.Continued)
	}
	st.Command = "nonsense"
	if _, err := ResumeRunner(e, st); err == nil {
		t.Error("an unknown recorded command must be refused")
	}
}

// ---------- Run ----------

// record builds a step that notes when it runs.
func record(log *[]string, name string, err error) Step {
	return Step{
		Name: name,
		Desc: "do " + name,
		Do:   func(ctx context.Context, e *Env) error { *log = append(*log, "do "+name); return err },
		Undo: func(ctx context.Context, e *Env) error { *log = append(*log, "undo "+name); return nil },
	}
}

func runnerWith(t *testing.T, steps ...Step) (*Runner, *bytes.Buffer) {
	t.Helper()
	e, out := runnerEnv(t)
	r := &Runner{Env: e, Steps: steps, Name: CmdReboot}
	e.State = state.NewState(e.StateDir, CmdReboot, "pve-1", "pve-2", r.StepNames())
	return r, out
}

func TestRunWalksEveryStepAndRecordsIt(t *testing.T) {
	var ran []string
	skipped := Step{Name: "wake", Desc: "send Wake-on-LAN",
		Skip: func(ctx context.Context, e *Env) (bool, string, error) { return true, "pve-1 is already up", nil },
		Do:   func(ctx context.Context, e *Env) error { ran = append(ran, "do wake"); return nil }}
	r, out := runnerWith(t, record(&ran, "lock", nil), skipped, Step{Name: "noop", Desc: "nothing to do"}, record(&ran, "verify", nil))

	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ran, []string{"do lock", "do verify"}) {
		t.Errorf("ran %v", ran)
	}
	st := loadState(t, r.Env.StateDir)
	if !st.Finished {
		t.Error("state.json does not say finished")
	}
	if got := stepStatuses(st); !reflect.DeepEqual(got, []string{"lock=done", "wake=skipped", "noop=done", "verify=done"}) {
		t.Errorf("steps %v", got)
	}
	want := "\n==> lock: do lock\n" +
		"\n==> wake: send Wake-on-LAN\n" +
		"  already satisfied: pve-1 is already up\n" +
		"\n==> noop: nothing to do\n" +
		"\n==> verify: do verify\n"
	if out.String() != want {
		t.Errorf("printed:\n%s\nwant:\n%s", out, want)
	}
}

func TestRunStopsAtTheFirstFailure(t *testing.T) {
	var ran []string
	boom := errors.New("PDB blocks eviction")
	r, out := runnerWith(t, record(&ran, "lock", nil), record(&ran, "k8s-drain", boom), record(&ran, "talos-stop", nil))

	err := r.Run(context.Background())
	if !errors.Is(err, boom) || err.Error() != "step k8s-drain: PDB blocks eviction" {
		t.Errorf("error %v", err)
	}
	if !reflect.DeepEqual(ran, []string{"do lock", "do k8s-drain"}) {
		t.Errorf("ran %v", ran)
	}
	st := loadState(t, r.Env.StateDir)
	if st.Finished {
		t.Error("a failed run must not be finished")
	}
	if got := stepStatuses(st); !reflect.DeepEqual(got, []string{"lock=done", "k8s-drain=failed", "talos-stop=pending"}) {
		t.Errorf("steps %v", got)
	}
	if got := st.Step("k8s-drain").Error; got != "PDB blocks eviction" {
		t.Errorf("recorded error %q", got)
	}
	if got := st.FirstIncomplete(); got != "k8s-drain" {
		t.Errorf("resume would start at %q", got)
	}
	want := "\n==> lock: do lock\n" +
		"\n==> k8s-drain: do k8s-drain\n" +
		"  x step k8s-drain failed: PDB blocks eviction\n" +
		"\n" +
		"The run stopped at step \"k8s-drain\". Nothing further was attempted.\n" +
		"  fix the cause and continue:  pvectl resume\n" +
		"  or roll back what was done:  pvectl abort\n"
	if out.String() != want {
		t.Errorf("printed:\n%s\nwant:\n%s", out, want)
	}
}

// resume is Run again over the same state: done and skipped steps are passed
// over, the failed one is retried.
func TestRunResumesPastCompletedSteps(t *testing.T) {
	var ran []string
	r, _ := runnerWith(t, record(&ran, "lock", nil), record(&ran, "wake", nil), record(&ran, "k8s-drain", nil), record(&ran, "verify", nil))
	st := r.Env.State
	st.Set("lock", state.StepDone, nil)
	st.Set("wake", state.StepSkipped, nil)
	st.Set("k8s-drain", state.StepFailed, errors.New("earlier failure"))

	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ran, []string{"do k8s-drain", "do verify"}) {
		t.Errorf("ran %v", ran)
	}
	// Marking the step running again clears the error text of the failed
	// attempt, so a retried step that succeeds is recorded clean.
	if got := loadState(t, r.Env.StateDir).Step("k8s-drain"); got.Status != state.StepDone || got.Error != "" {
		t.Errorf("k8s-drain is %s with error %q", got.Status, got.Error)
	}
}

func TestRunFailsWhenTheSkipCheckCannotTell(t *testing.T) {
	blind := errors.New("cannot determine whether talos-cp-1 is running")
	did := false
	r, _ := runnerWith(t, Step{Name: "talos-stop", Desc: "shut down the Talos VM",
		Skip: func(ctx context.Context, e *Env) (bool, string, error) { return false, "", blind },
		Do:   func(ctx context.Context, e *Env) error { did = true; return nil }})
	if err := r.Run(context.Background()); !errors.Is(err, blind) {
		t.Errorf("error %v", err)
	}
	if did {
		t.Error("Do ran although Skip could not tell")
	}
	if got := r.Env.State.Step("talos-stop").Status; got != state.StepFailed {
		t.Errorf("status %s", got)
	}
}

// The step is marked running on disk before Do starts, so a run that dies in
// it is found by resume and by a rollback that undoes started steps.
func TestRunMarksAStepRunningBeforeDoingIt(t *testing.T) {
	var during string
	r, _ := runnerWith(t)
	r.Steps = []Step{{Name: "ha-evacuate", Desc: "evacuate", Do: func(ctx context.Context, e *Env) error {
		during = string(loadState(t, e.StateDir).Step("ha-evacuate").Status)
		return nil
	}}}
	r.Env.State = state.NewState(r.Env.StateDir, CmdReboot, "pve-1", "pve-2", r.StepNames())
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if during != "running" {
		t.Errorf("state.json said %q while the step ran", during)
	}
}

// ---------- Rollback ----------

func TestRollbackUndoesCompletedStepsNewestFirst(t *testing.T) {
	var ran []string
	undoFails := errors.New("uncordon refused")
	drain := record(&ran, "k8s-drain", nil)
	drain.Undo = func(ctx context.Context, e *Env) error { ran = append(ran, "undo k8s-drain"); return undoFails }
	power := record(&ran, "host-reboot", nil)
	power.Irreversible, power.Undo = true, nil
	preflight := record(&ran, "preflight", nil)
	preflight.Undo = nil
	started := record(&ran, "ha-evacuate", nil)
	started.UndoIfStarted = true
	r, out := runnerWith(t, record(&ran, "lock", nil), preflight, record(&ran, "ceph-flags", nil), started,
		drain, record(&ran, "talos-stop", nil), power, record(&ran, "wait-up", nil))
	st := r.Env.State
	for _, s := range []string{"lock", "preflight", "ceph-flags", "k8s-drain", "host-reboot"} {
		st.Set(s, state.StepDone, nil)
	}
	st.Set("ha-evacuate", state.StepFailed, errors.New("timed out"))
	st.Set("talos-stop", state.StepFailed, errors.New("still up")) // failed, and does not ask to be undone
	st.Set("gone", state.StepDone, nil)                            // recorded by a build that had this step

	err := r.Rollback(context.Background())
	if !errors.Is(err, undoFails) || err.Error() != "undo k8s-drain: uncordon refused" {
		t.Errorf("error %v", err)
	}
	if want := []string{"undo k8s-drain", "undo ha-evacuate", "undo ceph-flags", "undo lock"}; !reflect.DeepEqual(ran, want) {
		t.Errorf("ran %v\nwant %v", ran, want)
	}
	saved := loadState(t, r.Env.StateDir)
	want := []string{"lock=undone", "preflight=done", "ceph-flags=undone", "ha-evacuate=undone", "k8s-drain=done",
		"talos-stop=failed", "host-reboot=done", "wait-up=pending", "gone=done"}
	if got := stepStatuses(saved); !reflect.DeepEqual(got, want) {
		t.Errorf("steps %v\nwant  %v", got, want)
	}
	wantOut := "  ! step gone is not part of this plan; skipping its rollback\n" +
		"  ! step host-reboot cannot be undone (do host-reboot); leaving it as-is\n" +
		"\n==> undo:k8s-drain: roll back do k8s-drain\n" +
		"  x rollback of k8s-drain failed: uncordon refused\n" +
		"\n==> undo:ha-evacuate: roll back do ha-evacuate\n" +
		"\n==> undo:ceph-flags: roll back do ceph-flags\n" +
		"\n==> undo:lock: roll back do lock\n"
	if out.String() != wantOut {
		t.Errorf("printed:\n%s\nwant:\n%s", out, wantOut)
	}
}

func TestRollbackWithNothingDone(t *testing.T) {
	var ran []string
	r, out := runnerWith(t, record(&ran, "lock", nil))
	r.Env.State.Set("lock", state.StepSkipped, nil)
	if err := r.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 0 || out.String() != "  nothing to roll back\n" {
		t.Errorf("ran %v, printed %q", ran, out)
	}
}

// Describe is what `pvectl plan` prints.
func TestDescribe(t *testing.T) {
	e, out := runnerEnv(t)
	steps, err := StepsFor(CmdMaintEnter, e)
	if err != nil {
		t.Fatal(err)
	}
	steps = append(steps, stepHostPower("shutdown"))
	(&Runner{Env: e, Steps: steps, Name: CmdMaintEnter}).Describe()
	want := `Plan: maint-enter pve-1
#   step             undo     description
------------------------------------------------------------------------------
1   lock             yes      hold the cluster-wide maintenance lock
2   preflight        -        read-only safety checks
3   silence          yes      silence Alertmanager for the node and announce on Discord
4   ceph-flags       yes      set ceph noout, norebalance
5   ha-evacuate      yes      put the node in HA maintenance; HA moves its resources off
6   k8s-drain        yes      cordon and drain the Talos node
7   host-shutdown    NO       power off the host
`
	if out.String() != want {
		t.Errorf("printed:\n%s\nwant:\n%s", out, want)
	}
}
