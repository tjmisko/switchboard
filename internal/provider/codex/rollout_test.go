package codex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/provider"
)

type memoryUsageRecorder struct {
	mu     sync.Mutex
	events []UsageUpdate
	seen   map[string]int64
	fail   bool
}

func (r *memoryUsageRecorder) PersistUsage(_ context.Context, update UsageUpdate) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		return errors.New("synthetic persistence failure containing /private/path and prompt text")
	}
	if r.seen == nil {
		r.seen = make(map[string]int64)
	}
	if revision, exists := r.seen[update.UpdateID]; exists && revision >= update.Revision {
		return nil
	}
	r.seen[update.UpdateID] = update.Revision
	r.events = append(r.events, cloneUsageUpdate(update))
	return nil
}

func (r *memoryUsageRecorder) snapshot() []UsageUpdate {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]UsageUpdate(nil), r.events...)
}

func TestRolloutFirstAttachUsesCumulativeAndRestartReplayIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "synthetic-rollout.jsonl")
	writeSyntheticRollout(t, path,
		sessionMeta("root", "", "openai"),
		turnContext("turn-1", "gpt-5.6-sol", "high", "fast"),
		tokenCount("turn-1", usageTuple(10, 2, 12), usageTuple(100, 20, 120), 272000),
	)
	recorder := &memoryUsageRecorder{}
	key := provider.RootKey{PID: 10, StartedAt: time.Unix(1, 0)}
	collector := testRolloutCollector(t, filepath.Join(dir, "state"), recorder)
	bindAndCollect(t, collector, key, "root", path)
	events := recorder.snapshot()
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].Delta.InputTokens != 100 || events[0].Delta.OutputTokens != 20 || events[0].Delta.TotalTokens != 120 {
		t.Fatalf("first-attach delta = %#v", events[0].Delta)
	}
	if events[0].Identity.Model != "gpt-5.6-sol" || events[0].Identity.ReasoningEffort != "high" ||
		events[0].Identity.ServiceTier != "fast" || events[0].RootSessionID != "root" || events[0].ThreadID != "root" {
		t.Fatalf("first-attach identity = %#v / %#v", events[0].Identity, events[0])
	}

	// A new collector simulates daemon restart. It reloads the durable byte and
	// cumulative cursor, so a full-file replay produces no second charge.
	restarted := testRolloutCollector(t, filepath.Join(dir, "state"), recorder)
	bindAndCollect(t, restarted, key, "root", path)
	if got := len(recorder.snapshot()); got != 1 {
		t.Fatalf("restart replay recorded %d events", got)
	}
}

func TestRolloutLateIdentityContextRevisionRerouteAndRegressionEpoch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout.jsonl")
	writeSyntheticRollout(t, path,
		sessionMeta("root", "", "openai"),
		tokenCount("late-turn", nil, usageTuple(100, 20, 120), 1000),
		turnContext("late-turn", "gpt-before", "medium", "standard"),
		// Same counters with a new context window are metadata-only.
		tokenCount("late-turn", nil, usageTuple(100, 20, 120), 2000),
		modelReroute("late-turn", "gpt-before", "gpt-after"),
		// A partial regression starts a new counter epoch and captures from zero.
		tokenCount("late-turn", nil, usageTuple(90, 25, 115), 2000),
	)
	recorder := &memoryUsageRecorder{}
	collector := testRolloutCollector(t, filepath.Join(dir, "state"), recorder)
	bindAndCollect(t, collector, provider.RootKey{PID: 11, StartedAt: time.Unix(2, 0)}, "root", path)
	events := recorder.snapshot()
	if len(events) != 5 {
		t.Fatalf("events = %d (%#v), want initial + identity/context/reroute revisions + epoch", len(events), events)
	}
	if events[0].UpdateID != events[1].UpdateID || events[1].Revision != 2 || events[1].Identity.Model != "gpt-before" {
		t.Fatalf("late identity revision = %#v / %#v", events[0], events[1])
	}
	if events[2].UpdateID != events[0].UpdateID || events[2].Revision != 3 || events[2].Total.ModelContextWindow != 2000 {
		t.Fatalf("context revision = %#v", events[2])
	}
	if events[3].UpdateID != events[0].UpdateID || events[3].Revision != 4 || events[3].Identity.Model != "gpt-after" ||
		events[3].ReroutedFromModel != "gpt-before" {
		t.Fatalf("reroute revision = %#v", events[3])
	}
	if events[4].UpdateID == events[0].UpdateID || events[4].Reconciliation != "counter_epoch" || events[4].Coverage != "partial" ||
		events[4].Delta.InputTokens != 90 || events[4].Delta.OutputTokens != 25 {
		t.Fatalf("regression epoch = %#v", events[4])
	}
}

func TestRolloutLastTotalDiscontinuityPricesOnlyAttributedLastAndMarksPartial(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout.jsonl")
	writeSyntheticRollout(t, path,
		sessionMeta("root", "", "openai"),
		turnContext("turn", "gpt", "high", ""),
		tokenCount("turn", nil, usageTuple(100, 20, 120), 1000),
		tokenCount("turn", usageTuple(50, 10, 60), usageTuple(300, 60, 360), 1000),
	)
	recorder := &memoryUsageRecorder{}
	collector := testRolloutCollector(t, filepath.Join(dir, "state"), recorder)
	bindAndCollect(t, collector, provider.RootKey{PID: 18, StartedAt: time.Unix(9, 0)}, "root", path)
	events := recorder.snapshot()
	if len(events) != 2 {
		t.Fatalf("events = %#v", events)
	}
	if events[1].Coverage != "partial" || events[1].Reconciliation != "last_total_gap" ||
		events[1].Delta.InputTokens != 50 || events[1].Delta.OutputTokens != 10 || events[1].Total.InputTokens != 300 {
		t.Fatalf("discontinuity event = %#v", events[1])
	}
}

func TestRolloutMalformedCommittedLinePersistsCoverageGap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout.jsonl")
	meta, _ := json.Marshal(sessionMeta("root", "", "openai"))
	tokens, _ := json.Marshal(tokenCount("turn", nil, usageTuple(5, 1, 6), 100))
	body := append(append(append(meta, '\n'), []byte("{malformed-content-that-must-not-escape}\n")...), tokens...)
	body = append(body, '\n')
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	recorder := &memoryUsageRecorder{}
	collector := testRolloutCollector(t, filepath.Join(dir, "state"), recorder)
	bindAndCollect(t, collector, provider.RootKey{PID: 19, StartedAt: time.Unix(10, 0)}, "root", path)
	events := recorder.snapshot()
	if len(events) != 2 || events[0].Coverage != "partial" || events[0].Reconciliation != "decode_gap" || !events[0].Delta.IsZero() {
		t.Fatalf("coverage records = %#v", events)
	}
}

func TestRolloutDurablePathDoesNotLoseMoreThanNotificationBuffer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout.jsonl")
	lines := []any{sessionMeta("root", "", "openai"), turnContext("turn", "gpt", "medium", "")}
	for i := int64(1); i <= 600; i++ {
		lines = append(lines, tokenCount("turn", nil, usageTuple(i, i, i*2), 1000))
	}
	writeSyntheticRollout(t, path, lines...)
	recorder := &memoryUsageRecorder{}
	collector := testRolloutCollector(t, filepath.Join(dir, "state"), recorder)
	bindAndCollect(t, collector, provider.RootKey{PID: 12, StartedAt: time.Unix(3, 0)}, "root", path)
	if got := len(recorder.snapshot()); got != 600 {
		t.Fatalf("durable collector persisted %d/600 events", got)
	}
}

func TestRolloutSamePrefixReplacementDoesNotReplayUsage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout.jsonl")
	writeSyntheticRollout(t, path,
		sessionMeta("root", "", "openai"),
		tokenCount("turn", nil, usageTuple(100, 20, 120), 1000),
		tokenCount("turn", nil, usageTuple(200, 40, 240), 1000),
	)
	recorder := &memoryUsageRecorder{}
	collector := testRolloutCollector(t, filepath.Join(dir, "state"), recorder)
	key := provider.RootKey{PID: 16, StartedAt: time.Unix(7, 0)}
	bindAndCollect(t, collector, key, "root", path)
	before := recorder.snapshot()
	if len(before) != 2 {
		t.Fatalf("initial events = %d", len(before))
	}

	replacement := filepath.Join(dir, "replacement.jsonl")
	writeSyntheticRollout(t, replacement,
		sessionMeta("root", "", "openai"),
		tokenCount("turn", nil, usageTuple(100, 20, 120), 1000),
		tokenCount("turn", nil, usageTuple(200, 40, 240), 1000),
		tokenCount("turn", nil, usageTuple(300, 60, 360), 1000),
	)
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	collector.collectAll(context.Background())
	after := recorder.snapshot()
	if len(after) != 4 {
		t.Fatalf("replacement replay produced %d events, want usage + one ambiguity marker + new usage", len(after))
	}
	if after[2].Coverage != "partial" || after[2].Reconciliation != "replacement_replay_ambiguity" || !after[2].Delta.IsZero() {
		t.Fatalf("replacement ambiguity marker = %#v", after[2])
	}
	if after[3].Delta.InputTokens != 100 || after[3].Delta.TotalTokens != 120 {
		t.Fatalf("post-replacement delta = %#v", after[3].Delta)
	}
	if after[0].UpdateID == after[3].UpdateID || after[1].UpdateID == after[3].UpdateID {
		t.Fatal("new cumulative point did not receive a distinct logical ID")
	}
}

func TestRolloutReplacementResetThatNeverReachesFloorPersistsOneGap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout.jsonl")
	writeSyntheticRollout(t, path,
		sessionMeta("root", "", "openai"),
		tokenCount("turn", nil, usageTuple(200, 40, 240), 1000),
	)
	recorder := &memoryUsageRecorder{}
	collector := testRolloutCollector(t, filepath.Join(dir, "state"), recorder)
	key := provider.RootKey{PID: 20, StartedAt: time.Unix(11, 0)}
	bindAndCollect(t, collector, key, "root", path)

	replacement := filepath.Join(dir, "replacement.jsonl")
	writeSyntheticRollout(t, replacement,
		sessionMeta("root", "", "openai"),
		tokenCount("turn", nil, usageTuple(25, 5, 30), 1000),
	)
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	collector.collectAll(context.Background())
	collector.collectAll(context.Background())
	events := recorder.snapshot()
	if len(events) != 2 || events[1].Coverage != "partial" ||
		events[1].Reconciliation != "replacement_replay_ambiguity" || !events[1].Delta.IsZero() {
		t.Fatalf("replacement reset coverage = %#v", events)
	}
	if events[1].Total.InputTokens != 200 || events[1].Total.TotalTokens != 240 {
		t.Fatalf("replacement reset lowered cumulative baseline: %#v", events[1].Total)
	}
}

func TestRolloutBindsRootAndChildFilesFromExactHooks(t *testing.T) {
	dir := t.TempDir()
	rootPath := filepath.Join(dir, "root.jsonl")
	childPath := filepath.Join(dir, "child.jsonl")
	writeSyntheticRollout(t, rootPath, sessionMeta("root", "", "openai"), tokenCount("root-turn", nil, usageTuple(2, 1, 3), 100))
	writeSyntheticRollout(t, childPath, sessionMeta("child", "root", "openai"), tokenCount("child-turn", nil, usageTuple(4, 2, 6), 100))
	recorder := &memoryUsageRecorder{}
	collector := testRolloutCollector(t, filepath.Join(dir, "state"), recorder)
	key := provider.RootKey{PID: 17, StartedAt: time.Unix(8, 0)}
	if err := collector.bind(key, "root", rootPath); err != nil {
		t.Fatal(err)
	}
	if err := collector.bind(key, "root", childPath); err != nil {
		t.Fatal(err)
	}
	collector.collectAll(context.Background())
	events := recorder.snapshot()
	if len(events) != 2 {
		t.Fatalf("multi-file events = %#v", events)
	}
	seen := map[string]bool{}
	for _, event := range events {
		seen[event.ThreadID] = true
	}
	if !seen["root"] || !seen["child"] {
		t.Fatalf("root/child exact files not both tailed: %#v", events)
	}
}

func TestRolloutExactBindingChildAssociationAndPrivacySafeDiagnostics(t *testing.T) {
	dir := t.TempDir()
	secret := "private-project-prompt-fragment"
	path := filepath.Join(dir, secret+".jsonl")
	writeSyntheticRollout(t, path,
		sessionMeta("child", "root", "openai"),
		map[string]any{"type": "response_item", "payload": map[string]any{"content": secret}},
		tokenCount("child-turn", nil, usageTuple(3, 2, 5), 100),
	)
	recorder := &memoryUsageRecorder{}
	var diagnostics []string
	collector := newRolloutCollector(filepath.Join(dir, "state"), recorder, func(category string) {
		diagnostics = append(diagnostics, category)
	}, time.Now)
	if err := collector.bind(provider.RootKey{PID: 13, StartedAt: time.Unix(4, 0)}, "root", path); err != nil {
		t.Fatal(err)
	}
	collector.collectAll(context.Background())
	events := recorder.snapshot()
	if len(events) != 1 || events[0].RootSessionID != "root" || events[0].ThreadID != "child" || events[0].ParentThreadID != "root" {
		t.Fatalf("child association = %#v", events)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		body, err := os.ReadFile(filepath.Join(dir, "state", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), secret) || strings.Contains(entry.Name(), secret) {
			t.Fatal("cursor persisted rollout path or ignored content")
		}
	}
	for _, category := range diagnostics {
		if strings.Contains(category, secret) || strings.Contains(category, string(filepath.Separator)) {
			t.Fatalf("unsafe diagnostic %q", category)
		}
	}

	wrongPath := filepath.Join(dir, "wrong.jsonl")
	writeSyntheticRollout(t, wrongPath, sessionMeta("another-root", "", "openai"), tokenCount("turn", nil, usageTuple(9, 1, 10), 100))
	wrong := testRolloutCollector(t, filepath.Join(dir, "wrong-state"), recorder)
	bindAndCollect(t, wrong, provider.RootKey{PID: 14, StartedAt: time.Unix(5, 0)}, "root", wrongPath)
	if got := len(recorder.snapshot()); got != 1 {
		t.Fatalf("mismatched exact binding persisted usage: %d", got)
	}
}

func TestRolloutPersistenceFailureDoesNotAdvanceCursor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout.jsonl")
	writeSyntheticRollout(t, path, sessionMeta("root", "", "openai"), tokenCount("turn", nil, usageTuple(7, 3, 10), 100))
	recorder := &memoryUsageRecorder{fail: true}
	var diagnostics []string
	collector := newRolloutCollector(filepath.Join(dir, "state"), recorder, func(category string) {
		diagnostics = append(diagnostics, category)
	}, time.Now)
	key := provider.RootKey{PID: 15, StartedAt: time.Unix(6, 0)}
	if err := collector.bind(key, "root", path); err != nil {
		t.Fatal(err)
	}
	collector.collectAll(context.Background())
	recorder.mu.Lock()
	recorder.fail = false
	recorder.mu.Unlock()
	collector.collectAll(context.Background())
	if got := len(recorder.snapshot()); got != 1 {
		t.Fatalf("retry after persistence failure recorded %d events", got)
	}
	if len(diagnostics) == 0 || diagnostics[0] != DiagnosticRolloutPersist {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
	for _, category := range diagnostics {
		if strings.Contains(category, "private") || strings.Contains(category, "prompt") || strings.Contains(category, "/") {
			t.Fatalf("raw persistence error escaped: %q", category)
		}
	}
}

func testRolloutCollector(t *testing.T, stateDir string, recorder UsageRecorder) *rolloutCollector {
	t.Helper()
	return newRolloutCollector(stateDir, recorder, func(category string) {
		if category != DiagnosticRolloutIdentityMismatch {
			t.Logf("rollout diagnostic: %s", category)
		}
	}, func() time.Time { return time.Unix(100, 0).UTC() })
}

func bindAndCollect(t *testing.T, collector *rolloutCollector, key provider.RootKey, rootID, path string) {
	t.Helper()
	if err := collector.bind(key, rootID, path); err != nil {
		t.Fatal(err)
	}
	collector.collectAll(context.Background())
}

func writeSyntheticRollout(t *testing.T, path string, values ...any) {
	t.Helper()
	var body strings.Builder
	for _, value := range values {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		body.Write(encoded)
		body.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(body.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func sessionMeta(threadID, rootID, providerName string) any {
	payload := map[string]any{"id": threadID, "model_provider": providerName}
	if rootID != "" {
		payload["root_session_id"] = rootID
	}
	return map[string]any{"type": "session_meta", "payload": payload}
}

func turnContext(turnID, model, effort, tier string) any {
	return map[string]any{"type": "turn_context", "payload": map[string]any{
		"turn_id": turnID, "model": model, "effort": effort, "service_tier": tier,
	}}
}

func modelReroute(turnID, from, to string) any {
	return map[string]any{"type": "event_msg", "payload": map[string]any{
		"type": "model_rerouted", "turn_id": turnID, "from_model": from, "to_model": to,
	}}
}

func tokenCount(turnID string, last, total any, contextWindow int64) any {
	info := map[string]any{"model_context_window": contextWindow}
	if last != nil {
		info["last_token_usage"] = last
	}
	if total != nil {
		info["total_token_usage"] = total
	}
	return map[string]any{"type": "event_msg", "payload": map[string]any{
		"type": "token_count", "turn_id": turnID, "info": info,
	}}
}

func usageTuple(input, output, total int64) any {
	return map[string]any{
		"input_tokens": input, "cached_input_tokens": input / 2,
		"output_tokens": output, "reasoning_output_tokens": output / 2, "total_tokens": total,
	}
}
