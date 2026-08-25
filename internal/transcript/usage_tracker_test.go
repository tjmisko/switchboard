package transcript

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func trackedUsageLine(ts, id, model string, input, output int64) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"uuid":%q,"message":{"id":%q,"role":"assistant","model":%q,"content":[],"usage":{"input_tokens":%d,"output_tokens":%d}}}`,
		ts, "row-"+id, id, model, input, output)
}

func appendUsageLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, line := range lines {
		if _, err := f.WriteString(line + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUsageTrackerDeduplicatesAndCountsOnlyPositiveRevisionsAcrossPolls(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "session.jsonl")
	first := trackedUsageLine("2026-08-25T10:00:00Z", "msg-1", "claude-sonnet-5", 100, 20)
	appendUsageLines(t, root, first)

	tracker, err := NewUsageTracker(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	deltas, err := tracker.ObserveSession("session-1", root, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(deltas) != 1 || deltas[0].Usage.InputTokens != 100 || deltas[0].Usage.OutputTokens != 20 {
		t.Fatalf("first delta = %+v, want one 100/20 message", deltas)
	}

	// An identical streamed fragment arriving in a later poll is not a second
	// provider response.
	appendUsageLines(t, root, first)
	deltas, err = tracker.ObserveSession("session-1", root, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(deltas) != 0 {
		t.Fatalf("identical later fragment replayed usage: %+v", deltas)
	}

	// A later authoritative revision contributes only its positive increase.
	appendUsageLines(t, root, trackedUsageLine("2026-08-25T10:00:03Z", "msg-1", "claude-sonnet-5", 125, 31))
	deltas, err = tracker.ObserveSession("session-1", root, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(deltas) != 1 || deltas[0].Usage.InputTokens != 25 || deltas[0].Usage.OutputTokens != 11 {
		t.Fatalf("revision delta = %+v, want one +25/+11 message", deltas)
	}

	// A counter regression is ignored rather than producing a negative sample or
	// lowering the high-water mark.
	appendUsageLines(t, root, trackedUsageLine("2026-08-25T10:00:04Z", "msg-1", "claude-sonnet-5", 120, 29))
	deltas, err = tracker.ObserveSession("session-1", root, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(deltas) != 0 {
		t.Fatalf("regression produced usage: %+v", deltas)
	}
}

func TestUsageTrackerIncludesRootFlatChildrenAndWorkflowChildrenOnce(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "session.jsonl")
	appendUsageLines(t, root,
		trackedUsageLine("2026-08-25T10:00:00Z", "msg-root", "claude-opus-4-8", 10, 1),
		trackedUsageLine("2026-08-25T10:00:01Z", "msg-shared", "claude-opus-4-8", 20, 2),
	)
	childDir := filepath.Join(dir, "session", "subagents")
	if err := os.MkdirAll(childDir, 0o755); err != nil {
		t.Fatal(err)
	}
	direct := filepath.Join(childDir, "agent-child.jsonl")
	appendUsageLines(t, direct,
		trackedUsageLine("2026-08-25T10:00:01Z", "msg-shared", "claude-opus-4-8", 20, 2),
		trackedUsageLine("2026-08-25T10:00:02Z", "msg-child", "claude-haiku-4-5", 30, 3),
	)
	workflowDir := filepath.Join(childDir, "workflows", "wf-test")
	if err := os.MkdirAll(workflowDir, 0o755); err != nil {
		t.Fatal(err)
	}
	workflow := filepath.Join(workflowDir, "agent-workflow.jsonl")
	appendUsageLines(t, workflow,
		trackedUsageLine("2026-08-25T10:00:03Z", "msg-workflow", "claude-sonnet-5", 40, 4),
	)
	// This is not an agent transcript and must never be interpreted as usage.
	appendUsageLines(t, filepath.Join(workflowDir, "journal.jsonl"),
		trackedUsageLine("2026-08-25T10:00:04Z", "msg-journal", "claude-sonnet-5", 999, 999),
	)

	tracker, err := NewUsageTracker(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	deltas, err := tracker.ObserveSession("session-1", root, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(deltas) != 4 {
		t.Fatalf("got %d deltas, want root/shared/direct/workflow exactly once: %+v", len(deltas), deltas)
	}
	var input int64
	byID := map[string]UsageDelta{}
	for _, delta := range deltas {
		input += delta.Usage.InputTokens
		byID[delta.ProviderMessageID] = delta
	}
	if input != 100 {
		t.Fatalf("input total = %d, want 10+20+30+40 (shared deduped)", input)
	}
	if got := byID["msg-child"].SourceID; got != "child:agent-child" {
		t.Errorf("direct child source = %q", got)
	}
	if got := byID["msg-workflow"].SourceID; got != "child:workflows/wf-test/agent-workflow" {
		t.Errorf("workflow child source = %q", got)
	}
	if _, counted := byID["msg-journal"]; counted {
		t.Error("workflow journal was counted as an agent transcript")
	}
}

func TestUsageTrackerRestartNeitherLosesNorReplaysCompletedMessages(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "session.jsonl")
	appendUsageLines(t, root, trackedUsageLine("2026-08-25T10:00:00Z", "msg-1", "claude-sonnet-5", 10, 2))
	stateDir := t.TempDir()

	first, err := NewUsageTracker(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	deltas, err := first.ObserveSession("session-1", root, time.Now())
	if err != nil || len(deltas) != 1 {
		t.Fatalf("first observation = %+v, %v", deltas, err)
	}
	statePath := filepath.Join(stateDir, usageCursorFilename)
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), dir) || strings.Contains(string(raw), root) {
		t.Fatal("durable cursor persisted a transcript filesystem path")
	}
	if info, err := os.Stat(statePath); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("cursor permissions = %o, want 600", info.Mode().Perm())
	}

	restarted, err := NewUsageTracker(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	deltas, err = restarted.ObserveSession("session-1", root, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(deltas) != 0 {
		t.Fatalf("restart replayed completed message: %+v", deltas)
	}

	appendUsageLines(t, root, trackedUsageLine("2026-08-25T10:00:05Z", "msg-2", "claude-sonnet-5", 7, 3))
	deltas, err = restarted.ObserveSession("session-1", root, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(deltas) != 1 || deltas[0].ProviderMessageID != "msg-2" || deltas[0].Usage.InputTokens != 7 {
		t.Fatalf("post-restart new usage was lost or replayed: %+v", deltas)
	}
}

func TestUsageTrackerReplacementRescansWithoutReplayingKnownMessages(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "session.jsonl")
	writeTranscriptBody := func(system string, messages ...string) {
		t.Helper()
		body := `{"type":"system","subtype":` + fmt.Sprintf("%q", system) + `}` + "\n"
		for _, message := range messages {
			body += message + "\n"
		}
		if err := os.WriteFile(root, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	one := trackedUsageLine("2026-08-25T10:00:00Z", "msg-1", "claude-sonnet-5", 10, 2)
	writeTranscriptBody("generation-a", one)
	tracker, err := NewUsageTracker(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if deltas, err := tracker.ObserveSession("session-1", root, time.Now()); err != nil || len(deltas) != 1 {
		t.Fatalf("initial observation = %+v, %v", deltas, err)
	}

	// Replace with a different first-line generation and a file larger than the
	// old offset. Size-only truncation detection would miss this replacement.
	two := trackedUsageLine("2026-08-25T10:00:05Z", "msg-2", "claude-sonnet-5", 20, 4)
	writeTranscriptBody("generation-b-with-a-longer-prefix", one, two)
	deltas, err := tracker.ObserveSession("session-1", root, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(deltas) != 1 || deltas[0].ProviderMessageID != "msg-2" || deltas[0].Usage.InputTokens != 20 {
		t.Fatalf("replacement result = %+v, want only new msg-2", deltas)
	}
}

func TestUsageTrackerPreservesPricingDimensionsAndProviderTimestamp(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "session.jsonl")
	line := `{"type":"assistant","timestamp":"2026-08-25T10:11:12.123Z","uuid":"row-1","message":{"id":"msg-rich","role":"assistant","model":"claude-opus-4-8","content":[],"usage":{"input_tokens":11,"output_tokens":12,"cache_read_input_tokens":13,"cache_creation_input_tokens":999,"cache_creation":{"ephemeral_5m_input_tokens":14,"ephemeral_1h_input_tokens":15},"service_tier":"priority","speed":"fast","inference_geo":"us","server_tool_use":{"web_search_requests":2,"web_fetch_requests":3}}}}`
	appendUsageLines(t, root, line)
	tracker, err := NewUsageTracker(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	deltas, err := tracker.ObserveSession("session-1", root, time.Now())
	if err != nil || len(deltas) != 1 {
		t.Fatalf("rich observation = %+v, %v", deltas, err)
	}
	delta := deltas[0]
	if delta.Model != "claude-opus-4-8" || delta.ProviderMessageID != "msg-rich" {
		t.Errorf("identity = %+v", delta)
	}
	if got := delta.Timestamp.Format(time.RFC3339Nano); got != "2026-08-25T10:11:12.123Z" {
		t.Errorf("timestamp = %q", got)
	}
	u := delta.Usage
	if u.CacheWrite5mTokens != 14 || u.CacheWrite1hTokens != 15 || u.CacheCreationTokens != 29 {
		t.Errorf("cache dimensions = %+v, want 14/15 and combined 29", u)
	}
	if u.ServiceTier != "priority" || u.Speed != "fast" || u.InferenceGeo != "us" || u.WebSearchRequests != 2 || u.WebFetchRequests != 3 {
		t.Errorf("pricing dimensions = %+v", u)
	}
}
