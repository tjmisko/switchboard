package durfmt

import (
	"testing"
	"time"
)

func TestCompact(t *testing.T) {
	cases := []struct {
		name string
		d    time.Duration
		want string
	}{
		{"negative clamps to zero", -5 * time.Second, "0s"},
		{"zero", 0, "0s"},
		{"sub-minute seconds", 45 * time.Second, "45s"},
		{"just under a minute", 59 * time.Second, "59s"},
		{"whole minutes drop seconds", 3*time.Minute + 12*time.Second, "3m"},
		{"just under an hour", 59*time.Minute + 59*time.Second, "59m"},
		{"hours and minutes zero-padded", 1*time.Hour + 4*time.Minute, "1h04m"},
		{"hours and minutes", 2*time.Hour + 37*time.Minute, "2h37m"},
		{"just under a day", 23*time.Hour + 59*time.Minute, "23h59m"},
		{"days and hours", 50 * time.Hour, "2d2h"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Compact(c.d); got != c.want {
				t.Errorf("Compact(%v) = %q, want %q", c.d, got, c.want)
			}
		})
	}
}

func TestSince(t *testing.T) {
	now := time.Date(2026, 6, 26, 14, 30, 0, 0, time.UTC)

	t.Run("nil yields empty", func(t *testing.T) {
		if got := Since(nil, now); got != "" {
			t.Errorf("Since(nil) = %q, want empty", got)
		}
	})
	t.Run("zero yields empty", func(t *testing.T) {
		var zero time.Time
		if got := Since(&zero, now); got != "" {
			t.Errorf("Since(zero) = %q, want empty", got)
		}
	})
	t.Run("computes age via Compact", func(t *testing.T) {
		since := now.Add(-45 * time.Second)
		if got := Since(&since, now); got != "45s" {
			t.Errorf("Since(now-45s) = %q, want 45s", got)
		}
	})
}

func TestCoarse(t *testing.T) {
	for _, tc := range []struct {
		name string
		d    time.Duration
		want string
	}{
		{"should floor zero below one minute", 0, "<1m"},
		{"should floor one second below one minute", time.Second, "<1m"},
		{"should floor 59s below one minute", 59 * time.Second, "<1m"},
		{"should floor a negative age from clock skew", -5 * time.Second, "<1m"},
		{"should show whole minutes at the boundary", time.Minute, "1m"},
		{"should show whole minutes under an hour", 59 * time.Minute, "59m"},
		{"should show hours and minutes past an hour", time.Hour + 4*time.Minute, "1h04m"},
		{"should show days and hours past a day", 50 * time.Hour, "2d2h"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Coarse(tc.d); got != tc.want {
				t.Errorf("Coarse(%v) = %q, want %q", tc.d, got, tc.want)
			}
		})
	}
}

// The reason Coarse exists: a hover redraws when its text changes, so nothing it
// shows may advance at second resolution. Walking a whole minute one second at a
// time must yield exactly one string.
func TestCoarseShouldNotChangeWithinAMinute(t *testing.T) {
	seen := map[string]bool{}
	for sec := range 60 {
		seen[Coarse(time.Duration(sec)*time.Second)] = true
	}
	if len(seen) != 1 {
		t.Errorf("Coarse produced %d distinct strings across one minute, want 1: %v", len(seen), seen)
	}
	// And the minute after it likewise holds one value, a different one.
	next := map[string]bool{}
	for sec := 60; sec < 120; sec++ {
		next[Coarse(time.Duration(sec)*time.Second)] = true
	}
	if len(next) != 1 {
		t.Errorf("Coarse produced %d distinct strings across the second minute, want 1: %v", len(next), next)
	}
}

func TestCoarseSince(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	t.Run("should yield empty for a nil instant", func(t *testing.T) {
		if got := CoarseSince(nil, now); got != "" {
			t.Errorf("CoarseSince(nil) = %q, want empty", got)
		}
	})
	t.Run("should yield empty for a zero instant", func(t *testing.T) {
		var zero time.Time
		if got := CoarseSince(&zero, now); got != "" {
			t.Errorf("CoarseSince(zero) = %q, want empty", got)
		}
	})
	t.Run("should floor a sub-minute age", func(t *testing.T) {
		since := now.Add(-45 * time.Second)
		if got := CoarseSince(&since, now); got != "<1m" {
			t.Errorf("CoarseSince(now-45s) = %q, want <1m", got)
		}
	})
	t.Run("should report minutes past the floor", func(t *testing.T) {
		since := now.Add(-8 * time.Minute)
		if got := CoarseSince(&since, now); got != "8m" {
			t.Errorf("CoarseSince(now-8m) = %q, want 8m", got)
		}
	})
}
