package main

import (
	"context"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	"github.com/TomayGo/pvectl/internal/pve/pvetest"
)

// These are the guest and OSD helpers of `shutdown cluster` / `start cluster`.
// The commands themselves are not run here; their helpers are, against a fake
// PVE, to record what each one asks for and prints.

func taskPath(node, upid string) string { return "/nodes/" + node + "/tasks/" + upid + "/status" }

func TestClusterGuestShutdown(t *testing.T) {
	w := newWorld(t)
	e := w.handEnv("")
	w.pve.JSON("GET", "/nodes/node-a/qemu/101/status/current", `{"status":"running"}`)
	w.pve.Static("POST", "/nodes/node-a/qemu/101/status/shutdown", "UPID:101")
	w.pve.JSON("GET", taskPath("node-a", "UPID:101"), `{"status":"stopped","exitstatus":"OK"}`)

	if err := shutdownGuest(context.Background(), e, "node-a", "qemu", 101); err != nil {
		t.Fatal(err)
	}
	if got, want := w.pve.Writes(), []string{"POST /nodes/node-a/qemu/101/status/shutdown timeout=180"}; !reflect.DeepEqual(got, want) {
		t.Errorf("writes %q, want %q", got, want)
	}
	wantLines(t, w.lines(), "  guest 101 on node-a stopped")
}

// A guest that is not running, or cannot even be read, is passed over: with
// the whole cluster going down there is no one left to ask later.
func TestClusterGuestShutdownPassesOverWhatIsNotRunning(t *testing.T) {
	w := newWorld(t)
	e := w.handEnv("")
	w.pve.JSON("GET", "/nodes/node-a/lxc/105/status/current", `{"status":"stopped"}`)
	w.pve.Handle("GET", "/nodes/node-b/qemu/102/status/current", func(pvetest.Request) (any, error) {
		return nil, &pvetest.Error{Status: 500, Message: "Configuration file 'nodes/node-b/qemu-server/102.conf' does not exist"}
	})
	if err := shutdownGuest(context.Background(), e, "node-a", "lxc", 105); err != nil {
		t.Fatal(err)
	}
	upid, running, err := requestGuestShutdown(context.Background(), e, "node-b", "qemu", 102)
	if err != nil || running || upid != "" {
		t.Errorf("upid=%q running=%v err=%v", upid, running, err)
	}
	if n := len(w.pve.Writes()); n != 0 {
		t.Errorf("%d write(s)", n)
	}
	wantLines(t, w.lines(),
		"  guest 105 on node-a is already stopped",
		"  ! cannot read guest 102 on node-b: GET /nodes/node-b/qemu/102/status/current: HTTP 500: "+
			"Configuration file 'nodes/node-b/qemu-server/102.conf' does not exist",
	)
}

func TestClusterGuestShutdownFailures(t *testing.T) {
	w := newWorld(t)
	e := w.handEnv("")
	w.pve.JSON("GET", "/nodes/node-a/qemu/101/status/current", `{"status":"running"}`)
	w.pve.Handle("POST", "/nodes/node-a/qemu/101/status/shutdown", func(pvetest.Request) (any, error) {
		return nil, &pvetest.Error{Status: 500, Message: "VM is locked (backup)"}
	})
	err := shutdownGuest(context.Background(), e, "node-a", "qemu", 101)
	want := "shutdown guest 101 on node-a: POST /nodes/node-a/qemu/101/status/shutdown: HTTP 500: VM is locked (backup)"
	if err == nil || err.Error() != want {
		t.Errorf("error %v", err)
	}

	// Not shutting down in time is an error unless --force-stop-after allows
	// pulling the plug.
	w = newWorld(t)
	e = w.handEnv("")
	w.pve.JSON("GET", taskPath("node-a", "UPID:101"), `{"status":"stopped","exitstatus":"got timeout"}`)
	err = awaitGuestShutdown(context.Background(), e, "node-a", "qemu", 101, "UPID:101")
	want = `guest 101 on node-a did not shut down within 3m0s: task UPID:101 finished with "got timeout" (use --force-stop-after)`
	if err == nil || err.Error() != want {
		t.Errorf("error %v", err)
	}
}

func TestClusterGuestShutdownForcesAfterTheDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		e := w.handEnv("")
		e.Opts.ForceStopAfter = time.Minute
		w.pve.JSON("GET", "/nodes/node-a/qemu/101/status/current", `{"status":"running"}`)
		w.pve.Static("POST", "/nodes/node-a/qemu/101/status/shutdown", "UPID:101")
		w.pve.JSON("GET", taskPath("node-a", "UPID:101"), `{"status":"running"}`)
		w.pve.Static("POST", "/nodes/node-a/qemu/101/status/stop", "UPID:101-stop")
		w.pve.JSON("GET", taskPath("node-a", "UPID:101-stop"), `{"status":"stopped","exitstatus":"OK"}`)

		start := time.Now()
		if err := shutdownGuest(context.Background(), e, "node-a", "qemu", 101); err != nil {
			t.Fatal(err)
		}
		if got := time.Since(start); got != 92*time.Second {
			t.Errorf("took %s, want 1m32s (60s graceful + 30s, noticed on the next 2s poll)", got)
		}
		want := []string{"POST /nodes/node-a/qemu/101/status/shutdown timeout=60", "POST /nodes/node-a/qemu/101/status/stop"}
		if got := w.pve.Writes(); !reflect.DeepEqual(got, want) {
			t.Errorf("writes %q, want %q", got, want)
		}
		// As the code stands, the forced path does not print "stopped".
		wantLines(t, w.lines(), "  ! guest 101 did not stop cleanly (task UPID:101 still running after 1m30s); forcing")
	})
}

// waitOSDs holds `start cluster` back from clearing pause until every OSD is
// up and in, asking every 10 s. A status it cannot read counts as not yet.
func TestWaitOSDs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		e := w.handEnv("")
		n := 0
		w.pve.Handle("GET", "/cluster/ceph/status", func(pvetest.Request) (any, error) {
			switch n++; n {
			case 1:
				return nil, &pvetest.Error{Status: 500, Message: "mon not ready"}
			case 2:
				return map[string]any{"osdmap": map[string]int{"num_osds": 3, "num_up_osds": 3, "num_in_osds": 2}}, nil
			}
			return map[string]any{"osdmap": map[string]int{"num_osds": 3, "num_up_osds": 3, "num_in_osds": 3}}, nil
		})
		start := time.Now()
		if err := waitOSDs(context.Background(), e); err != nil {
			t.Fatal(err)
		}
		if got := time.Since(start); got != 20*time.Second {
			t.Errorf("waited %s, want 20s", got)
		}
		wantLines(t, w.lines(), "  3/3 OSDs up and in")
	})
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		e := w.handEnv("")
		e.Opts.TimeoutUp = 25 * time.Second
		w.pve.JSON("GET", "/cluster/ceph/status", `{"osdmap":{"num_osds":3,"num_up_osds":2,"num_in_osds":3}}`)
		start := time.Now()
		err := waitOSDs(context.Background(), e)
		if err == nil || err.Error() != "OSDs did not all come back within 25s" {
			t.Errorf("error %v", err)
		}
		if got := time.Since(start); got != 30*time.Second {
			t.Errorf("gave up after %s, want 30s", got)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := waitOSDs(ctx, e); err != context.DeadlineExceeded {
			t.Errorf("error %v, want the context's", err)
		}
	})
}
