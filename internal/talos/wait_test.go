package talos

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	plog "github.com/TomayGo/pvectl/internal/log"
)

// probed returns a client whose liveness probe answers from the given list,
// one entry per call, the last one repeating.
func probed(answers ...bool) (*Client, *int) {
	calls := 0
	c := New("/nonexistent/talosconfig", plog.Discard(), false)
	c.probe = func(ctx context.Context, ip string) (bool, error) {
		a := answers[min(calls, len(answers)-1)]
		calls++
		return a, nil
	}
	return c, &calls
}

// WaitDown and WaitUp ask every 5 s, and check the deadline after asking: a
// node is always probed at least once, and the wait can overshoot the timeout
// by up to one interval.
func TestWaitDown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, calls := probed(true, true, false)
		start := time.Now()
		if err := c.WaitDown(context.Background(), "192.0.2.21", time.Minute); err != nil {
			t.Fatal(err)
		}
		if *calls != 3 || time.Since(start) != 10*time.Second {
			t.Errorf("%d probe(s) over %s, want 3 over 10s", *calls, time.Since(start))
		}
	})
	synctest.Test(t, func(t *testing.T) {
		c, calls := probed(true)
		start := time.Now()
		err := c.WaitDown(context.Background(), "192.0.2.21", 12*time.Second)
		if err == nil || err.Error() != "talos node 192.0.2.21 still responding after 12s" {
			t.Errorf("error %v", err)
		}
		if *calls != 4 || time.Since(start) != 15*time.Second {
			t.Errorf("%d probe(s) over %s, want 4 over 15s", *calls, time.Since(start))
		}
	})
}

func TestWaitUp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, calls := probed(false, true)
		start := time.Now()
		if err := c.WaitUp(context.Background(), "192.0.2.21", time.Minute); err != nil {
			t.Fatal(err)
		}
		if *calls != 2 || time.Since(start) != 5*time.Second {
			t.Errorf("%d probe(s) over %s, want 2 over 5s", *calls, time.Since(start))
		}
	})
	synctest.Test(t, func(t *testing.T) {
		c, _ := probed(false)
		err := c.WaitUp(context.Background(), "192.0.2.21", 0)
		if err == nil || err.Error() != "talos node 192.0.2.21 not answering after 0s" {
			t.Errorf("error %v", err)
		}
	})
}

// "Cannot tell" is not "down": a probe that fails for a reason other than the
// node being off ends the wait with that reason.
func TestWaitsStopWhenTheProbeCannotTell(t *testing.T) {
	blind := errors.New("talosconfig unreadable")
	c := New("/nonexistent/talosconfig", plog.Discard(), false)
	c.probe = func(ctx context.Context, ip string) (bool, error) { return false, blind }
	err := c.WaitDown(context.Background(), "192.0.2.21", time.Minute)
	if !errors.Is(err, blind) || err.Error() != "cannot tell whether 192.0.2.21 is down: talosconfig unreadable" {
		t.Errorf("WaitDown: %v", err)
	}
	err = c.WaitUp(context.Background(), "192.0.2.21", time.Minute)
	if !errors.Is(err, blind) || err.Error() != "cannot reach 192.0.2.21: talosconfig unreadable" {
		t.Errorf("WaitUp: %v", err)
	}
}

func TestWaitsHonourCancelAndDryRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, _ := probed(true)
		ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
		defer cancel()
		if err := c.WaitDown(ctx, "192.0.2.21", time.Minute); err != context.DeadlineExceeded {
			t.Errorf("WaitDown: %v", err)
		}
		c, _ = probed(false)
		ctx2, cancel2 := context.WithTimeout(context.Background(), 7*time.Second)
		defer cancel2()
		if err := c.WaitUp(ctx2, "192.0.2.21", time.Minute); err != context.DeadlineExceeded {
			t.Errorf("WaitUp: %v", err)
		}
	})
	// --dry-run shut nothing down, so there is nothing to wait for.
	c := New("/nonexistent/talosconfig", plog.Discard(), true)
	c.probe = func(ctx context.Context, ip string) (bool, error) {
		t.Error("dry-run must not probe")
		return false, nil
	}
	if err := c.WaitDown(context.Background(), "192.0.2.21", time.Minute); err != nil {
		t.Error(err)
	}
	if err := c.WaitUp(context.Background(), "192.0.2.21", time.Minute); err != nil {
		t.Error(err)
	}
}
