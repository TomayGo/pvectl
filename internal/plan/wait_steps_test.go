package plan

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/TomayGo/pvectl/internal/pve/pvetest"
)

// clusterStatus renders GET /cluster/status with pve-1 online or not.
func clusterStatus(pve1Online bool) string {
	on := "1"
	if !pve1Online {
		on = "0"
	}
	return `[{"type":"cluster","name":"lab","nodes":3,"quorate":1},` +
		`{"type":"node","name":"pve-1","online":` + on + `},` +
		`{"type":"node","name":"pve-2","online":1},{"type":"node","name":"pve-3","online":1}]`
}

func waitDown(te *testEnv) error {
	return stepWaitDown(hostPowerStep("reboot")).Do(context.Background(), te.Env)
}

func TestHostPowerRecordsUptimeAndAsksAnotherNode(t *testing.T) {
	te := newTestEnv(t, CmdReboot)
	te.pve.JSON("GET", "/nodes/pve-1/status", `{"uptime":86400}`)
	te.pve.Static("POST", "/nodes/pve-1/status", nil)
	if err := stepHostPower("reboot").Do(context.Background(), te.Env); err != nil {
		t.Fatal(err)
	}
	te.wantWrites(t, "POST /nodes/pve-1/status command=reboot")
	for _, r := range te.pve.Requests() {
		if r.Endpoint == ip1 {
			t.Errorf("%s was sent to the node being rebooted", r)
		}
	}
	if got := dataOf(te.Env, "host-reboot", "uptime_before"); got != int64(86400) {
		t.Errorf("uptime_before recorded as %#v", got)
	}
	te.wantLines(t, "  sending reboot to pve-1 via ["+ip2+" "+ip3+"]")

	// An unreadable uptime is not a reason to hold the reboot back.
	te = newTestEnv(t, CmdShutdown)
	te.pve.Handle("GET", "/nodes/pve-1/status", func(pvetest.Request) (any, error) {
		return nil, &pvetest.Error{Status: 500, Message: "boom"}
	})
	te.pve.Static("POST", "/nodes/pve-1/status", nil)
	if err := stepHostPower("shutdown").Do(context.Background(), te.Env); err != nil {
		t.Fatal(err)
	}
	te.wantWrites(t, "POST /nodes/pve-1/status command=shutdown")
	if got := dataOf(te.Env, "host-shutdown", "uptime_before"); got != nil {
		t.Errorf("uptime_before recorded as %#v", got)
	}
}

// Signal one: corosync says the node left.
func TestWaitDownSeesTheNodeLeaveTheCluster(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		te := newTestEnv(t, CmdReboot)
		te.pve.Sequence("GET", "/cluster/status", clusterStatus(true), clusterStatus(true), clusterStatus(false))
		start := time.Now()
		if err := waitDown(te); err != nil {
			t.Fatal(err)
		}
		if got := elapsed(start); got != 10*time.Second {
			t.Errorf("waited %s, want 10s (polls 5s apart)", got)
		}
		te.wantLines(t, "  pve-1 is offline")
	})
}

// Signal two: the node's own API stops answering, three probes in a row.
func TestWaitDownSeesTheNodeStopAnswering(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		te := newTestEnv(t, CmdReboot)
		te.pve.JSON("GET", "/cluster/status", clusterStatus(true))
		te.pve.SetDown(ip1, true)
		start := time.Now()
		if err := waitDown(te); err != nil {
			t.Fatal(err)
		}
		if got := elapsed(start); got != 10*time.Second {
			t.Errorf("waited %s, want 10s (third probe)", got)
		}
		te.wantLines(t, "  pve-1 stopped answering on "+ip1)
	})
}

// One answer in between starts the count again.
func TestWaitDownRestartsTheCountWhenTheNodeAnswers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		te := newTestEnv(t, CmdReboot)
		te.pve.JSON("GET", "/cluster/status", clusterStatus(true))
		probes := 0
		te.pve.Handle("GET", "/version", func(r pvetest.Request) (any, error) {
			probes++
			if probes == 3 {
				return map[string]string{"version": "9.2.4"}, nil
			}
			return nil, &pvetest.Error{Status: 595, Message: "no route to host"}
		})
		start := time.Now()
		if err := waitDown(te); err != nil {
			t.Fatal(err)
		}
		// down, down, up, down, down, down: the sixth probe, 25s in.
		if got := elapsed(start); got != 25*time.Second {
			t.Errorf("waited %s, want 25s", got)
		}
	})
}

// Signal three: the node answers, but its uptime is lower than the one
// recorded before the command -- it went down and came back between polls.
// The record is only usable once it has been through state.json.
func TestWaitDownSeesAQuickRebootByUptime(t *testing.T) {
	te := newTestEnv(t, CmdReboot)
	te.State.SetData("host-reboot", "uptime_before", int64(86400))
	te.reload(t)
	te.pve.JSON("GET", "/cluster/status", clusterStatus(true))
	te.pve.JSON("GET", "/nodes/pve-1/status", `{"uptime":42}`)
	if err := waitDown(te); err != nil {
		t.Fatal(err)
	}
	te.wantLines(t, "  pve-1 has already rebooted (uptime 42s, was 86400s)")
}

// As the code stands: within the run that recorded it, uptime_before is still
// an int64 in memory, the step reads it as float64, and so the uptime signal
// is off unless the state was reloaded (`resume`). The wait then ends on one
// of the other two signals or times out.
func TestWaitDownIgnoresUptimeRecordedInTheSameProcess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		te := newTestEnv(t, CmdReboot)
		te.Opts.TimeoutDown = 20 * time.Second
		te.State.SetData("host-reboot", "uptime_before", int64(86400))
		te.pve.JSON("GET", "/cluster/status", clusterStatus(true))
		start := time.Now()
		err := waitDown(te)
		if err == nil || err.Error() != "pve-1 still online after 20s" {
			t.Errorf("error %v", err)
		}
		if got := elapsed(start); got != 25*time.Second {
			t.Errorf("gave up after %s, want 25s", got)
		}
		for _, r := range te.pve.Requests() {
			if r.Path == "/nodes/pve-1/status" {
				t.Fatal("the uptime was read; the in-memory record is being used after all")
			}
		}
	})
}

// A cluster status that cannot be read is not evidence either way.
func TestWaitDownToleratesAnUnreadableClusterStatus(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		te := newTestEnv(t, CmdReboot)
		n := 0
		te.pve.Handle("GET", "/cluster/status", func(pvetest.Request) (any, error) {
			if n++; n < 3 {
				return nil, &pvetest.Error{Status: 500, Message: "no quorum"}
			}
			return rawJSON(clusterStatus(false)), nil
		})
		if err := waitDown(te); err != nil {
			t.Fatal(err)
		}
		te.wantLines(t, "  pve-1 is offline")
	})
}

func TestWaitDownDryRunAndCancel(t *testing.T) {
	te := newTestEnv(t, CmdReboot)
	te.dryRun()
	if err := waitDown(te); err != nil {
		t.Fatal(err)
	}
	if n := len(te.pve.Requests()); n != 0 {
		t.Errorf("dry-run made %d request(s)", n)
	}
	te.wantLines(t, "  (dry-run) would wait for pve-1 to go offline")

	synctest.Test(t, func(t *testing.T) {
		te := newTestEnv(t, CmdReboot)
		te.pve.JSON("GET", "/cluster/status", clusterStatus(true))
		ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
		defer cancel()
		err := stepWaitDown(hostPowerStep("reboot")).Do(ctx, te.Env)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("error %v, want the context's", err)
		}
	})
}

// waitFor is the loop behind wait-up and verify: probe, and if not ready,
// sleep 10 s and probe again until the deadline has passed.
func TestWaitFor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		te := newTestEnv(t, CmdReboot)
		start := time.Now()
		calls := 0
		err := waitFor(context.Background(), te.Env, start.Add(time.Minute), "ceph OSDs up+in", func() (bool, string) {
			calls++
			return calls == 3, "2/3 up"
		})
		if err != nil || calls != 3 || elapsed(start) != 20*time.Second {
			t.Errorf("err=%v calls=%d elapsed=%s; want nil, 3, 20s", err, calls, elapsed(start))
		}
		te.wantLines(t, "  ceph OSDs up+in: ready")
	})
	synctest.Test(t, func(t *testing.T) {
		te := newTestEnv(t, CmdReboot)
		start := time.Now()
		calls := 0
		err := waitFor(context.Background(), te.Env, start.Add(25*time.Second), "talos API", func() (bool, string) {
			calls++
			return false, "no answer from 192.0.2.21"
		})
		// Probes at 0, 10, 20 and 30 s; the fourth is the first past the
		// deadline.
		if err == nil || err.Error() != "timed out waiting for talos API (no answer from 192.0.2.21)" || calls != 4 || elapsed(start) != 30*time.Second {
			t.Errorf("err=%v calls=%d elapsed=%s", err, calls, elapsed(start))
		}
	})
	synctest.Test(t, func(t *testing.T) {
		te := newTestEnv(t, CmdReboot)
		// A deadline already passed still gets one probe.
		calls := 0
		err := waitFor(context.Background(), te.Env, time.Now().Add(-time.Second), "corosync quorum", func() (bool, string) {
			calls++
			return true, ""
		})
		if err != nil || calls != 1 {
			t.Errorf("err=%v calls=%d", err, calls)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err = waitFor(ctx, te.Env, time.Now().Add(time.Minute), "x", func() (bool, string) { return false, "not yet" })
		if err != context.Canceled {
			t.Errorf("error %v, want context.Canceled", err)
		}
	})
}
