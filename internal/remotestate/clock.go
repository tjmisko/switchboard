package remotestate

import "time"

// Timer is the subset of *time.Timer the hold state machine uses.
type Timer interface {
	// Stop prevents a not-yet-fired timer from firing. Its return value is not
	// load-bearing here: every deadline decision is re-validated against the
	// host's epoch when the callback runs, so a Stop that loses the race is
	// harmless.
	Stop() bool
}

// Clock is the client-side time base for hold deadlines. It is injectable for
// the same reason CommandFactory and RetryWaiter are: the behavior under test
// is entirely about elapsed time, and a test that establishes it by sleeping
// establishes it flakily.
//
// Every deadline here is measured on the CLIENT's clock, deliberately. A remote
// snapshot's updated_at is a different machine's wall clock — it is not a
// causal revision and cannot be differenced against a local instant — so the
// only defensible statement the client can make is how long IT has been out of
// contact.
type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) Timer
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) AfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }
