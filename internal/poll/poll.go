// Package poll is the one wait loop pvectl has: ask, and if the answer is
// "not yet", sleep and ask again until a deadline.
//
// Every wait in pvectl has this shape -- a worker task, an HA evacuation, a
// host going down, OSDs coming back -- and they all share two properties this
// package keeps: the thing is always asked about at least once, even with the
// deadline already passed, and the deadline is checked after asking, so a wait
// can overshoot its timeout by up to one interval.
package poll

import (
	"context"
	"errors"
	"time"
)

// ErrTimeout is returned when the deadline passed with the probe still saying
// "not yet". Callers replace it with a message that says what they were
// waiting for.
var ErrTimeout = errors.New("timed out")

// Loop describes one wait.
type Loop struct {
	// Every is how long to sleep between probes.
	Every time.Duration
	// Deadline is checked after each probe that was not done.
	Deadline time.Time
	// BeforeSleep, if set, runs before each sleep: after a probe that was not
	// done, with the deadline not yet passed. It is where a wait reports its
	// progress.
	BeforeSleep func()
}

// Until calls probe until it reports done or fails. It returns nil when done,
// the probe's error when it fails, ErrTimeout when the deadline has passed
// after a probe that was not done, and the context's error when the context
// ends during a sleep.
func (l Loop) Until(ctx context.Context, probe func() (done bool, err error)) error {
	for {
		done, err := probe()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		if time.Now().After(l.Deadline) {
			return ErrTimeout
		}
		if l.BeforeSleep != nil {
			l.BeforeSleep()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(l.Every):
		}
	}
}

// Within is a Loop that gives up timeout from now.
func Within(every, timeout time.Duration) Loop {
	return Loop{Every: every, Deadline: time.Now().Add(timeout)}
}
