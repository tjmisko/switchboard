package durfmt

import "time"

// Timer is the resettable clock edge used by live renderers. Exposing the small
// interface here lets their tests drive time without sleeping.
type Timer interface {
	C() <-chan time.Time
	Reset(time.Duration)
	Stop()
}

// Clock supplies wall time and timers to a renderer loop.
type Clock interface {
	Now() time.Time
	NewTimer(time.Duration) Timer
}

// SystemClock is the production Clock.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now() }

func (SystemClock) NewTimer(d time.Duration) Timer {
	return &systemTimer{timer: time.NewTimer(positive(d))}
}

type systemTimer struct{ timer *time.Timer }

func (t *systemTimer) C() <-chan time.Time { return t.timer.C }

func (t *systemTimer) Reset(d time.Duration) {
	if !t.timer.Stop() {
		select {
		case <-t.timer.C:
		default:
		}
	}
	t.timer.Reset(positive(d))
}

func (t *systemTimer) Stop() { t.timer.Stop() }

func positive(d time.Duration) time.Duration {
	if d <= 0 {
		return time.Nanosecond
	}
	return d
}

// NextCompactChange returns the first instant after now at which Compact's
// output for an age measured from since can change. Compact advances once per
// second below a minute, once per minute below a day, then once per hour.
func NextCompactChange(since, now time.Time) time.Time {
	age := now.Sub(since)
	if age < 0 {
		return since.Add(time.Second)
	}
	if age < time.Minute {
		return since.Add((age/time.Second + 1) * time.Second)
	}
	if age < 24*time.Hour {
		return since.Add((age/time.Minute + 1) * time.Minute)
	}
	return since.Add((age/time.Hour + 1) * time.Hour)
}

// NextCoarseChange returns the first instant after now at which Coarse's output
// can change. The sub-minute floor lasts until one minute, then the display
// advances once per minute below a day and once per hour thereafter.
func NextCoarseChange(since, now time.Time) time.Time {
	age := now.Sub(since)
	if age < time.Minute {
		return since.Add(time.Minute)
	}
	if age < 24*time.Hour {
		return since.Add((age/time.Minute + 1) * time.Minute)
	}
	return since.Add((age/time.Hour + 1) * time.Hour)
}

// Earlier returns the earlier non-zero instant. It is a convenience for a
// renderer that shows several independent ages in one frame.
func Earlier(a, b time.Time) time.Time {
	if a.IsZero() || (!b.IsZero() && b.Before(a)) {
		return b
	}
	return a
}
