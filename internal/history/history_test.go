package history

import (
	"encoding/json"
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

func TestLegacyUsageEventJSONRemainsReadable(t *testing.T) {
	const line = `{"ts":"2026-08-25T12:00:00Z","type":"usage_sample","agent":"claude","model":"claude-opus-4-8","tok_in":100,"tok_out":20,"tok_cache_read":30,"tok_cache_create":40}`
	var event Event
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		t.Fatal(err)
	}
	usage := event.CanonicalUsage()
	if event.SchemaVersion != 0 || usage.InputTokens != 100 || usage.OutputTokens != 20 ||
		usage.CachedInputTokens != 30 || usage.CacheWriteInputTokens != 40 {
		t.Fatalf("legacy event projection = event %+v usage %+v", event, usage)
	}
}

func TestCanonicalUsageKeepsClaudeCacheTTLsDistinct(t *testing.T) {
	event := Event{TokIn: 100, TokCacheCreate: 30, TokCacheCreate5m: 10, TokCacheCreate1h: 20}
	usage := event.CanonicalUsage()
	if usage.InputTokens != 100 || usage.CacheWriteInputTokens != 0 ||
		usage.CacheWrite5mInputTokens != 10 || usage.CacheWrite1hInputTokens != 20 {
		t.Fatalf("canonical usage = %+v", usage)
	}
}
