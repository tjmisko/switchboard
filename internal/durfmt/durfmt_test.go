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
