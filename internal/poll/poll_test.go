package poll

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func TestUntilAsksUntilDone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		calls, sleeps := 0, 0
		l := Within(5*time.Second, time.Minute)
		l.BeforeSleep = func() { sleeps++ }
		err := l.Until(context.Background(), func() (bool, error) { calls++; return calls == 3, nil })
		if err != nil || calls != 3 || sleeps != 2 || time.Since(start) != 10*time.Second {
			t.Errorf("err=%v calls=%d sleeps=%d elapsed=%s", err, calls, sleeps, time.Since(start))
		}
	})
}

// The deadline is checked after a probe: always one probe, and up to one
// interval of overshoot. BeforeSleep does not run on the way out.
func TestUntilTimesOutAfterAProbe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		calls, sleeps := 0, 0
		l := Within(5*time.Second, 12*time.Second)
		l.BeforeSleep = func() { sleeps++ }
		err := l.Until(context.Background(), func() (bool, error) { calls++; return false, nil })
		if err != ErrTimeout || calls != 4 || sleeps != 3 || time.Since(start) != 15*time.Second {
			t.Errorf("err=%v calls=%d sleeps=%d elapsed=%s", err, calls, sleeps, time.Since(start))
		}

		calls = 0
		err = Loop{Every: time.Second, Deadline: time.Now().Add(-time.Hour)}.Until(context.Background(),
			func() (bool, error) { calls++; return true, nil })
		if err != nil || calls != 1 {
			t.Errorf("a passed deadline must still probe once: err=%v calls=%d", err, calls)
		}
	})
}

func TestUntilStopsOnAProbeErrorOrCancel(t *testing.T) {
	boom := errors.New("cannot tell")
	err := Within(time.Second, time.Minute).Until(context.Background(), func() (bool, error) { return true, boom })
	if err != boom {
		t.Errorf("error %v, want the probe's", err)
	}
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
		defer cancel()
		start := time.Now()
		err := Within(5*time.Second, time.Minute).Until(ctx, func() (bool, error) { return false, nil })
		if err != context.DeadlineExceeded || time.Since(start) != 7*time.Second {
			t.Errorf("err=%v elapsed=%s", err, time.Since(start))
		}
	})
}
