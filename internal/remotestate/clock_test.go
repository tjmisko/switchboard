package remotestate

import (
	"sync"
	"time"
)

// fakeClock drives the hold state machine deterministically. Advance fires
// every due timer in deadline order, and it does so with its own lock
// RELEASED — a callback re-arms the host's next deadline through AfterFunc, so
// holding the lock across the call would deadlock the thing under test.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at      time.Time
	fn      func()
	done    bool
	stopped bool
}

func (t *fakeTimer) Stop() bool {
	if t.done || t.stopped {
		return false
	}
	t.stopped = true
	return true
}

func newFakeClock(now time.Time) *fakeClock { return &fakeClock{now: now} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &fakeTimer{at: c.now.Add(d), fn: f}
	c.timers = append(c.timers, timer)
	return timer
}

// Advance moves the clock and runs everything that came due, one timer at a
// time so a callback that arms a nearer deadline still gets to run within the
// same step.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
	for {
		next := c.takeDue()
		if next == nil {
			return
		}
		next.fn()
	}
}

func (c *fakeClock) takeDue() *fakeTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	live := c.timers[:0]
	var due *fakeTimer
	for _, timer := range c.timers {
		if timer.done || timer.stopped {
			continue
		}
		live = append(live, timer)
		if timer.at.After(c.now) {
			continue
		}
		if due == nil || timer.at.Before(due.at) {
			due = timer
		}
	}
	c.timers = live
	if due != nil {
		due.done = true
	}
	return due
}

// pending reports how many timers are still armed, which is how the shutdown
// test observes that Run disarmed the hold machinery.
func (c *fakeClock) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	count := 0
	for _, timer := range c.timers {
		if !timer.done && !timer.stopped {
			count++
		}
	}
	return count
}
