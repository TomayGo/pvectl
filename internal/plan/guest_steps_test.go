package plan

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/TomayGo/pvectl/internal/config"
	"github.com/TomayGo/pvectl/internal/pve/pvetest"
)

// guestEnv is a shutdown of pve-1 with three guests configured to be stopped:
// a running VM with a warning, a container, and a VM that is not on the node.
func guestEnv(t *testing.T) *testEnv {
	t.Helper()
	te := newTestEnv(t, CmdShutdown)
	te.Node.StopGuests = []config.Guest{
		{ID: 103, Warn: "the Wi-Fi drops"},
		{ID: 105, Type: "lxc"},
		{ID: 107},
	}
	te.pve.JSON("GET", "/cluster/resources", `[
		{"type":"qemu","node":"pve-1","vmid":103,"name":"ap","status":"running"},
		{"type":"lxc","node":"pve-1","vmid":105,"name":"dns","status":"running"},
		{"type":"qemu","node":"pve-2","vmid":107,"name":"elsewhere","status":"running"},
		{"type":"qemu","node":"pve-1","vmid":101,"name":"talos-cp-1","status":"running"}]`)
	return te
}

func taskStatusPath(upid string) string { return "/nodes/pve-1/tasks/" + upid + "/status" }

func TestGuestStopSkipsWithoutConfiguredGuests(t *testing.T) {
	te := newTestEnv(t, CmdShutdown)
	skip, reason, err := stepGuestStop().Skip(context.Background(), te.Env)
	if err != nil || !skip || reason != "no guests configured for pve-1" {
		t.Errorf("skip=%v reason=%q err=%v", skip, reason, err)
	}
	te = guestEnv(t)
	if skip, _, _ := stepGuestStop().Skip(context.Background(), te.Env); skip {
		t.Error("configured guests must not be skipped")
	}
}

func TestGuestStopShutsDownWhatIsRunning(t *testing.T) {
	te := guestEnv(t)
	te.pve.Static("POST", "/nodes/pve-1/qemu/103/status/shutdown", "UPID:103")
	te.pve.Static("POST", "/nodes/pve-1/lxc/105/status/shutdown", "UPID:105")
	te.pve.JSON("GET", taskStatusPath("UPID:103"), `{"status":"stopped","exitstatus":"OK"}`)
	te.pve.JSON("GET", taskStatusPath("UPID:105"), `{"status":"stopped","exitstatus":"OK"}`)

	if err := stepGuestStop().Do(context.Background(), te.Env); err != nil {
		t.Fatal(err)
	}
	// --timeout-guest (3m) is the graceful window when nothing forces.
	te.wantWrites(t,
		"POST /nodes/pve-1/qemu/103/status/shutdown timeout=180",
		"POST /nodes/pve-1/lxc/105/status/shutdown timeout=180",
	)
	te.wantLines(t,
		"  ! stopping 103 ap: the Wi-Fi drops",
		"  guest 103 ap stopped",
		"  guest 105 dns stopped",
		"  ! guest 107 is not on pve-1; skipping",
	)
	if got := dataOf(te.Env, guestStopStep, "stopped"); !reflect.DeepEqual(got, []int{103, 105}) {
		t.Errorf("stopped recorded as %#v", got)
	}
}

// A guest that is already stopped is left out of the record, so the undo does
// not start something that was off before the maintenance.
func TestGuestStopLeavesStoppedGuestsOutOfTheRecord(t *testing.T) {
	te := guestEnv(t)
	te.pve.JSON("GET", "/cluster/resources", `[
		{"type":"qemu","node":"pve-1","vmid":103,"name":"ap","status":"stopped"},
		{"type":"lxc","node":"pve-1","vmid":105,"name":"dns","status":"paused"}]`)
	if err := stepGuestStop().Do(context.Background(), te.Env); err != nil {
		t.Fatal(err)
	}
	te.wantWrites(t)
	te.wantLines(t,
		"  guest 103 ap is already stopped",
		"  guest 105 dns is already paused",
		"  ! guest 107 is not on pve-1; skipping",
	)
	if got := dataOf(te.Env, guestStopStep, "stopped"); got == nil || len(got.([]int)) != 0 {
		t.Errorf("stopped recorded as %#v, want an empty list", got)
	}
}

func TestGuestStopFailsRatherThanForcingByDefault(t *testing.T) {
	te := guestEnv(t)
	te.pve.Static("POST", "/nodes/pve-1/qemu/103/status/shutdown", "UPID:103")
	te.pve.JSON("GET", taskStatusPath("UPID:103"), `{"status":"stopped","exitstatus":"VM quit/powerdown failed - got timeout"}`)
	err := stepGuestStop().Do(context.Background(), te.Env)
	want := `guest 103 did not shut down within 3m0s: task UPID:103 finished with "VM quit/powerdown failed - got timeout" (use --force-stop-after to hard-stop)`
	if err == nil || err.Error() != want {
		t.Errorf("error:\n got %v\nwant %s", err, want)
	}
	te.wantWrites(t, "POST /nodes/pve-1/qemu/103/status/shutdown timeout=180")
	// Nothing is recorded when the step fails partway: the record is written
	// once, at the end.
	if got := dataOf(te.Env, guestStopStep, "stopped"); got != nil {
		t.Errorf("stopped recorded as %#v after a failure", got)
	}
}

// --force-stop-after is both the graceful window and the permission to pull
// the plug when it runs out.
func TestGuestStopForcesAfterTheDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		te := guestEnv(t)
		te.Node.StopGuests = te.Node.StopGuests[:1]
		te.Opts.ForceStopAfter = time.Minute
		te.pve.Static("POST", "/nodes/pve-1/qemu/103/status/shutdown", "UPID:103")
		te.pve.JSON("GET", taskStatusPath("UPID:103"), `{"status":"running"}`)
		te.pve.Static("POST", "/nodes/pve-1/qemu/103/status/stop", "UPID:103-stop")
		te.pve.Sequence("GET", taskStatusPath("UPID:103-stop"), `{"status":"running"}`, `{"status":"stopped","exitstatus":"OK"}`)

		start := time.Now()
		if err := stepGuestStop().Do(context.Background(), te.Env); err != nil {
			t.Fatal(err)
		}
		// 60s graceful + 30s slack, noticed on the 2s poll after it, then one
		// 2s poll of the stop task.
		if got := elapsed(start); got != 94*time.Second {
			t.Errorf("took %s, want 1m34s", got)
		}
		te.wantWrites(t,
			"POST /nodes/pve-1/qemu/103/status/shutdown timeout=60",
			"POST /nodes/pve-1/qemu/103/status/stop",
		)
		te.wantLines(t,
			"  ! stopping 103 ap: the Wi-Fi drops",
			"  ! guest 103 did not shut down cleanly (task UPID:103 still running after 1m30s); forcing stop",
			"  guest 103 ap stopped",
		)
	})
}

func TestGuestStopReportsAPIFailures(t *testing.T) {
	te := guestEnv(t)
	te.pve.Handle("GET", "/cluster/resources", func(pvetest.Request) (any, error) {
		return nil, &pvetest.Error{Status: 500, Message: "boom"}
	})
	if err := stepGuestStop().Do(context.Background(), te.Env); err == nil || !strings.HasPrefix(err.Error(), "list guests on pve-1: ") {
		t.Errorf("error %v", err)
	}

	te = guestEnv(t)
	te.pve.Handle("POST", "/nodes/pve-1/qemu/103/status/shutdown", func(pvetest.Request) (any, error) {
		return nil, &pvetest.Error{Status: 500, Message: "VM is locked (backup)"}
	})
	err := stepGuestStop().Do(context.Background(), te.Env)
	if want := "shutdown guest 103: POST /nodes/pve-1/qemu/103/status/shutdown: HTTP 500: VM is locked (backup)"; err == nil || err.Error() != want {
		t.Errorf("error %v, want %s", err, want)
	}

	te = guestEnv(t)
	te.Opts.ForceStopAfter = time.Second
	te.pve.Static("POST", "/nodes/pve-1/qemu/103/status/shutdown", "UPID:103")
	te.pve.JSON("GET", taskStatusPath("UPID:103"), `{"status":"stopped","exitstatus":"timeout"}`)
	te.pve.Handle("POST", "/nodes/pve-1/qemu/103/status/stop", func(pvetest.Request) (any, error) {
		return nil, &pvetest.Error{Status: 500, Message: "no such VM"}
	})
	err = stepGuestStop().Do(context.Background(), te.Env)
	if want := "force stop guest 103: POST /nodes/pve-1/qemu/103/status/stop: HTTP 500: no such VM"; err == nil || err.Error() != want {
		t.Errorf("error %v, want %s", err, want)
	}
}

// The guest list is read through any node, the target included; --dry-run
// sends no shutdown but still reports and records what it would stop.
func TestGuestStopDryRun(t *testing.T) {
	te := guestEnv(t)
	te.dryRun()
	if err := stepGuestStop().Do(context.Background(), te.Env); err != nil {
		t.Fatal(err)
	}
	te.wantWrites(t)
	te.wantLines(t,
		"  ! stopping 103 ap: the Wi-Fi drops",
		"  (dry-run) would POST /nodes/pve-1/qemu/103/status/shutdown timeout=180",
		"  guest 103 ap stopped",
		"  (dry-run) would POST /nodes/pve-1/lxc/105/status/shutdown timeout=180",
		"  guest 105 dns stopped",
		"  ! guest 107 is not on pve-1; skipping",
	)
}

// The undo starts what the step stopped. It reads the ids back from
// state.json, takes each guest's kind from the config, and skips a guest that
// is already running so a half-finished undo can be retried.
func TestGuestStopUndoStartsWhatWasStopped(t *testing.T) {
	te := guestEnv(t)
	te.State.SetData(guestStopStep, "stopped", []int{103, 105})
	te.reload(t)
	te.pve.JSON("GET", "/nodes/pve-1/qemu/103/status/current", `{"status":"running"}`)
	te.pve.JSON("GET", "/nodes/pve-1/lxc/105/status/current", `{"status":"stopped"}`)
	te.pve.Static("POST", "/nodes/pve-1/lxc/105/status/start", "UPID:105-start")
	te.pve.JSON("GET", taskStatusPath("UPID:105-start"), `{"status":"stopped","exitstatus":"OK"}`)

	if err := stepGuestStop().Undo(context.Background(), te.Env); err != nil {
		t.Fatal(err)
	}
	te.wantWrites(t, "POST /nodes/pve-1/lxc/105/status/start")
	te.wantLines(t, "  guest 103 is already running", "  guest 105 started")
	// Starting goes through the other nodes: the target may be the one down.
	for _, r := range te.pve.Requests() {
		if r.Endpoint == ip1 {
			t.Errorf("%s was sent to the target itself", r)
		}
	}
}

// One guest failing to start does not stop the next, and every failure is
// reported.
func TestStartGuestsKeepsGoingAndReportsEveryFailure(t *testing.T) {
	te := guestEnv(t)
	te.pve.Handle("GET", "/nodes/pve-1/qemu/103/status/current", func(pvetest.Request) (any, error) {
		return nil, &pvetest.Error{Status: 500, Message: "unreadable"}
	})
	te.pve.Handle("POST", "/nodes/pve-1/qemu/103/status/start", func(pvetest.Request) (any, error) {
		return nil, &pvetest.Error{Status: 500, Message: "no such device"}
	})
	te.pve.JSON("GET", "/nodes/pve-1/lxc/105/status/current", `{"status":"stopped"}`)
	te.pve.Static("POST", "/nodes/pve-1/lxc/105/status/start", "UPID:105-start")
	te.pve.JSON("GET", taskStatusPath("UPID:105-start"), `{"status":"stopped","exitstatus":"startup failed"}`)
	// 999 is in no stop_guests list: it is started as a VM.
	te.pve.JSON("GET", "/nodes/pve-1/qemu/999/status/current", `{"status":"stopped"}`)
	te.pve.Static("POST", "/nodes/pve-1/qemu/999/status/start", "UPID:999-start")
	te.pve.JSON("GET", taskStatusPath("UPID:999-start"), `{"status":"stopped","exitstatus":"OK"}`)

	err := startGuests(context.Background(), te.Env, []int{103, 105, 999})
	want := `start 103: POST /nodes/pve-1/qemu/103/status/start: HTTP 500: no such device; ` +
		`start 105: task UPID:105-start finished with "startup failed"`
	if err == nil || err.Error() != want {
		t.Errorf("error:\n got %v\nwant %s", err, want)
	}
	te.wantLines(t, "  guest 999 started")
}
