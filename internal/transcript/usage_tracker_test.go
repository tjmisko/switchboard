package transcript

import (
	"errors"
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

func syncUsageSnapshots(tracker *UsageTracker, sessionID, root string, now time.Time) ([]UsageSnapshot, error) {
	return tracker.SyncSession(sessionID, root, now, func([]UsageSnapshot) error { return nil })
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
	snapshots, err := syncUsageSnapshots(tracker, "session-1", root, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 1 || snapshots[0].Usage.InputTokens != 100 || snapshots[0].Usage.OutputTokens != 20 {
		t.Fatalf("first snapshot = %+v, want one 100/20 message", snapshots)
	}
	firstEventID := snapshots[0].UsageEventID
	firstRevision := snapshots[0].UsageRevision
	if firstRevision <= 0 {
		t.Fatalf("first revision = %d, want positive", firstRevision)
	}

	// An identical streamed fragment arriving in a later poll is not a second
	// provider response.
	appendUsageLines(t, root, first)
	snapshots, err = syncUsageSnapshots(tracker, "session-1", root, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 0 {
		t.Fatalf("identical later fragment replayed usage: %+v", snapshots)
	}

	// A later authoritative revision replaces the prior full snapshot under the
	// same stable event id.
	appendUsageLines(t, root, trackedUsageLine("2026-08-25T10:00:03Z", "msg-1", "claude-sonnet-5", 125, 31))
	snapshots, err = syncUsageSnapshots(tracker, "session-1", root, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 1 || snapshots[0].UsageEventID != firstEventID || snapshots[0].UsageRevision <= firstRevision || snapshots[0].Usage.InputTokens != 125 || snapshots[0].Usage.OutputTokens != 31 {
		t.Fatalf("revision snapshot = %+v, want an increasing revision with full 125/31 under %q", snapshots, firstEventID)
	}

	// A counter regression is ignored rather than producing a negative sample or
	// lowering the high-water mark.
	appendUsageLines(t, root, trackedUsageLine("2026-08-25T10:00:04Z", "msg-1", "claude-sonnet-5", 120, 29))
	snapshots, err = syncUsageSnapshots(tracker, "session-1", root, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 0 {
		t.Fatalf("regression produced usage: %+v", snapshots)
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
	snapshots, err := syncUsageSnapshots(tracker, "session-1", root, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 4 {
		t.Fatalf("got %d snapshots, want root/shared/direct/workflow exactly once: %+v", len(snapshots), snapshots)
	}
	var input int64
	byID := map[string]UsageSnapshot{}
	for _, snapshot := range snapshots {
		input += snapshot.Usage.InputTokens
		byID[snapshot.ProviderMessageID] = snapshot
	}
	if input != 100 {
		t.Fatalf("input total = %d, want 10+20+30+40 (shared deduped)", input)
	}
	if got := byID["msg-child"].SourceID; !strings.HasPrefix(got, "cusrc_") || strings.Contains(got, "child") {
		t.Errorf("direct child source is not opaque: %q", got)
	}
	if got := byID["msg-workflow"].SourceID; !strings.HasPrefix(got, "cusrc_") || strings.Contains(got, "workflow") {
		t.Errorf("workflow child source is not opaque: %q", got)
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
	snapshots, err := syncUsageSnapshots(first, "session-1", root, time.Now())
	if err != nil || len(snapshots) != 1 {
		t.Fatalf("first observation = %+v, %v", snapshots, err)
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
	snapshots, err = syncUsageSnapshots(restarted, "session-1", root, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 0 {
		t.Fatalf("restart replayed completed message: %+v", snapshots)
	}

	appendUsageLines(t, root, trackedUsageLine("2026-08-25T10:00:05Z", "msg-2", "claude-sonnet-5", 7, 3))
	snapshots, err = syncUsageSnapshots(restarted, "session-1", root, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 1 || snapshots[0].ProviderMessageID != "msg-2" || snapshots[0].Usage.InputTokens != 7 {
		t.Fatalf("post-restart new usage was lost or replayed: %+v", snapshots)
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
	if snapshots, err := syncUsageSnapshots(tracker, "session-1", root, time.Now()); err != nil || len(snapshots) != 1 {
		t.Fatalf("initial observation = %+v, %v", snapshots, err)
	}

	// Replace with a different first-line generation and a file larger than the
	// old offset. Size-only truncation detection would miss this replacement.
	two := trackedUsageLine("2026-08-25T10:00:05Z", "msg-2", "claude-sonnet-5", 20, 4)
	writeTranscriptBody("generation-b-with-a-longer-prefix", one, two)
	snapshots, err := syncUsageSnapshots(tracker, "session-1", root, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 1 || snapshots[0].ProviderMessageID != "msg-2" || snapshots[0].Usage.InputTokens != 20 {
		t.Fatalf("replacement result = %+v, want only new msg-2", snapshots)
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
	snapshots, err := syncUsageSnapshots(tracker, "session-1", root, time.Now())
	if err != nil || len(snapshots) != 1 {
		t.Fatalf("rich observation = %+v, %v", snapshots, err)
	}
	snapshot := snapshots[0]
	if snapshot.Model != "claude-opus-4-8" || snapshot.ProviderMessageID != "msg-rich" {
		t.Errorf("identity = %+v", snapshot)
	}
	if got := snapshot.Timestamp.Format(time.RFC3339Nano); got != "2026-08-25T10:11:12.123Z" {
		t.Errorf("timestamp = %q", got)
	}
	u := snapshot.Usage
	if u.CacheWrite5mTokens != 14 || u.CacheWrite1hTokens != 15 || u.CacheCreationTokens != 29 {
		t.Errorf("cache dimensions = %+v, want 14/15 and combined 29", u)
	}
	if u.ServiceTier != "priority" || u.Speed != "fast" || u.InferenceGeo != "us" || u.WebSearchRequests != 2 || u.WebFetchRequests != 3 {
		t.Errorf("pricing dimensions = %+v", u)
	}
}

func TestUsageTrackerBackfillBatchExceedsLegacyAsyncBuffer(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "session.jsonl")
	const messages = 700
	lines := make([]string, messages)
	for i := range lines {
		lines[i] = trackedUsageLine("2026-08-25T10:00:00Z", fmt.Sprintf("msg-%04d", i), "claude-sonnet-5", int64(i+1), 1)
	}
	appendUsageLines(t, root, lines...)
	tracker, err := NewUsageTracker(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var appended []UsageSnapshot
	snapshots, err := tracker.SyncSession("session-large", root, time.Now(), func(batch []UsageSnapshot) error {
		appended = append(appended, batch...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != messages || len(appended) != messages {
		t.Fatalf("large backfill returned/appended %d/%d snapshots, want %d", len(snapshots), len(appended), messages)
	}
}

func TestUsageTrackerAppendFailureDoesNotAdvanceCursor(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "session.jsonl")
	appendUsageLines(t, root, trackedUsageLine("2026-08-25T10:00:00Z", "msg-1", "claude-sonnet-5", 10, 2))
	stateDir := t.TempDir()
	tracker, err := NewUsageTracker(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	var failed []UsageSnapshot
	_, err = tracker.SyncSession("session-1", root, time.Now(), func(batch []UsageSnapshot) error {
		failed = append(failed, batch...)
		return errors.New("write failed at /private/customer/transcript.jsonl")
	})
	if err == nil {
		t.Fatal("append failure was ignored")
	}
	if strings.Contains(err.Error(), "/private/customer") {
		t.Fatalf("append error leaked a path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, usageCursorFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cursor was persisted after append failure: %v", err)
	}

	appendUsageLines(t, root, trackedUsageLine("2026-08-25T10:00:01Z", "msg-2", "claude-sonnet-5", 20, 4))
	var retried []UsageSnapshot
	_, err = tracker.SyncSession("session-1", root, time.Now(), func(batch []UsageSnapshot) error {
		retried = append(retried, batch...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(failed) != 1 || len(retried) != 2 || retried[0].UsageEventID != failed[0].UsageEventID || retried[0].UsageRevision < failed[0].UsageRevision {
		t.Fatalf("retry snapshots = %+v after failed batch %+v; cursor advanced or ids changed", retried, failed)
	}
}

func TestUsageTrackerAppendBeforePersistCrashRetriesSameSnapshotID(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "session.jsonl")
	appendUsageLines(t, root, trackedUsageLine("2026-08-25T10:00:00Z", "msg-1", "claude-sonnet-5", 10, 2))
	stateDir := t.TempDir()
	tracker, err := NewUsageTracker(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	tracker.persistOverride = func(usageTrackerState) error { return errors.New("disk error with /private/state/path") }
	var durable UsageSnapshot
	_, err = tracker.SyncSession("session-1", root, time.Now(), func(batch []UsageSnapshot) error {
		durable = batch[0]
		// Model the history append that survived a crash. The upgrade scanner must
		// recognize this as an idempotent sample, not legacy additive history.
		line := fmt.Sprintf("{\"type\":\"usage_sample\",\"agent\":\"claude\",\"session_id\":\"session-1\",\"usage_event_id\":%q}\n", durable.UsageEventID)
		return os.WriteFile(filepath.Join(stateDir, "2026-08-25.jsonl"), []byte(line), 0o600)
	})
	if err == nil || strings.Contains(err.Error(), "/private/state") {
		t.Fatalf("persist error = %v, want sanitized failure", err)
	}

	restarted, err := NewUsageTracker(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	var replay UsageSnapshot
	_, err = restarted.SyncSession("session-1", root, time.Now(), func(batch []UsageSnapshot) error {
		replay = batch[0]
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if replay.UsageEventID != durable.UsageEventID || replay.UsageRevision < durable.UsageRevision || replay.Usage != durable.Usage {
		t.Fatalf("crash retry = %+v, want same authoritative upsert id/value and nondecreasing revision as %+v", replay, durable)
	}
}

func TestUsageTrackerLegacyUpgradeMarksPartialAndDoesNotReplay(t *testing.T) {
	stateDir := t.TempDir()
	legacy := `{"ts":"2026-08-25T10:00:00Z","type":"usage_sample","agent":"claude","session_id":"session-old","tok_in":10}` + "\n"
	if err := os.WriteFile(filepath.Join(stateDir, "2026-08-25.jsonl"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	root := filepath.Join(dir, "session.jsonl")
	appendUsageLines(t, root,
		trackedUsageLine("2026-08-25T10:00:00Z", "msg-legacy", "claude-sonnet-5", 10, 2),
		trackedUsageLine("2026-08-25T13:00:00Z", "msg-new", "claude-sonnet-5", 20, 4),
	)
	cutover := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	tracker, err := newUsageTracker(stateDir, cutover)
	if err != nil {
		t.Fatal(err)
	}
	var appended []UsageSnapshot
	_, err = tracker.SyncSession("session-old", root, cutover, func(batch []UsageSnapshot) error {
		appended = append(appended, batch...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(appended) != 2 || !appended[0].Cutover || appended[0].Coverage != UsageCoveragePartialLegacy || appended[1].ProviderMessageID != "msg-new" {
		t.Fatalf("upgrade append = %+v, want partial marker plus only post-cutover message", appended)
	}
	for _, snapshot := range appended {
		if snapshot.ProviderMessageID == "msg-legacy" {
			t.Fatal("legacy overlapping message was replayed")
		}
	}
	raw, err := os.ReadFile(filepath.Join(stateDir, usageCursorFilename))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), UsageCoveragePartialLegacy) || strings.Contains(string(raw), root) {
		t.Fatalf("cutover state lacks coverage or leaks path: %s", raw)
	}

	// A pre-cutover row completed after the prime remains suppressed, while a
	// genuinely post-cutover message is still collected.
	appendUsageLines(t, root,
		trackedUsageLine("2026-08-25T11:00:00Z", "msg-delayed-legacy", "claude-sonnet-5", 30, 6),
		trackedUsageLine("2026-08-25T14:00:00Z", "msg-later", "claude-sonnet-5", 40, 8),
	)
	restarted, err := NewUsageTracker(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	later, err := syncUsageSnapshots(restarted, "session-old", root, cutover.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(later) != 1 || later[0].ProviderMessageID != "msg-later" || later[0].Coverage != UsageCoveragePartialLegacy {
		t.Fatalf("post-cutover snapshots = %+v, want only msg-later with partial coverage", later)
	}
}

func TestUsageTrackerLegacyHistoryIsScopedBySession(t *testing.T) {
	stateDir := t.TempDir()
	legacy := `{"type":"usage_sample","agent":"claude","session_id":"other-session","tok_in":10}` + "\n"
	if err := os.WriteFile(filepath.Join(stateDir, "2026-08-25.jsonl"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "session.jsonl")
	appendUsageLines(t, root, trackedUsageLine("2026-08-25T10:00:00Z", "msg-1", "claude-sonnet-5", 10, 2))
	tracker, err := newUsageTracker(stateDir, time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := syncUsageSnapshots(tracker, "fresh-session", root, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 1 || snapshots[0].Cutover || snapshots[0].ProviderMessageID != "msg-1" {
		t.Fatalf("unrelated legacy session blocked fresh backfill: %+v", snapshots)
	}
}

func TestUsageTrackerTTLAndMetadataRevisionsReplaceSnapshot(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "session.jsonl")
	appendUsageLines(t, root, `{"type":"assistant","timestamp":"2026-08-25T10:00:00Z","uuid":"row-1","message":{"id":"msg-1","role":"assistant","model":"claude-opus-4-8","usage":{"input_tokens":10,"output_tokens":2,"cache_creation_input_tokens":100,"service_tier":"standard"}}}`)
	tracker, err := NewUsageTracker(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := syncUsageSnapshots(tracker, "session-1", root, time.Now())
	if err != nil || len(first) != 1 {
		t.Fatalf("first snapshot = %+v, %v", first, err)
	}

	appendUsageLines(t, root, `{"type":"assistant","timestamp":"2026-08-25T10:00:01Z","uuid":"row-2","message":{"id":"msg-1","role":"assistant","model":"claude-opus-4-8","usage":{"input_tokens":10,"output_tokens":2,"cache_creation_input_tokens":999,"cache_creation":{"ephemeral_5m_input_tokens":25,"ephemeral_1h_input_tokens":75},"service_tier":"standard"}}}`)
	second, err := syncUsageSnapshots(tracker, "session-1", root, time.Now())
	if err != nil || len(second) != 1 {
		t.Fatalf("TTL reclassification snapshot = %+v, %v", second, err)
	}
	u := second[0].Usage
	if second[0].UsageEventID != first[0].UsageEventID || second[0].UsageRevision <= first[0].UsageRevision || u.CacheCreationTokens != 100 || u.CacheWrite5mTokens != 25 || u.CacheWrite1hTokens != 75 {
		t.Fatalf("TTL revision = %+v, want full replacement 25/75 with combined 100", second[0])
	}

	appendUsageLines(t, root, `{"type":"assistant","timestamp":"2026-08-25T10:00:02Z","uuid":"row-3","message":{"id":"msg-1","role":"assistant","model":"claude-opus-4-8","usage":{"input_tokens":10,"output_tokens":2,"cache_creation":{"ephemeral_5m_input_tokens":25,"ephemeral_1h_input_tokens":75},"service_tier":"priority","speed":"fast","inference_geo":"us"}}}`)
	third, err := syncUsageSnapshots(tracker, "session-1", root, time.Now())
	if err != nil || len(third) != 1 || third[0].UsageRevision <= second[0].UsageRevision {
		t.Fatalf("metadata-only revision = %+v, %v", third, err)
	}
	if third[0].Usage.ServiceTier != "priority" || third[0].Usage.Speed != "fast" || third[0].Usage.InferenceGeo != "us" || third[0].Usage.CacheCreationTokens != 100 {
		t.Fatalf("metadata-only snapshot did not retain authoritative usage: %+v", third[0])
	}
}

func TestUsageTrackerSanitizesPathErrorsAndPersistsOpaqueSources(t *testing.T) {
	tracker, err := NewUsageTracker(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sensitiveRoot := filepath.Join(t.TempDir(), "customer-secret-project.jsonl")
	_, err = syncUsageSnapshots(tracker, "session-1", sensitiveRoot, time.Now())
	if err == nil {
		t.Fatal("missing transcript unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), "customer-secret") || strings.Contains(err.Error(), sensitiveRoot) || !strings.Contains(err.Error(), "cusrc_") {
		t.Fatalf("source error is not content-free: %v", err)
	}

	dir := t.TempDir()
	root := filepath.Join(dir, "session.jsonl")
	appendUsageLines(t, root, trackedUsageLine("2026-08-25T10:00:00Z", "msg-root", "claude-sonnet-5", 10, 2))
	childDir := filepath.Join(dir, "session", "subagents")
	if err := os.MkdirAll(childDir, 0o755); err != nil {
		t.Fatal(err)
	}
	appendUsageLines(t, filepath.Join(childDir, "agent-customer-secret-project.jsonl"), trackedUsageLine("2026-08-25T10:00:01Z", "msg-child", "claude-sonnet-5", 20, 4))
	stateDir := t.TempDir()
	tracker, err = NewUsageTracker(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := syncUsageSnapshots(tracker, "session-opaque", root, time.Now()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(stateDir, usageCursorFilename))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "customer-secret") || strings.Contains(string(raw), dir) || strings.Contains(string(raw), "agent-") {
		t.Fatalf("cursor state leaked a source path/name: %s", raw)
	}
}

func TestUsageTrackerDetectsSamePrefixInPlaceReplacement(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "session.jsonl")
	prefix := `{"type":"system","padding":"` + strings.Repeat("x", usageAnchorBytes+1024) + `"}` + "\n"
	oldLine := trackedUsageLine("2026-08-25T10:00:00Z", "msg-old", "claude-sonnet-5", 10, 2)
	newLine := trackedUsageLine("2026-08-25T10:00:00Z", "msg-new", "claude-sonnet-5", 20, 4)
	if len(oldLine) != len(newLine) {
		t.Fatal("synthetic replacement rows must have equal length")
	}
	if err := os.WriteFile(root, []byte(prefix+oldLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tracker, err := NewUsageTracker(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if snapshots, err := syncUsageSnapshots(tracker, "session-1", root, time.Now()); err != nil || len(snapshots) != 1 {
		t.Fatalf("initial snapshot = %+v, %v", snapshots, err)
	}
	// Preserve the first 4 KiB, inode, and total size. A generation hash of only
	// the prefix plus a size check would miss this replacement; the consumed-tail
	// anchor must force a rescan.
	if err := os.WriteFile(root, []byte(prefix+newLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshots, err := syncUsageSnapshots(tracker, "session-1", root, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 1 || snapshots[0].ProviderMessageID != "msg-new" {
		t.Fatalf("same-prefix replacement was missed: %+v", snapshots)
	}
}

func TestUsageTrackerEvictionReplayRetainsStableEventID(t *testing.T) {
	root := filepath.Join(t.TempDir(), "session.jsonl")
	appendUsageLines(t, root, trackedUsageLine("2026-08-25T10:00:00Z", "msg-1", "claude-sonnet-5", 10, 2))
	tracker, err := NewUsageTracker(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tracker.messageLimit = 1
	first, err := syncUsageSnapshots(tracker, "session-1", root, time.Now())
	if err != nil || len(first) != 1 {
		t.Fatalf("first snapshot = %+v, %v", first, err)
	}
	appendUsageLines(t, root, trackedUsageLine("2026-08-25T10:00:01Z", "msg-2", "claude-sonnet-5", 20, 4))
	if snapshots, err := syncUsageSnapshots(tracker, "session-1", root, time.Now()); err != nil || len(snapshots) != 1 {
		t.Fatalf("second snapshot = %+v, %v", snapshots, err)
	}
	if err := os.WriteFile(root, []byte(trackedUsageLine("2026-08-25T10:00:00Z", "msg-1", "claude-sonnet-5", 10, 2)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	replayed, err := syncUsageSnapshots(tracker, "session-1", root, time.Now())
	if err != nil || len(replayed) != 1 {
		t.Fatalf("eviction replay = %+v, %v", replayed, err)
	}
	if replayed[0].UsageEventID != first[0].UsageEventID || replayed[0].Usage != first[0].Usage || replayed[0].UsageRevision <= first[0].UsageRevision {
		t.Fatalf("evicted message changed id/value: first=%+v replay=%+v", first[0], replayed[0])
	}
}

func TestUsageTrackerV1MigrationScrubsLogicalSourceIDs(t *testing.T) {
	stateDir := t.TempDir()
	v1 := `{"version":1,"sessions":{"session-1":{"sources":{"workflows/customer-secret/agent-private.jsonl":{"offset":1,"generation":"abc"}},"messages":{"workflows/customer-secret|entry:row":{"source_id":"workflows/customer-secret/agent-private.jsonl","usage":{"input_tokens":10}}},"message_order":["workflows/customer-secret|entry:row"]}}}`
	if err := os.WriteFile(filepath.Join(stateDir, usageCursorFilename), []byte(v1), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "session.jsonl")
	appendUsageLines(t, root, trackedUsageLine("2026-08-25T10:00:00Z", "msg-1", "claude-sonnet-5", 10, 2))
	tracker, err := newUsageTracker(stateDir, time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := syncUsageSnapshots(tracker, "session-1", root, time.Now())
	if err != nil || len(snapshots) != 1 || !snapshots[0].Cutover {
		t.Fatalf("v1 migration = %+v, %v, want cutover marker only", snapshots, err)
	}
	raw, err := os.ReadFile(filepath.Join(stateDir, usageCursorFilename))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "customer-secret") || strings.Contains(string(raw), "agent-private") {
		t.Fatalf("v1 logical source ids survived migration: %s", raw)
	}
}

func TestUsageTrackerRequiresDurableAppenderBeforeCursorAdvance(t *testing.T) {
	root := filepath.Join(t.TempDir(), "session.jsonl")
	appendUsageLines(t, root, trackedUsageLine("2026-08-25T10:00:00Z", "msg-1", "claude-sonnet-5", 10, 2))
	stateDir := t.TempDir()
	tracker, err := NewUsageTracker(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tracker.SyncSession("session-1", root, time.Now(), nil); err == nil {
		t.Fatal("nil durable appender unexpectedly advanced usage")
	}
	if _, err := os.Stat(filepath.Join(stateDir, usageCursorFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cursor exists after nil durable appender: %v", err)
	}
}
