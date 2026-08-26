package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The Aug-14 repair scripts rewrote span lines with Python json.dumps spacing
// (`"type": "subagent_stop"`), so any prefilter keyed on the compact
// `"type":"…"` form silently skips exactly the repaired lines. The discovery
// scan must find both spellings.
func TestSeedBenchSessionsToleratesSpacedRepairLines(t *testing.T) {
	dir := t.TempDir()
	lines := `{"ts":"2026-01-01T10:00:00Z","type":"subagent_spawn","session_id":"s1","agent_id":"a1"}
{"ts": "2026-01-01T10:01:00Z", "type": "subagent_stop", "session_id": "s1", "agent_id": "a1"}
{"ts":"2026-01-01T10:02:00Z","type":"transition","session_id":"s1","from":"idle","to":"working"}
{"ts":"2026-01-01T10:03:00Z","type":"workflow_start","session_id":"s2","workflow_run_id":"wf_1"}
`
	if err := os.WriteFile(filepath.Join(dir, "2026-01-01.jsonl"), []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}

	ids, counts, err := seedBenchSessions(dir)
	if err != nil {
		t.Fatalf("seedBenchSessions: %v", err)
	}
	if counts["s1"] != 2 {
		t.Errorf("s1 count = %d, want 2 (the spaced repair line must be counted)", counts["s1"])
	}
	if counts["s2"] != 1 {
		t.Errorf("s2 count = %d, want 1", counts["s2"])
	}
	if len(ids) != 2 || ids[0] != "s1" {
		t.Errorf("ids = %v, want [s1 s2] (busiest first)", ids)
	}
}

// A line whose CONTENT mentions an event type must not be miscounted: the
// substring markers only admit lines to the decode, the decoded type decides.
func TestSeedBenchSessionsIgnoresTypeMentionsInContent(t *testing.T) {
	dir := t.TempDir()
	lines := `{"ts":"2026-01-01T10:00:00Z","type":"transition","session_id":"s1","reason":"saw a \"subagent_spawn\" in the log"}
`
	if err := os.WriteFile(filepath.Join(dir, "2026-01-01.jsonl"), []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	ids, counts, err := seedBenchSessions(dir)
	if err != nil {
		t.Fatalf("seedBenchSessions: %v", err)
	}
	if len(ids) != 0 || len(counts) != 0 {
		t.Errorf("ids=%v counts=%v, want none — content mentions are not events", ids, counts)
	}
}
