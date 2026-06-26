package history

import (
	"testing"
	"time"
)

func TestHeldMs(t *testing.T) {
	base := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)

	t.Run("zero since yields 0 (guards against a since-epoch duration)", func(t *testing.T) {
		if got := HeldMs(time.Time{}, base); got != 0 {
			t.Errorf("HeldMs(zero, now) = %d, want 0", got)
		}
	})
	t.Run("normal interval yields milliseconds", func(t *testing.T) {
		if got := HeldMs(base, base.Add(1500*time.Millisecond)); got != 1500 {
			t.Errorf("HeldMs = %d, want 1500", got)
		}
	})
	t.Run("now before since yields a negative duration", func(t *testing.T) {
		if got := HeldMs(base, base.Add(-2*time.Second)); got != -2000 {
			t.Errorf("HeldMs(now<since) = %d, want -2000", got)
		}
	})
}
