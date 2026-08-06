package history

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeDay(t *testing.T, dir, day string, lines ...string) {
	t.Helper()
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, day+".jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadDayToleratesTornLine(t *testing.T) {
	dir := t.TempDir()
	writeDay(t, dir, "2026-06-26",
		`{"ts":"2026-06-26T12:00:00Z","type":"transition","to":"working"}`,
		`{"ts":"2026-06-26T12:01:00Z","type":"transition","to":"idle"}`,
		`{"ts":"2026-06-26T12:02:00Z","type":"transi`, // torn final line (crash mid-append)
	)
	evs, err := ReadDay(dir, "2026-06-26")
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2 (torn line skipped)", len(evs))
	}
	if evs[1].To != "idle" {
		t.Errorf("event 1 = %+v, want to=idle", evs[1])
	}
}

// TestReadDaySkipsForeignJSONLine asserts a foreign-but-valid JSON line (e.g.
// {"foo":"bar"}) is dropped, not surfaced as a phantom zero-value Event. Unlike a
// torn line it parses cleanly, but `type` is guaranteed-present for a real event,
// so a type-less line is treated as foreign and skipped — matching the tolerance
// the transcript reader already applies to stray lines.
func TestReadDaySkipsForeignJSONLine(t *testing.T) {
	dir := t.TempDir()
	writeDay(t, dir, "2026-06-26",
		`{"ts":"2026-06-26T12:00:00Z","type":"transition","to":"working"}`,
		`{"foo":"bar"}`, // foreign-but-valid JSON — NOT a torn line
		`{"ts":"2026-06-26T12:02:00Z","type":"transition","to":"idle"}`,
	)
	evs, err := ReadDay(dir, "2026-06-26")
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2 (foreign line skipped): %+v", len(evs), evs)
	}
	if evs[0].To != "working" || evs[1].To != "idle" {
		t.Errorf("real events should pass through in order, got %+v", evs)
	}
}

func TestReadDayMissingFileIsEmpty(t *testing.T) {
	evs, err := ReadDay(t.TempDir(), "2026-01-01")
	if err != nil {
		t.Fatalf("missing day should not error: %v", err)
	}
	if len(evs) != 0 {
		t.Errorf("missing day should be empty, got %d", len(evs))
	}
}

func TestReadRangeSpansDaysAndFilters(t *testing.T) {
	dir := t.TempDir()
	writeDay(t, dir, "2026-06-25", `{"ts":"2026-06-25T23:30:00Z","type":"transition","to":"a"}`)
	writeDay(t, dir, "2026-06-26",
		`{"ts":"2026-06-26T08:00:00Z","type":"transition","to":"b"}`,
		`{"ts":"2026-06-26T20:00:00Z","type":"transition","to":"c"}`,
	)
	writeDay(t, dir, "2026-06-27", `{"ts":"2026-06-27T09:00:00Z","type":"transition","to":"d"}`)

	from := time.Date(2026, 6, 26, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 26, 18, 0, 0, 0, time.UTC)
	evs, err := ReadRange(dir, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].To != "b" {
		t.Fatalf("range [06-26 00:00, 18:00) = %+v, want only event b", evs)
	}
}

func TestPriorSubagentStateByAgentIDExcludesOtherSessions(t *testing.T) {
	dir := t.TempDir()
	writeDay(t, dir, "2026-06-25",
		`{"ts":"2026-06-25T10:00:00Z","type":"subagent_spawn","session_id":"s1","agent_id":"a1"}`,
		`{"ts":"2026-06-25T10:05:00Z","type":"subagent_stop","session_id":"s1","agent_id":"a1"}`,
	)
	writeDay(t, dir, "2026-06-26",
		`{"ts":"2026-06-26T10:00:00Z","type":"subagent_spawn","session_id":"s1","agent_id":"a2"}`,    // spawned, not yet stopped
		`{"ts":"2026-06-26T10:01:00Z","type":"subagent_spawn","session_id":"s1","tool_use_id":"t3"}`, // older event → keyed by tool_use_id
		`{"ts":"2026-06-26T10:02:00Z","type":"subagent_spawn","session_id":"s2","agent_id":"b1"}`,    // a DIFFERENT session — must be excluded
	)

	spawned, stopped, err := PriorSubagentState(dir, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(spawned) != 3 || !spawned["a1"] || !spawned["a2"] || !spawned["t3"] {
		t.Errorf("spawned = %v, want exactly {a1,a2,t3} (agent_id, plus tool_use_id fallback)", spawned)
	}
	if spawned["b1"] {
		t.Errorf("spawned must exclude the other session's b1: %v", spawned)
	}
	if len(stopped) != 1 || !stopped["a1"] {
		t.Errorf("stopped = %v, want exactly {a1}", stopped)
	}
}

// The tests below pin the behavior the byte pre-filter in scanCandidateLines
// must not change. Both mutations below were actually run.
//
// Only ONE of the filter's two parts is load-bearing for correctness: the
// post-decode `ev.SessionID != sessionID` check. Delete it and
// ...IgnoresTheIDAppearingInAnotherField fails, because a substring hit is not
// a parse and the byte scan cannot tell which field it matched.
//
// Quoting the needle is a PERFORMANCE guard, not a correctness one — it stops
// `s1` from matching inside `"session_id":"s10"` and wasting a decode, but the
// SessionID check would reject that line anyway. Replacing the needle with the
// bare id leaves every test here green, which is the honest result: do not read
// ...PrefixOfAnother as protecting the quoting. It is a regression test for the
// s1/s10 hazard itself, which is worth keeping whichever guard catches it.

func TestPriorSubagentStateExcludesASessionWhoseIDIsAPrefixOfAnother(t *testing.T) {
	dir := t.TempDir()
	writeDay(t, dir, "2026-06-26",
		`{"ts":"2026-06-26T10:00:00Z","type":"subagent_spawn","session_id":"s1","agent_id":"mine"}`,
		`{"ts":"2026-06-26T10:01:00Z","type":"subagent_spawn","session_id":"s10","agent_id":"theirs"}`,
		`{"ts":"2026-06-26T10:02:00Z","type":"subagent_stop","session_id":"s10","agent_id":"theirs"}`,
	)

	spawned, stopped, err := PriorSubagentState(dir, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(spawned) != 1 || !spawned["mine"] {
		t.Errorf("spawned = %v, want exactly {mine}: s10 is a different session that merely starts with s1", spawned)
	}
	if len(stopped) != 0 {
		t.Errorf("stopped = %v, want empty: the only stop belongs to s10", stopped)
	}
}

func TestPriorSubagentStateIgnoresTheIDAppearingInAnotherField(t *testing.T) {
	dir := t.TempDir()
	// A DIFFERENT session's spawn, carrying our session id as the value of some
	// other field — here a cwd, which is contrived on purpose. The point is not
	// that this happens often; it is that the byte scan cannot tell WHICH field it
	// matched, so this line passes both needles and IS decoded. Only the SessionID
	// check after the decode can reject it.
	writeDay(t, dir, "2026-06-26",
		`{"ts":"2026-06-26T10:00:00Z","type":"subagent_spawn","session_id":"other","agent_id":"theirs","cwd":"s1"}`,
		`{"ts":"2026-06-26T10:01:00Z","type":"subagent_spawn","session_id":"s1","agent_id":"mine"}`,
	)

	spawned, _, err := PriorSubagentState(dir, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(spawned) != 1 || !spawned["mine"] {
		t.Errorf("spawned = %v, want exactly {mine}: a substring hit in another field is not a session match", spawned)
	}
}

func TestPriorSubagentStateSkipsNonSubagentEventsForTheSameSession(t *testing.T) {
	dir := t.TempDir()
	writeDay(t, dir, "2026-06-26",
		`{"ts":"2026-06-26T10:00:00Z","type":"transition","session_id":"s1","from":"idle","to":"working"}`,
		`{"ts":"2026-06-26T10:01:00Z","type":"usage_sample","session_id":"s1","agent_id":"notaspawn"}`,
		`{"ts":"2026-06-26T10:02:00Z","type":"subagent_spawn","session_id":"s1","agent_id":"a1"}`,
	)

	spawned, stopped, err := PriorSubagentState(dir, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(spawned) != 1 || !spawned["a1"] {
		t.Errorf("spawned = %v, want exactly {a1}: only subagent_spawn seeds the set", spawned)
	}
	if len(stopped) != 0 {
		t.Errorf("stopped = %v, want empty", stopped)
	}
}

func TestPriorSubagentStateToleratesATornLine(t *testing.T) {
	dir := t.TempDir()
	writeDay(t, dir, "2026-06-26",
		`{"ts":"2026-06-26T10:00:00Z","type":"subagent_spawn","session_id":"s1","agent_id":"a1"}`,
		`{"ts":"2026-06-26T10:01:00Z","type":"subagent_spawn","session_id":"s1","agent_i`, // torn mid-append
	)

	spawned, _, err := PriorSubagentState(dir, "s1")
	if err != nil {
		t.Fatalf("a torn final line must not be an error: %v", err)
	}
	if len(spawned) != 1 || !spawned["a1"] {
		t.Errorf("spawned = %v, want exactly {a1} with the torn line skipped", spawned)
	}
}

func TestPriorSubagentStateEmptySessionIsEmpty(t *testing.T) {
	dir := t.TempDir()
	writeDay(t, dir, "2026-06-26",
		`{"ts":"2026-06-26T10:00:00Z","type":"subagent_spawn","session_id":"s1","agent_id":"a1"}`,
	)
	spawned, stopped, err := PriorSubagentState(dir, "")
	if err != nil {
		t.Fatalf("empty session id should not error: %v", err)
	}
	if len(spawned) != 0 || len(stopped) != 0 {
		t.Errorf("empty session id should yield empty sets, got spawned=%v stopped=%v", spawned, stopped)
	}
}

// PriorWorkflowState carries the same byte pre-filter as PriorSubagentState, and
// the tests below are its half of the contract that pre-filter must not change.
// They were written against the unfiltered (ReadRange) implementation and pass
// against both, which is the claim being made: the filter is a cost change, not a
// behavior change.
//
// One asymmetry is worth stating, because it is the reason the type needle here
// is weaker than `subagent_`: `workflow_run_id` is a FIELD NAME, so every event a
// workflow's agents emit contains the `workflow_` needle. ...SkipsNonWorkflowEvents
// is the test that pins it — a subagent_spawn carrying a run id passes both byte
// scans and is rejected only by the switch on ev.Type.

func TestPriorWorkflowStateByRunIDExcludesOtherSessions(t *testing.T) {
	dir := t.TempDir()
	writeDay(t, dir, "2026-06-25",
		`{"ts":"2026-06-25T10:00:00Z","type":"workflow_start","session_id":"s1","workflow_run_id":"r1"}`,
		`{"ts":"2026-06-25T10:05:00Z","type":"workflow_stop","session_id":"s1","workflow_run_id":"r1"}`,
	)
	writeDay(t, dir, "2026-06-26",
		`{"ts":"2026-06-26T10:00:00Z","type":"workflow_start","session_id":"s1","workflow_run_id":"r2"}`, // started, still running
		`{"ts":"2026-06-26T10:02:00Z","type":"workflow_start","session_id":"s2","workflow_run_id":"r9"}`, // a DIFFERENT session — must be excluded
	)

	started, stopped, err := PriorWorkflowState(dir, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(started) != 2 || !started["r1"] || !started["r2"] {
		t.Errorf("started = %v, want exactly {r1,r2}", started)
	}
	if started["r9"] {
		t.Errorf("started must exclude the other session's r9: %v", started)
	}
	if len(stopped) != 1 || !stopped["r1"] {
		t.Errorf("stopped = %v, want exactly {r1}", stopped)
	}
}

func TestPriorWorkflowStateExcludesASessionWhoseIDIsAPrefixOfAnother(t *testing.T) {
	dir := t.TempDir()
	writeDay(t, dir, "2026-06-26",
		`{"ts":"2026-06-26T10:00:00Z","type":"workflow_start","session_id":"s1","workflow_run_id":"mine"}`,
		`{"ts":"2026-06-26T10:01:00Z","type":"workflow_start","session_id":"s10","workflow_run_id":"theirs"}`,
		`{"ts":"2026-06-26T10:02:00Z","type":"workflow_stop","session_id":"s10","workflow_run_id":"theirs"}`,
	)

	started, stopped, err := PriorWorkflowState(dir, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(started) != 1 || !started["mine"] {
		t.Errorf("started = %v, want exactly {mine}: s10 is a different session that merely starts with s1", started)
	}
	if len(stopped) != 0 {
		t.Errorf("stopped = %v, want empty: the only stop belongs to s10", stopped)
	}
}

func TestPriorWorkflowStateIgnoresTheIDAppearingInAnotherField(t *testing.T) {
	dir := t.TempDir()
	// A different session's start, carrying our session id as the value of some
	// other field. It passes both byte scans; only the post-decode SessionID check
	// can reject it.
	writeDay(t, dir, "2026-06-26",
		`{"ts":"2026-06-26T10:00:00Z","type":"workflow_start","session_id":"other","workflow_run_id":"theirs","cwd":"s1"}`,
		`{"ts":"2026-06-26T10:01:00Z","type":"workflow_start","session_id":"s1","workflow_run_id":"mine"}`,
	)

	started, _, err := PriorWorkflowState(dir, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(started) != 1 || !started["mine"] {
		t.Errorf("started = %v, want exactly {mine}: a substring hit in another field is not a session match", started)
	}
}

func TestPriorWorkflowStateSkipsNonWorkflowEventsForTheSameSession(t *testing.T) {
	dir := t.TempDir()
	writeDay(t, dir, "2026-06-26",
		`{"ts":"2026-06-26T10:00:00Z","type":"transition","session_id":"s1","from":"idle","to":"working"}`,
		// A workflow agent's own spawn: same session, and it CARRIES a run id, so it
		// passes both needles. Only the type switch keeps it out of the started set.
		`{"ts":"2026-06-26T10:01:00Z","type":"subagent_spawn","session_id":"s1","agent_id":"a1","workflow_run_id":"r1"}`,
		`{"ts":"2026-06-26T10:02:00Z","type":"workflow_start","session_id":"s1","workflow_run_id":"r1"}`,
	)

	started, stopped, err := PriorWorkflowState(dir, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(started) != 1 || !started["r1"] {
		t.Errorf("started = %v, want exactly {r1}: only workflow_start seeds the set", started)
	}
	if len(stopped) != 0 {
		t.Errorf("stopped = %v, want empty", stopped)
	}
}

func TestPriorWorkflowStateIgnoresAnEventWithoutARunID(t *testing.T) {
	dir := t.TempDir()
	writeDay(t, dir, "2026-06-26",
		`{"ts":"2026-06-26T10:00:00Z","type":"workflow_start","session_id":"s1"}`,
		`{"ts":"2026-06-26T10:01:00Z","type":"workflow_start","session_id":"s1","workflow_run_id":"r1"}`,
	)

	started, _, err := PriorWorkflowState(dir, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(started) != 1 || !started["r1"] {
		t.Errorf("started = %v, want exactly {r1}: a start with no run id names nothing", started)
	}
}

func TestPriorWorkflowStateToleratesATornLine(t *testing.T) {
	dir := t.TempDir()
	writeDay(t, dir, "2026-06-26",
		`{"ts":"2026-06-26T10:00:00Z","type":"workflow_start","session_id":"s1","workflow_run_id":"r1"}`,
		`{"ts":"2026-06-26T10:01:00Z","type":"workflow_start","session_id":"s1","workflow_run_`, // torn mid-append
	)

	started, _, err := PriorWorkflowState(dir, "s1")
	if err != nil {
		t.Fatalf("a torn final line must not be an error: %v", err)
	}
	if len(started) != 1 || !started["r1"] {
		t.Errorf("started = %v, want exactly {r1} with the torn line skipped", started)
	}
}

func TestPriorWorkflowStateEmptySessionIsEmpty(t *testing.T) {
	dir := t.TempDir()
	writeDay(t, dir, "2026-06-26",
		`{"ts":"2026-06-26T10:00:00Z","type":"workflow_start","session_id":"s1","workflow_run_id":"r1"}`,
	)
	started, stopped, err := PriorWorkflowState(dir, "")
	if err != nil {
		t.Fatalf("empty session id should not error: %v", err)
	}
	if len(started) != 0 || len(stopped) != 0 {
		t.Errorf("empty session id should yield empty sets, got started=%v stopped=%v", started, stopped)
	}
}

func TestDaysSortedOldestFirst(t *testing.T) {
	dir := t.TempDir()
	writeDay(t, dir, "2026-06-27", "{}")
	writeDay(t, dir, "2026-06-25", "{}")
	writeDay(t, dir, "2026-06-26", "{}")
	days, err := Days(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"2026-06-25", "2026-06-26", "2026-06-27"}
	if len(days) != 3 || days[0] != want[0] || days[2] != want[2] {
		t.Errorf("Days = %v, want %v", days, want)
	}
}
