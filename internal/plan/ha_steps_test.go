package plan

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/TomayGo/pvectl/internal/pve/pvetest"
	"github.com/TomayGo/pvectl/internal/remote/remotetest"
	"github.com/TomayGo/pvectl/internal/state"
)

const haStatusPath = "/cluster/ha/status/manager_status"

// These tests pin down what ha-evacuate, its undo and restore do today, API
// call by API call and line by line, with the CRM played by a fake. The waits
// run on synctest's clock, so "5s" below is how long the code slept, not how
// long the test took.

func TestHAEvacuateSkipsOnlyWithoutHAResources(t *testing.T) {
	te := newTestEnv(t, CmdMaintEnter)
	skipFn := stepHAEvacuate().Skip

	te.pve.JSON("GET", "/cluster/ha/resources", `[]`)
	skip, reason, err := skipFn(context.Background(), te.Env)
	if err != nil || !skip || reason != "no HA resources are configured" {
		t.Errorf("nothing configured: skip=%v reason=%q err=%v", skip, reason, err)
	}

	// A maintenance this window already owns is never skipped: it still has
	// to be waited on, and ended.
	te.State.SetData(haStep, haOwnedKey, true)
	if skip, _, err := skipFn(context.Background(), te.Env); err != nil || skip {
		t.Errorf("owned maintenance: skip=%v err=%v", skip, err)
	}
	te.State.SetData(haStep, haOwnedKey, false)

	te.pve.JSON("GET", "/cluster/ha/resources", `[{"sid":"vm:100","type":"vm","state":"started"}]`)
	if skip, _, err := skipFn(context.Background(), te.Env); err != nil || skip {
		t.Errorf("resources configured: skip=%v err=%v", skip, err)
	}

	te.pve.Handle("GET", "/cluster/ha/resources", func(pvetest.Request) (any, error) {
		return nil, &pvetest.Error{Status: 500, Message: "no quorum"}
	})
	if skip, _, err := skipFn(context.Background(), te.Env); err == nil || skip {
		t.Errorf("an unreadable resource list must not skip: skip=%v err=%v", skip, err)
	}
}

func TestHAEvacuateRequestsMaintenanceAndWaits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		te := newTestEnv(t, CmdMaintEnter)
		// Ownership must already be on disk when the wait starts.
		var savedBeforeWaiting bool
		te.pve.Handle("GET", haStatusPath, evacuationReplies(te, &savedBeforeWaiting))

		start := time.Now()
		if err := stepHAEvacuate().Do(context.Background(), te.Env); err != nil {
			t.Fatal(err)
		}
		if got := elapsed(start); got != 5*time.Second {
			t.Errorf("waited %s, want 5s (one sleep between two polls)", got)
		}
		if got, want := te.commands(), []string{"ha-manager crm-command node-maintenance enable pve-1"}; !reflect.DeepEqual(got, want) {
			t.Errorf("commands %q, want %q", got, want)
		}
		if !savedBeforeWaiting {
			t.Error("ownership was not saved to state.json before the wait began")
		}
		if !haOwned(te.Env) {
			t.Error("ownership not recorded")
		}
		if got := toStrings(dataOf(te.Env, haStep, "migrated")); !reflect.DeepEqual(got, []string{"ct:200", "vm:100"}) {
			t.Errorf("migrated recorded as %v", got)
		}
		te.wantLines(t,
			"  ! ct:200 is a container: HA relocates it offline, so it will be briefly unavailable",
			"  HA will move ct:200, vm:100 where its rules allow",
			"  running ha-manager crm-command node-maintenance enable pve-1",
			"  pve-1 is in HA maintenance; no active HA resources remain on it",
			"    ct:200 is on pve-3 (started)",
			"    vm:100 is on pve-2 (started)",
		)
		te.wantWrites(t)
	})
}

// evacuationReplies plays the CRM through one evacuation: nothing requested,
// then the request taken with resources on the move, then done. On the first
// read of the wait loop it also looks in state.json for the ownership record.
func evacuationReplies(te *testEnv, saved *bool) pvetest.Handler {
	replies := []string{
		haStatus(nil, allOnline, "vm:100 pve-1 started", "ct:200 pve-1 started", "vm:300 pve-3 started"),
		haStatus([]string{"pve-1"}, allOnline, "vm:100 pve-1 migrate pve-2", "ct:200 pve-1 relocate pve-3", "vm:300 pve-3 started"),
		haStatus([]string{"pve-1"}, inMaintenance("pve-1"), "vm:100 pve-2 started", "ct:200 pve-3 started", "vm:300 pve-3 started"),
	}
	n := 0
	return func(pvetest.Request) (any, error) {
		if n == 1 {
			if st, err := state.LoadState(te.StateDir); err == nil && st != nil {
				v, _ := st.Step(haStep).Data[haOwnedKey].(bool)
				*saved = v
			}
		}
		r := replies[n]
		if n < len(replies)-1 {
			n++
		}
		return rawJSON(r), nil
	}
}

func TestHAEvacuateLeavesSomeoneElsesMaintenanceAlone(t *testing.T) {
	te := newTestEnv(t, CmdMaintEnter)
	te.pve.JSON("GET", haStatusPath, haStatus([]string{"pve-1"}, inMaintenance("pve-1"), "vm:100 pve-2 started"))
	if err := stepHAEvacuate().Do(context.Background(), te.Env); err != nil {
		t.Fatal(err)
	}
	if len(te.commands()) != 0 {
		t.Errorf("ha-manager was run: %v", te.commands())
	}
	if haOwned(te.Env) {
		t.Error("a maintenance someone else started must not become ours")
	}
	te.wantLines(t,
		"  pve-1 is already in HA maintenance; leaving it to whoever put it there",
		"  pve-1 is in HA maintenance; no active HA resources remain on it",
	)
}

// Re-running after a failed attempt finds the request already made, and keeps
// the ownership the first attempt recorded.
func TestHAEvacuateKeepsOwnershipFromAnEarlierAttempt(t *testing.T) {
	te := newTestEnv(t, CmdMaintEnter)
	te.State.SetData(haStep, haOwnedKey, true)
	te.State.SetData(haStep, "migrated", []string{"vm:100"})
	te.reload(t)
	te.pve.JSON("GET", haStatusPath, haStatus([]string{"pve-1"}, inMaintenance("pve-1"), "vm:100 pve-2 started"))
	if err := stepHAEvacuate().Do(context.Background(), te.Env); err != nil {
		t.Fatal(err)
	}
	if len(te.commands()) != 0 || !haOwned(te.Env) {
		t.Errorf("commands %v, owned %v", te.commands(), haOwned(te.Env))
	}
	te.wantLines(t,
		"  pve-1 is already in HA maintenance from an earlier attempt at this maintenance; keeping ownership",
		"  pve-1 is in HA maintenance; no active HA resources remain on it",
		"    vm:100 is on pve-2 (started)",
	)
}

func TestHAEvacuateRefusesADisarmedStack(t *testing.T) {
	te := newTestEnv(t, CmdMaintEnter)
	te.pve.JSON("GET", haStatusPath, `{"manager_status":{"disarm":{"mode":"freeze"},"node_status":{"pve-1":"online"}}}`)
	err := stepHAEvacuate().Do(context.Background(), te.Env)
	want := "the HA stack is disarmed, and the CRM ignores maintenance requests while it is; re-arm it (ha-manager arm-ha) and `pvectl resume`"
	if err == nil || err.Error() != want {
		t.Errorf("error %v", err)
	}
	if len(te.commands()) != 0 {
		t.Errorf("ha-manager was run: %v", te.commands())
	}
}

func TestHAEvacuateDoesNotClaimAFailedRequest(t *testing.T) {
	te := newTestEnv(t, CmdMaintEnter)
	te.pve.JSON("GET", haStatusPath, haStatus(nil, allOnline))
	te.cmd.Answer = func(remotetest.Call) remotetest.Result {
		return remotetest.Result{Stderr: "cluster not ready - no quorum?\n", Err: errors.New("exit status 2")}
	}
	err := stepHAEvacuate().Do(context.Background(), te.Env)
	want := "ha-manager crm-command node-maintenance enable pve-1: exit status 2: cluster not ready - no quorum?"
	if err == nil || err.Error() != want {
		t.Errorf("error %v, want %s", err, want)
	}
	if haOwned(te.Env) || dataOf(te.Env, haStep, haOwnedKey) != nil {
		t.Error("ownership recorded for a request that was never queued")
	}
}

func TestHAEvacuateTimesOutNamingWhatIsLeft(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		te := newTestEnv(t, CmdMaintEnter)
		te.Opts.TimeoutMigrate = 12 * time.Second
		te.pve.Sequence("GET", haStatusPath,
			haStatus(nil, allOnline, "vm:100 pve-1 started", "vm:101 pve-1 started"),
			haStatus([]string{"pve-1"}, allOnline, "vm:100 pve-1 started", "vm:101 pve-1 migrate pve-3"),
		)
		start := time.Now()
		err := stepHAEvacuate().Do(context.Background(), te.Env)
		want := `after 12s pve-1 is in HA state "online" (maintenance requested: true), active resources still on it: ` +
			`vm:100 (started), vm:101 (migrate pve-1 -> pve-3)` +
			` -- a strict node-affinity rule with no other available node keeps a resource in place (see the ha-affinity preflight check);` +
			" `pvectl resume` keeps waiting, `pvectl abort` ends the maintenance"
		if err == nil || err.Error() != want {
			t.Errorf("error:\n got %v\nwant %s", err, want)
		}
		// Polls at 0, 5, 10 and 15 s; the deadline is noticed on the first
		// poll after it.
		if got := elapsed(start); got != 15*time.Second {
			t.Errorf("gave up after %s, want 15s", got)
		}
		if !haOwned(te.Env) {
			t.Error("a timed-out evacuation still owns its maintenance, so abort can end it")
		}
	})
}

func TestHAEvacuateStopsWaitingWhenCancelledOrBlind(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		te := newTestEnv(t, CmdMaintEnter)
		te.pve.Sequence("GET", haStatusPath, haStatus(nil, allOnline, "vm:100 pve-1 started"),
			haStatus([]string{"pve-1"}, allOnline, "vm:100 pve-1 started"))
		ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
		defer cancel()
		if err := stepHAEvacuate().Do(ctx, te.Env); err != context.DeadlineExceeded {
			t.Errorf("error %v, want the context's", err)
		}
	})
	te := newTestEnv(t, CmdMaintEnter)
	n := 0
	te.pve.Handle("GET", haStatusPath, func(pvetest.Request) (any, error) {
		if n++; n == 1 {
			return rawJSON(haStatus(nil, allOnline, "vm:100 pve-1 started")), nil
		}
		return nil, &pvetest.Error{Status: 500, Message: "no quorum"}
	})
	if err := stepHAEvacuate().Do(context.Background(), te.Env); err == nil || !strings.Contains(err.Error(), "HTTP 500: no quorum") {
		t.Errorf("a status read failing mid-wait must end the wait: %v", err)
	}
}

// --dry-run says what it would run, records what it would move, and does not
// wait for a maintenance it never requested.
func TestHAEvacuateDryRun(t *testing.T) {
	te := newTestEnv(t, CmdMaintEnter)
	te.dryRun()
	te.pve.JSON("GET", haStatusPath, haStatus(nil, allOnline, "vm:100 pve-1 started"))
	if err := stepHAEvacuate().Do(context.Background(), te.Env); err != nil {
		t.Fatal(err)
	}
	if len(te.commands()) != 0 {
		t.Errorf("dry-run ran %v", te.commands())
	}
	if n := len(te.pve.Requests()); n != 1 {
		t.Errorf("%d API request(s), want the one status read", n)
	}
	te.wantLines(t,
		"  HA will move vm:100 where its rules allow",
		"  (dry-run) would run ha-manager crm-command node-maintenance enable pve-1",
	)
}

func TestLeaveHAMaintenanceEndsOnlyItsOwn(t *testing.T) {
	te := newTestEnv(t, CmdReboot)
	if err := leaveHAMaintenance(context.Background(), te.Env); err != nil {
		t.Fatal(err)
	}
	if len(te.commands()) != 0 || len(te.pve.Requests()) != 0 {
		t.Errorf("an unowned maintenance was touched: %v %v", te.commands(), te.pve.Calls())
	}
}

func TestLeaveHAMaintenanceWaitsForTheCRM(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		te := newTestEnv(t, CmdReboot)
		te.State.SetData(haStep, haOwnedKey, true)
		te.reload(t)
		back := `{"manager_status":{"node_request":{"pve-1":{}},"node_status":{"pve-1":"online"},"service_status":{` +
			`"vm:100":{"node":"pve-2","state":"started","maintenance_node":"pve-1"},` +
			`"vm:300":{"node":"pve-3","state":"started"}}}}`
		te.pve.Sequence("GET", haStatusPath, haStatus([]string{"pve-1"}, inMaintenance("pve-1"), "vm:100 pve-2 started"), back)

		start := time.Now()
		// The undo of ha-evacuate and restore both come through here.
		if err := stepHAEvacuate().Undo(context.Background(), te.Env); err != nil {
			t.Fatal(err)
		}
		if got := elapsed(start); got != 5*time.Second {
			t.Errorf("waited %s, want 5s", got)
		}
		if got, want := te.commands(), []string{"ha-manager crm-command node-maintenance disable pve-1"}; !reflect.DeepEqual(got, want) {
			t.Errorf("commands %q, want %q", got, want)
		}
		if haOwned(te.Env) || dataOf(te.Env, haStep, haOwnedKey) != false {
			t.Error("ownership must be recorded as ended (false), not removed")
		}
		te.wantLines(t,
			"  running ha-manager crm-command node-maintenance disable pve-1",
			`  HA maintenance ended on pve-1 (CRM state "online")`,
			"  HA moves vm:100 back to pve-1",
		)
	})
}

func TestLeaveHAMaintenanceGivesUpOnASilentCRM(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		te := newTestEnv(t, CmdReboot)
		te.State.SetData(haStep, haOwnedKey, true)
		te.pve.JSON("GET", haStatusPath, haStatus([]string{"pve-1"}, inMaintenance("pve-1")))
		start := time.Now()
		err := leaveHAMaintenance(context.Background(), te.Env)
		want := "the CRM has not taken the request to end maintenance on pve-1 after 2m0s; check `ha-manager status`"
		if err == nil || err.Error() != want {
			t.Errorf("error %v", err)
		}
		if got := elapsed(start); got != 2*time.Minute+5*time.Second {
			t.Errorf("gave up after %s, want 2m5s", got)
		}
		if !haOwned(te.Env) {
			t.Error("still owned, so a retry ends it")
		}
	})
}

func TestLeaveHAMaintenanceDryRunAndFailure(t *testing.T) {
	te := newTestEnv(t, CmdReboot)
	te.State.SetData(haStep, haOwnedKey, true)
	te.dryRun()
	if err := leaveHAMaintenance(context.Background(), te.Env); err != nil {
		t.Fatal(err)
	}
	if len(te.commands()) != 0 || len(te.pve.Requests()) != 0 {
		t.Errorf("dry-run touched the cluster: %v %v", te.commands(), te.pve.Calls())
	}
	// As today: the in-memory record is cleared even though nothing ran. A dry
	// run never saves it.
	if haOwned(te.Env) {
		t.Error("dry-run leaves the in-memory record cleared")
	}
	te.wantLines(t, "  (dry-run) would run ha-manager crm-command node-maintenance disable pve-1")

	te = newTestEnv(t, CmdReboot)
	te.State.SetData(haStep, haOwnedKey, true)
	te.cmd.Answer = func(remotetest.Call) remotetest.Result { return remotetest.Result{Err: errors.New("exit status 1")} }
	if err := leaveHAMaintenance(context.Background(), te.Env); err == nil ||
		err.Error() != "ha-manager crm-command node-maintenance disable pve-1: exit status 1" {
		t.Errorf("error %v", err)
	}
	if !haOwned(te.Env) {
		t.Error("a failed disable must leave the maintenance owned")
	}
}

func TestRestoreHAMaintenance(t *testing.T) {
	// The ownership record lives on the ha-evacuate step, so these use a
	// reboot's state, which has both that step and restore.

	// Recorded and already ended: nothing to do, nothing asked.
	te := newTestEnv(t, CmdReboot)
	te.State.SetData(haStep, haOwnedKey, false)
	if err := restoreHAMaintenance(context.Background(), te.Env); err != nil {
		t.Fatal(err)
	}
	if len(te.pve.Requests()) != 0 || len(te.commands()) != 0 {
		t.Errorf("touched the cluster: %v %v", te.pve.Calls(), te.commands())
	}

	// No record, node in maintenance: someone else's. Warn, leave it.
	te = newTestEnv(t, CmdReboot)
	te.pve.JSON("GET", haStatusPath, haStatus([]string{"pve-1"}, inMaintenance("pve-1")))
	if err := restoreHAMaintenance(context.Background(), te.Env); err != nil {
		t.Fatal(err)
	}
	if len(te.commands()) != 0 {
		t.Errorf("ended a maintenance it has no record of: %v", te.commands())
	}
	te.wantLines(t, "  ! pve-1 is in HA maintenance, but this maintenance has no record of starting it; leaving it alone -- "+
		"end it with `ha-manager crm-command node-maintenance disable pve-1` or `pvectl doctor --fix`")

	// No record, not in maintenance: silent.
	te = newTestEnv(t, CmdReboot)
	te.pve.JSON("GET", haStatusPath, haStatus(nil, allOnline))
	if err := restoreHAMaintenance(context.Background(), te.Env); err != nil || len(te.lines()) != 0 {
		t.Errorf("err=%v lines=%v", err, te.lines())
	}

	// No record and the status unreadable: a warning, not a failed restore.
	te = newTestEnv(t, CmdReboot)
	te.pve.Handle("GET", haStatusPath, func(pvetest.Request) (any, error) {
		return nil, &pvetest.Error{Status: 500, Message: "no quorum"}
	})
	if err := restoreHAMaintenance(context.Background(), te.Env); err != nil {
		t.Errorf("an unreadable status must not fail restore: %v", err)
	}
	te.wantLines(t, "  ! cannot read HA manager status: GET /cluster/ha/status/manager_status: HTTP 500: no quorum")

	// Owned: ends it.
	te = newTestEnv(t, CmdReboot)
	te.State.SetData(haStep, haOwnedKey, true)
	te.pve.JSON("GET", haStatusPath, haStatus(nil, allOnline))
	if err := restoreHAMaintenance(context.Background(), te.Env); err != nil {
		t.Fatal(err)
	}
	if got := te.commands(); len(got) != 1 || !strings.HasSuffix(got[0], "disable pve-1") {
		t.Errorf("commands %v", got)
	}
}

// A timed-out evacuation is "failed", not "done" -- and abort still has to end
// the maintenance it requested.
func TestAbortEndsATimedOutEvacuation(t *testing.T) {
	te := newTestEnv(t, CmdMaintEnter)
	r, err := ResumeRunner(te.Env, te.State)
	if err != nil {
		t.Fatal(err)
	}
	te.State.Set(lockStep, state.StepDone, nil)
	te.State.Set(haStep, state.StepFailed, errors.New("timed out"))
	te.State.SetData(haStep, haOwnedKey, true)
	te.pve.JSON("GET", haStatusPath, haStatus(nil, allOnline))
	te.KeepLock = false
	if err := r.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := te.commands(); len(got) != 1 || !strings.HasSuffix(got[0], "disable pve-1") {
		t.Errorf("commands %v", got)
	}
	if got := te.State.Step(haStep).Status; got != state.StepUndone {
		t.Errorf("ha-evacuate is %s, want undone", got)
	}
}
