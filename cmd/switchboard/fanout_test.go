package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/fanout"
	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/state"
)

func readEvents(t *testing.T, dir string) []history.Event {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var evs []history.Event
	for _, e := range entries {
		f, _ := os.Open(filepath.Join(dir, e.Name()))
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var ev history.Event
			if json.Unmarshal(sc.Bytes(), &ev) == nil {
				evs = append(evs, ev)
			}
		}
		f.Close()
	}
	return evs
}

func eventsOfType(evs []history.Event, typ string) []history.Event {
	var out []history.Event
	for _, ev := range evs {
		if ev.Type == typ {
			out = append(out, ev)
		}
	}
	return out
}

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Subagent fanout detection moved to internal/fanout (the Observer), which has
// its own thorough tests; observe() now just delegates to it. The old tail-based
// TestObserveFanout* tests were removed with the tail-based code they exercised.

func assistantUsageModelLine(model string, in, out int64) string {
	return `{"type":"assistant","message":{"role":"assistant","model":"` + model +
		`","content":[],"usage":{"input_tokens":` + itoa(in) + `,"output_tokens":` + itoa(out) + `}}}`
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestObserveUsageEmitsOneSamplePerLogicalMessage(t *testing.T) {
	dir := t.TempDir()
	tpath := filepath.Join(dir, "t.jsonl")
	writeLines(t, tpath, `{"type":"system"}`) // a baseline line so priming has something to skip past

	histDir := t.TempDir()
	sink := history.NewSink(history.Config{Enabled: true, Detail: history.DetailFull, Dir: histDir})
	rs := newReconcileState(fanout.NewObserver(t.TempDir()), nil)
	sess := &state.Session{PID: 7, Agent: "claude", CWD: "/home/u/proj",
		Claude: &state.AgentInfo{SessionID: "s7", Transcript: tpath}}

	// First observe primes the usage cursor to EOF — no sample for the baseline.
	rs.observe(sink, sess, sess.Claude, time.Now())

	// Two models accrue tokens while we watch.
	f, _ := os.OpenFile(tpath, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(assistantUsageModelLine("claude-opus-4-8", 100, 40) + "\n")
	f.WriteString(assistantUsageModelLine("claude-haiku-4-5", 10, 5) + "\n")
	f.WriteString(assistantUsageModelLine("claude-opus-4-8", 20, 8) + "\n")
	f.Close()

	rs.observe(sink, sess, sess.Claude, time.Now())
	sink.Close()

	samples := eventsOfType(readEvents(t, histDir), history.EventUsageSample)
	if len(samples) != 3 {
		t.Fatalf("got %d usage samples, want one per logical message: %+v", len(samples), samples)
	}
	byModel := map[string]history.Event{}
	for _, s := range samples {
		total := byModel[s.Model]
		total.Model = s.Model
		total.TokIn += s.TokIn
		total.TokOut += s.TokOut
		byModel[s.Model] = total
	}
	if o := byModel["claude-opus-4-8"]; o.TokIn != 120 || o.TokOut != 48 {
		t.Errorf("opus sample = %+v, want summed 120/48", o)
	}
	if h := byModel["claude-haiku-4-5"]; h.TokIn != 10 || h.TokOut != 5 {
		t.Errorf("haiku sample = %+v, want 10/5", h)
	}
}

func TestObserveUsagePreservesClaudePricingDimensions(t *testing.T) {
	dir := t.TempDir()
	tpath := filepath.Join(dir, "session.jsonl")
	writeLines(t, tpath, `{"type":"assistant","timestamp":"2026-08-25T10:11:12.123Z","uuid":"row-rich","message":{"id":"msg-rich","role":"assistant","model":"claude-opus-4-8","content":[],"usage":{"input_tokens":11,"output_tokens":12,"cache_read_input_tokens":13,"cache_creation_input_tokens":29,"cache_creation":{"ephemeral_5m_input_tokens":14,"ephemeral_1h_input_tokens":15},"service_tier":"priority","speed":"fast","inference_geo":"us","server_tool_use":{"web_search_requests":2,"web_fetch_requests":3,"code_execution_requests":1,"future_server_tool_requests":4}}}}`)

	histDir := t.TempDir()
	sink := history.NewSink(history.Config{Enabled: true, Detail: history.DetailFull, Dir: histDir})
	rs := newReconcileState(fanout.NewObserver(t.TempDir()), nil)
	sess := &state.Session{PID: 7, Agent: "claude", CWD: "/home/u/proj",
		Claude: &state.AgentInfo{SessionID: "s7", Transcript: tpath}}
	rs.observe(sink, sess, sess.Claude, time.Now())
	sink.Close()

	samples := eventsOfType(readEvents(t, histDir), history.EventUsageSample)
	if len(samples) != 1 {
		t.Fatalf("got %d usage samples, want 1: %+v", len(samples), samples)
	}
	sample := samples[0]
	if got := sample.Ts.Format(time.RFC3339Nano); got != "2026-08-25T10:11:12.123Z" {
		t.Errorf("sample timestamp = %q, want provider timestamp", got)
	}
	if sample.ProviderMessageID != "msg-rich" || !strings.HasPrefix(sample.UsageSourceID, "cusrc_") ||
		!strings.HasPrefix(sample.UsageEventID, "cuev_") || !sample.UsageSnapshot || sample.UsageRevision <= 0 ||
		sample.Source != agentgraph.SourceClaudeTranscript {
		t.Errorf("sample correlation = %+v", sample)
	}
	if sample.SchemaVersion != history.HistorySchemaVersion || sample.ExecutionProvider != "" ||
		sample.BillingRoute != "" || sample.AccountKind != "" || sample.AuthMode != "" {
		t.Errorf("sample billing identity = %+v", sample)
	}
	if sample.TokCacheCreate != 29 || sample.TokCacheCreate5m != 14 || sample.TokCacheCreate1h != 15 {
		t.Errorf("cache dimensions = %+v", sample)
	}
	if sample.Usage == nil || sample.Usage.InputTokens != 11 || sample.Usage.OutputTokens != 12 ||
		sample.Usage.CachedInputTokens != 13 || sample.Usage.CacheWriteInputTokens != 0 ||
		sample.Usage.CacheWrite5mInputTokens != 14 || sample.Usage.CacheWrite1hInputTokens != 15 ||
		sample.Usage.WebSearchRequests != 2 || sample.Usage.WebFetchRequests != 3 ||
		sample.Usage.CodeExecutionRequests != 1 || sample.Usage.UnclassifiedServerToolUnits != 4 {
		t.Errorf("canonical usage = %+v", sample.Usage)
	}
	if sample.ServiceTier != "priority" || sample.Speed != "fast" || sample.InferenceGeo != "us" || sample.WebSearchRequests != 2 || sample.WebFetchRequests != 3 {
		t.Errorf("pricing dimensions = %+v", sample)
	}
}

func TestObserveUsageMapsCombinedCacheWriteOnlyWhenTTLUnknown(t *testing.T) {
	root := filepath.Join(t.TempDir(), "session.jsonl")
	writeLines(t, root, `{"type":"assistant","timestamp":"2026-08-25T10:00:00Z","uuid":"row-1","message":{"id":"msg-1","role":"assistant","model":"claude-opus-4-8","content":[],"usage":{"input_tokens":11,"output_tokens":12,"cache_creation_input_tokens":29}}}`)
	histDir := t.TempDir()
	sink := history.NewSink(history.Config{Enabled: true, Detail: history.DetailFull, Dir: histDir})
	rs := newReconcileState(fanout.NewObserver(t.TempDir()), nil)
	sess := &state.Session{PID: 7, Agent: "claude", CWD: "/home/u/proj",
		Claude: &state.AgentInfo{SessionID: "session-combined", Transcript: root}}
	rs.observe(sink, sess, sess.Claude, time.Now())
	sink.Close()

	samples := eventsOfType(readEvents(t, histDir), history.EventUsageSample)
	if len(samples) != 1 || samples[0].Usage == nil {
		t.Fatalf("combined cache sample = %+v", samples)
	}
	u := samples[0].Usage
	if u.CacheWriteInputTokens != 29 || u.CacheWrite5mInputTokens != 0 || u.CacheWrite1hInputTokens != 0 || samples[0].TokCacheCreate != 29 {
		t.Fatalf("unknown-TTL cache mapping = canonical %+v legacy %+v", u, samples[0])
	}
}

func TestObserveUsageLegacyCutoverCarriesCanonicalIdentity(t *testing.T) {
	histDir := t.TempDir()
	day := time.Now().Local().Format("2006-01-02")
	legacy := fmt.Sprintf(`{"ts":%q,"type":"usage_sample","agent":"claude","session_id":"session-old","tok_in":10}`, time.Now().UTC().Format(time.RFC3339Nano)) + "\n"
	if err := os.WriteFile(filepath.Join(histDir, day+".jsonl"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "session.jsonl")
	writeLines(t, root, `{"type":"assistant","timestamp":"2026-08-25T10:00:00Z","uuid":"row-1","message":{"id":"msg-old","role":"assistant","model":"claude-opus-4-8","content":[],"usage":{"input_tokens":10,"output_tokens":2}}}`)
	sink := history.NewSink(history.Config{Enabled: true, Detail: history.DetailFull, Dir: histDir})
	rs := newReconcileState(fanout.NewObserver(t.TempDir()), nil)
	sess := &state.Session{PID: 7, Agent: "claude", CWD: "/home/u/proj",
		Claude: &state.AgentInfo{SessionID: "session-old", Transcript: root}}
	rs.observe(sink, sess, sess.Claude, time.Now())
	sink.Close()

	markers := eventsOfType(readEvents(t, histDir), history.EventUsageCutover)
	if len(markers) != 1 {
		t.Fatalf("cutover markers = %+v", markers)
	}
	marker := markers[0]
	if marker.SchemaVersion != history.HistorySchemaVersion || marker.ExecutionProvider != "" ||
		marker.BillingRoute != "" || marker.AccountKind != "" || marker.AuthMode != "" ||
		marker.UsageCoverage != "partial_legacy_cutover" || marker.Usage != nil ||
		!marker.UsageSnapshot || marker.UsageRevision <= 0 || !strings.HasPrefix(marker.UsageEventID, "cuev_") {
		t.Fatalf("canonical cutover marker = %+v", marker)
	}
}

func TestObserveUsageDurablyBackfillsMoreThanAsyncBuffer(t *testing.T) {
	dir := t.TempDir()
	tPath := filepath.Join(dir, "session.jsonl")
	const messages = 700
	lines := make([]string, messages)
	for i := range lines {
		lines[i] = fmt.Sprintf(`{"type":"assistant","timestamp":"2026-08-25T10:00:00Z","uuid":"row-%d","message":{"id":"msg-%d","role":"assistant","model":"claude-sonnet-5","content":[],"usage":{"input_tokens":%d,"output_tokens":1}}}`, i, i, i+1)
	}
	writeLines(t, tPath, lines...)

	histDir := t.TempDir()
	sink := history.NewSink(history.Config{Enabled: true, Detail: history.DetailFull, Dir: histDir})
	rs := newReconcileState(fanout.NewObserver(t.TempDir()), nil)
	sess := &state.Session{PID: 7, Agent: "claude", CWD: "/home/u/proj",
		Claude: &state.AgentInfo{SessionID: "session-large", Transcript: tPath}}
	rs.observe(sink, sess, sess.Claude, time.Now())
	sink.Close()

	samples := eventsOfType(readEvents(t, histDir), history.EventUsageSample)
	if len(samples) != messages {
		t.Fatalf("durable backfill samples = %d, want %d", len(samples), messages)
	}
}

func TestObserveUsageRetriesTransientTrackerInitialization(t *testing.T) {
	dir := t.TempDir()
	tPath := filepath.Join(dir, "session.jsonl")
	writeLines(t, tPath, `{"type":"assistant","timestamp":"2026-08-25T10:00:00Z","uuid":"row-1","message":{"id":"msg-1","role":"assistant","model":"claude-sonnet-5","content":[],"usage":{"input_tokens":10,"output_tokens":2}}}`)

	histDir := t.TempDir()
	cursorPath := filepath.Join(histDir, "claude-usage-cursors-v1.json")
	if err := os.WriteFile(cursorPath, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	sink := history.NewSink(history.Config{Enabled: true, Detail: history.DetailFull, Dir: histDir})
	rs := newReconcileState(fanout.NewObserver(t.TempDir()), nil)
	sess := &state.Session{PID: 7, Agent: "claude", CWD: "/home/u/proj",
		Claude: &state.AgentInfo{SessionID: "session-retry", Transcript: tPath}}

	rs.observe(sink, sess, sess.Claude, time.Now())
	if rs.usage != nil {
		t.Fatal("invalid cursor unexpectedly initialized tracker")
	}
	if err := os.WriteFile(cursorPath, []byte(`{"version":2,"sessions":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rs.observe(sink, sess, sess.Claude, time.Now())
	if rs.usage == nil {
		t.Fatal("tracker initialization was not retried after transient failure")
	}
	sink.Close()

	samples := eventsOfType(readEvents(t, histDir), history.EventUsageSample)
	if len(samples) != 1 || samples[0].ProviderMessageID != "msg-1" {
		t.Fatalf("usage after initialization retry = %+v", samples)
	}
}

func TestObserveUsageDisabledHistoryDoesNotAdvanceCursor(t *testing.T) {
	root := filepath.Join(t.TempDir(), "session.jsonl")
	writeLines(t, root, `{"type":"assistant","timestamp":"2026-08-25T10:00:00Z","uuid":"row-1","message":{"id":"msg-1","role":"assistant","model":"claude-sonnet-5","content":[],"usage":{"input_tokens":10,"output_tokens":2}}}`)
	histDir := t.TempDir()
	rs := newReconcileState(fanout.NewObserver(t.TempDir()), nil)
	sess := &state.Session{PID: 7, Agent: "claude", CWD: "/home/u/proj",
		Claude: &state.AgentInfo{SessionID: "session-disabled", Transcript: root}}

	disabled := history.NewSink(history.Config{Enabled: false, Detail: history.DetailFull, Dir: histDir})
	rs.observe(disabled, sess, sess.Claude, time.Now())
	disabled.Close()
	if rs.usage != nil {
		t.Fatal("disabled history initialized or advanced a usage tracker")
	}

	enabled := history.NewSink(history.Config{Enabled: true, Detail: history.DetailFull, Dir: histDir})
	rs.observe(enabled, sess, sess.Claude, time.Now())
	enabled.Close()
	samples := eventsOfType(readEvents(t, histDir), history.EventUsageSample)
	if len(samples) != 1 || samples[0].ProviderMessageID != "msg-1" {
		t.Fatalf("usage was not backfilled after enabling history: %+v", samples)
	}
}

func TestObserveLabelEmitsOnChangeOnly(t *testing.T) {
	dir := t.TempDir()
	tpath := filepath.Join(dir, "t.jsonl")
	writeLines(t, tpath, `{"type":"system"}`) // label tracking is transcript-independent, but observe still needs a path

	histDir := t.TempDir()
	sink := history.NewSink(history.Config{Enabled: true, Detail: history.DetailFull, Dir: histDir})
	rs := newReconcileState(fanout.NewObserver(t.TempDir()), nil)
	// pid 424242 has no ~/.claude/sessions file, so label.RawName falls to the
	// wezterm window title — the name we control here.
	sess := &state.Session{PID: 424242, Agent: "claude", CWD: "/home/u/proj",
		Wezterm: &state.WeztermInfo{WindowTitle: "first-name"},
		Claude:  &state.AgentInfo{SessionID: "s1", Transcript: tpath}}

	rs.observe(sink, sess, sess.Claude, time.Now()) // emit "first-name"
	rs.observe(sink, sess, sess.Claude, time.Now()) // unchanged → no emit

	sess.Wezterm.WindowTitle = "second-name"        // user renamed the session
	rs.observe(sink, sess, sess.Claude, time.Now()) // emit "second-name"
	rs.observe(sink, sess, sess.Claude, time.Now()) // unchanged → no emit
	sink.Close()

	labels := eventsOfType(readEvents(t, histDir), history.EventSessionLabel)
	if len(labels) != 2 {
		t.Fatalf("got %d session_label events, want 2 (one per distinct name): %+v", len(labels), labels)
	}
	if labels[0].Label != "first-name" || labels[1].Label != "second-name" {
		t.Errorf("labels = %q, %q; want first-name, second-name", labels[0].Label, labels[1].Label)
	}
	if labels[0].SessionID != "s1" || labels[0].PID != 424242 {
		t.Errorf("label event identity = %+v, want session s1 / pid 424242", labels[0])
	}
}

func TestObserveLabelTracksCodexDisplayNameAndNativeOverrideOncePerVisibleChange(t *testing.T) {
	histDir := t.TempDir()
	sink := history.NewSink(history.Config{Enabled: true, Detail: history.DetailFull, Dir: histDir})
	rs := newReconcileState(fanout.NewObserver(t.TempDir()), nil)
	sess := &state.Session{
		PID: 424244, Agent: state.AgentKindCodex, CWD: "/home/u/proj",
		Codex: &state.AgentInfo{SessionID: "thread-1"},
		AgentGraph: &state.AgentGraph{
			RootID: "thread-1", Nodes: []state.AgentNode{{ID: "thread-1", Nickname: "initial-native"}},
		},
	}

	rs.observeLabel(sink, sess, sess.Codex, time.Now())
	rs.observeLabel(sink, sess, sess.Codex, time.Now())
	sess.DisplayName = &state.DisplayName{
		Value: "generated-display-name", Origin: state.DisplayNameGenerated, ConversationID: "thread-1",
	}
	rs.observeLabel(sink, sess, sess.Codex, time.Now())
	rs.observeLabel(sink, sess, sess.Codex, time.Now())
	sess.DisplayName = nil
	sess.AgentGraph.Nodes[0].Nickname = "manual-native-name"
	rs.observeLabel(sink, sess, sess.Codex, time.Now())
	rs.observeLabel(sink, sess, sess.Codex, time.Now())
	sink.Close()

	labels := eventsOfType(readEvents(t, histDir), history.EventSessionLabel)
	if len(labels) != 3 {
		t.Fatalf("got %d session_label events, want one per visible change: %+v", len(labels), labels)
	}
	want := []string{"initial-native", "generated-display-name", "manual-native-name"}
	for i := range want {
		if labels[i].Label != want[i] || labels[i].SessionID != "thread-1" {
			t.Fatalf("label[%d] = %+v, want %q on thread-1", i, labels[i], want[i])
		}
	}
}

// The authoritative name lives in ~/.claude/sessions/<pid>.json, and `/name`
// rewrites it under a running session. observeLabel memoizes that read against
// the file's stamp, so this pins the invalidation: a rename must still produce a
// second session_label event on the next tick.
func TestObserveLabelEmitsAgainWhenTheSessionFileIsRenamed(t *testing.T) {
	dir := t.TempDir()
	tpath := filepath.Join(dir, "t.jsonl")
	writeLines(t, tpath, `{"type":"system"}`)

	home := t.TempDir()
	t.Setenv("HOME", home)
	sessDir := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	spath := filepath.Join(sessDir, "424243.json")
	writeSessionName := func(name string) {
		t.Helper()
		body := fmt.Sprintf(`{"pid":424243,"name":%q,"status":"busy"}`, name)
		if err := os.WriteFile(spath, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeSessionName("aaa-name")

	histDir := t.TempDir()
	sink := history.NewSink(history.Config{Enabled: true, Detail: history.DetailFull, Dir: histDir})
	rs := newReconcileState(fanout.NewObserver(t.TempDir()), nil)
	sess := &state.Session{PID: 424243, Agent: "claude", CWD: "/home/u/proj",
		Wezterm: &state.WeztermInfo{WindowTitle: "ignored-window-title"},
		Claude:  &state.AgentInfo{SessionID: "s1", Transcript: tpath}}

	rs.observe(sink, sess, sess.Claude, time.Now()) // emit "aaa-name"
	rs.observe(sink, sess, sess.Claude, time.Now()) // unchanged → no emit

	// `/name bbb-name`. Same length as the old name, so the file's size does not
	// move and mtime alone has to carry the invalidation; forced forward so the
	// test does not depend on the filesystem's timestamp granularity.
	writeSessionName("bbb-name")
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(spath, future, future); err != nil {
		t.Fatal(err)
	}

	rs.observe(sink, sess, sess.Claude, time.Now()) // emit "bbb-name"
	rs.observe(sink, sess, sess.Claude, time.Now()) // unchanged → no emit
	sink.Close()

	labels := eventsOfType(readEvents(t, histDir), history.EventSessionLabel)
	if len(labels) != 2 {
		t.Fatalf("got %d session_label events, want 2 (one per distinct name): %+v", len(labels), labels)
	}
	if labels[0].Label != "aaa-name" || labels[1].Label != "bbb-name" {
		t.Errorf("labels = %q, %q; want aaa-name, bbb-name — the rename did not reach the timeline", labels[0].Label, labels[1].Label)
	}
}

// A /clear mints a fresh session id in the same pane, and the timeline reads that
// as a new lane. The name is unchanged — it is the pane's name — so a pid-keyed
// dedup would swallow it and leave the new lane permanently nameless. The label
// must be re-announced for the new session id.
func TestObserveLabelReAnnouncesTheNameToANewSessionInTheSameProcess(t *testing.T) {
	dir := t.TempDir()
	tpath := filepath.Join(dir, "t.jsonl")
	writeLines(t, tpath, `{"type":"system"}`)

	histDir := t.TempDir()
	sink := history.NewSink(history.Config{Enabled: true, Detail: history.DetailFull, Dir: histDir})
	rs := newReconcileState(fanout.NewObserver(t.TempDir()), nil)
	sess := &state.Session{PID: 424242, Agent: "claude", CWD: "/home/u/proj",
		Wezterm: &state.WeztermInfo{WindowTitle: "digest-status"},
		Claude:  &state.AgentInfo{SessionID: "s1", Transcript: tpath}}

	rs.observe(sink, sess, sess.Claude, time.Now()) // emit for s1
	rs.observe(sink, sess, sess.Claude, time.Now()) // unchanged → no emit

	sess.Claude.SessionID = "s2"                    // /clear: same pane, same name, new session
	rs.observe(sink, sess, sess.Claude, time.Now()) // emit for s2
	rs.observe(sink, sess, sess.Claude, time.Now()) // unchanged → no emit
	sink.Close()

	labels := eventsOfType(readEvents(t, histDir), history.EventSessionLabel)
	if len(labels) != 2 {
		t.Fatalf("got %d session_label events, want one per session: %+v", len(labels), labels)
	}
	if labels[0].SessionID != "s1" || labels[1].SessionID != "s2" {
		t.Errorf("label sessions = %q, %q; want s1 then s2", labels[0].SessionID, labels[1].SessionID)
	}
	if labels[1].Label != "digest-status" {
		t.Errorf("second label = %q, want the name the pane still carries", labels[1].Label)
	}
}

// Label cursors expire with their PROCESS, not with the key's liveness: every
// cursor a live pid owns survives, and a dead pid takes all of its cursors with
// it.
func TestPruneDropsLabelCursorsByTheirProcess(t *testing.T) {
	rs := newReconcileState(fanout.NewObserver(t.TempDir()), nil)
	rs.labels["s:superseded"] = labelCursor{name: "digest-status", pid: 1} // pre-/clear session, pid still alive
	rs.labels["s:live"] = labelCursor{name: "digest-status", pid: 1}
	rs.labels["p:99"] = labelCursor{name: "some-pane", pid: 99} // its process is gone

	rs.prune(map[int]*state.Session{
		1: {PID: 1, Claude: &state.AgentInfo{SessionID: "live"}},
	})

	if _, ok := rs.labels["s:live"]; !ok {
		t.Error("cursor for a live session should survive")
	}
	if _, ok := rs.labels["s:superseded"]; !ok {
		t.Error("cursor for a superseded session should survive while its process does")
	}
	if _, ok := rs.labels["p:99"]; ok {
		t.Error("cursors of a dead pid should be pruned")
	}
}

// A tick that cannot see a session's claude info skips observe() but still runs
// prune(). Expiring the cursor there would re-announce an identical name the
// moment the info came back — a duplicate label event per blip, which is exactly
// what the first restart after the session-id keying produced.
func TestObserveLabelDoesNotReAnnounceAfterATickWithoutClaudeInfo(t *testing.T) {
	dir := t.TempDir()
	tpath := filepath.Join(dir, "t.jsonl")
	writeLines(t, tpath, `{"type":"system"}`)

	histDir := t.TempDir()
	sink := history.NewSink(history.Config{Enabled: true, Detail: history.DetailFull, Dir: histDir})
	rs := newReconcileState(fanout.NewObserver(t.TempDir()), nil)
	sess := &state.Session{PID: 424242, Agent: "claude", CWD: "/home/u/proj",
		Wezterm: &state.WeztermInfo{WindowTitle: "digest-status"},
		Claude:  &state.AgentInfo{SessionID: "s1", Transcript: tpath}}
	live := map[int]*state.Session{424242: sess}

	rs.observe(sink, sess, sess.Claude, time.Now())
	rs.prune(live)

	claude := sess.Claude
	sess.Claude = nil // the tick the reconciler could not resolve: observe is skipped
	rs.prune(live)
	sess.Claude = claude

	rs.observe(sink, sess, sess.Claude, time.Now())
	sink.Close()

	labels := eventsOfType(readEvents(t, histDir), history.EventSessionLabel)
	if len(labels) != 1 {
		t.Fatalf("got %d session_label events, want 1 (the name never changed): %+v", len(labels), labels)
	}
}

func TestApplyFocusRecordsOnChangeOnly(t *testing.T) {
	histDir := t.TempDir()
	sink := history.NewSink(history.Config{Enabled: true, Detail: history.DetailFull, Dir: histDir})

	a := &state.Session{PID: 1, Agent: "claude",
		Hyprland: &state.HyprlandInfo{Address: "0xA"}, Claude: &state.AgentInfo{SessionID: "sa"}}
	b := &state.Session{PID: 2, Agent: "claude",
		Hyprland: &state.HyprlandInfo{Address: "0xB"}, Claude: &state.AgentInfo{SessionID: "sb"}}
	m := map[int]*state.Session{1: a, 2: b}
	now := time.Now()

	applyFocus(m, "0xA", sink, now) // focus → A : emit focus{sa}
	applyFocus(m, "0xA", sink, now) // unchanged : no emit
	if !a.Focused || b.Focused {
		t.Errorf("after focus A: a.Focused=%v b.Focused=%v, want true/false", a.Focused, b.Focused)
	}
	applyFocus(m, "0xB", sink, now) // focus → B : emit focus{sb}
	applyFocus(m, "0xC", sink, now) // a non-agent window : emit focus{""} (focus left all agents)
	applyFocus(m, "0xC", sink, now) // still no agent focused : no emit
	applyFocus(m, "", sink, now)    // no active window : still no agent : no emit
	if a.Focused || b.Focused {
		t.Errorf("after focus leaves: a.Focused=%v b.Focused=%v, want both false", a.Focused, b.Focused)
	}
	sink.Close()

	focus := eventsOfType(readEvents(t, histDir), history.EventFocus)
	if len(focus) != 3 {
		t.Fatalf("got %d focus events, want 3 (A, B, left-all): %+v", len(focus), focus)
	}
	if focus[0].SessionID != "sa" || focus[1].SessionID != "sb" {
		t.Errorf("focus[0]=%q focus[1]=%q, want sa, sb", focus[0].SessionID, focus[1].SessionID)
	}
	if focus[2].SessionID != "" {
		t.Errorf("focus[2] SessionID = %q, want empty (focus left all agent windows)", focus[2].SessionID)
	}
}

func TestObserveUsageBackfillsThenSamples(t *testing.T) {
	dir := t.TempDir()
	tpath := filepath.Join(dir, "t.jsonl")
	// Pre-existing complete records are backfilled on first discovery.
	writeLines(t, tpath, `{"type":"assistant","message":{"role":"assistant","content":[],"usage":{"input_tokens":9999,"output_tokens":9999}}}`)

	histDir := t.TempDir()
	sink := history.NewSink(history.Config{Enabled: true, Detail: history.DetailFull, Dir: histDir})
	rs := newReconcileState(fanout.NewObserver(t.TempDir()), nil)
	sess := &state.Session{PID: 1, Agent: "claude", CWD: "/home/u/proj",
		Claude: &state.AgentInfo{SessionID: "s1", Transcript: tpath}}

	// First observe emits the backlog using provider timestamps when available.
	rs.observe(sink, sess, sess.Claude, time.Now())

	// New usage accrues while we watch.
	f, _ := os.OpenFile(tpath, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(`{"type":"assistant","message":{"role":"assistant","content":[],"usage":{"input_tokens":120,"output_tokens":34}}}` + "\n")
	f.Close()

	rs.observe(sink, sess, sess.Claude, time.Now())
	sink.Close()

	samples := eventsOfType(readEvents(t, histDir), history.EventUsageSample)
	if len(samples) != 2 {
		t.Fatalf("got %d usage samples, want backlog plus appended usage: %+v", len(samples), samples)
	}
	if samples[0].TokIn != 9999 || samples[0].TokOut != 9999 || samples[1].TokIn != 120 || samples[1].TokOut != 34 {
		t.Errorf("usage samples = %+v, want backlog 9999/9999 then appended 120/34", samples)
	}
}
