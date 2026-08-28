package durfmt

import (
	"testing"
	"time"
)

func TestNextCompactChangeTracksVisibleResolution(t *testing.T) {
	since := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{"future instant remains zero until one second after it", since.Add(-5 * time.Second), since.Add(time.Second)},
		{"seconds", since.Add(45*time.Second + 500*time.Millisecond), since.Add(46 * time.Second)},
		{"minutes", since.Add(8*time.Minute + 30*time.Second), since.Add(9 * time.Minute)},
		{"hours still advance by minute", since.Add(2*time.Hour + 37*time.Minute + 2*time.Second), since.Add(2*time.Hour + 38*time.Minute)},
		{"days advance by hour", since.Add(50*time.Hour + 30*time.Minute), since.Add(51 * time.Hour)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := NextCompactChange(since, test.now); !got.Equal(test.want) {
				t.Fatalf("next = %v, want %v", got, test.want)
			}
		})
	}
}

func TestNextCoarseChangeNeverSchedulesSubMinutePolling(t *testing.T) {
	since := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{"future", since.Add(-5 * time.Second), since.Add(time.Minute)},
		{"sub-minute floor", since.Add(45 * time.Second), since.Add(time.Minute)},
		{"minutes", since.Add(8*time.Minute + 30*time.Second), since.Add(9 * time.Minute)},
		{"hours still advance by minute", since.Add(2*time.Hour + 37*time.Minute), since.Add(2*time.Hour + 38*time.Minute)},
		{"days advance by hour", since.Add(50*time.Hour + 30*time.Minute), since.Add(51 * time.Hour)},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := NextCoarseChange(since, test.now)
			if !got.Equal(test.want) {
				t.Fatalf("next = %v, want %v", got, test.want)
			}
			if test.now.Before(since.Add(time.Minute)) && got.Sub(test.now) < 15*time.Second {
				t.Fatalf("coarse refresh unexpectedly scheduled at sub-minute cadence: %v", got.Sub(test.now))
			}
		})
	}
}

func TestEarlierIgnoresZeroInstants(t *testing.T) {
	a := time.Unix(10, 0)
	b := time.Unix(20, 0)
	if got := Earlier(time.Time{}, b); got != b {
		t.Fatalf("Earlier(zero, b) = %v", got)
	}
	if got := Earlier(a, time.Time{}); got != a {
		t.Fatalf("Earlier(a, zero) = %v", got)
	}
	if got := Earlier(b, a); got != a {
		t.Fatalf("Earlier(b, a) = %v", got)
	}
}
