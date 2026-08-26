package history

import (
	"testing"
)

// The semantics PriorSubagentState pinned before SeedScan replaced it: keys
// come from agent_id with a tool_use_id fallback, sessions are kept apart,
// and days are folded across file boundaries.
func TestSeedScanKeysByAgentIDWithToolUseFallback(t *testing.T) {
	dir := t.TempDir()
	writeDay(t, dir, "2026-06-25",
		`{"ts":"2026-06-25T10:00:00Z","type":"subagent_spawn","session_id":"s1","agent_id":"a1"}`,
		`{"ts":"2026-06-25T10:05:00Z","type":"subagent_stop","session_id":"s1","agent_id":"a1"}`,
	)
	writeDay(t, dir, "2026-06-26",
		`{"ts":"2026-06-26T10:00:00Z","type":"subagent_spawn","session_id":"s1","agent_id":"a2"}`,    // spawned, not yet stopped
		`{"ts":"2026-06-26T10:01:00Z","type":"subagent_spawn","session_id":"s1","tool_use_id":"t3"}`, // older event → keyed by tool_use_id
		`{"ts":"2026-06-26T10:02:00Z","type":"subagent_spawn","session_id":"s2","agent_id":"b1"}`,    // a DIFFERENT session
	)

	index, stats, err := SeedScan(dir)
	if err != nil {
		t.Fatal(err)
	}
	s1 := index.Sets("s1")
	if len(s1.Spawned) != 3 || !s1.Spawned["a1"] || !s1.Spawned["a2"] || !s1.Spawned["t3"] {
		t.Errorf("s1 spawned = %v, want exactly {a1,a2,t3} (agent_id, plus tool_use_id fallback)", s1.Spawned)
	}
	if s1.Spawned["b1"] {
		t.Errorf("s1 spawned must exclude the other session's b1: %v", s1.Spawned)
	}
	if len(s1.Stopped) != 1 || !s1.Stopped["a1"] {
		t.Errorf("s1 stopped = %v, want exactly {a1}", s1.Stopped)
	}
	s2 := index.Sets("s2")
	if len(s2.Spawned) != 1 || !s2.Spawned["b1"] {
		t.Errorf("s2 spawned = %v, want exactly {b1} — the shared pass seeds every session at once", s2.Spawned)
	}
	if stats.Files != 2 || stats.Lines != 5 || stats.Matched != 5 {
		t.Errorf("stats = %+v, want files=2 lines=5 matched=5", stats)
	}
}

func TestSeedScanUnknownSessionYieldsEmptySets(t *testing.T) {
	dir := t.TempDir()
	writeDay(t, dir, "2026-06-26",
		`{"ts":"2026-06-26T10:00:00Z","type":"subagent_spawn","session_id":"s1","agent_id":"a1"}`,
	)
	index, _, err := SeedScan(dir)
	if err != nil {
		t.Fatal(err)
	}
	sets := index.Sets("never-seen")
	if len(sets.Spawned) != 0 || len(sets.Stopped) != 0 || sets.Spawned == nil {
		t.Errorf("unknown session must get fresh empty sets, got %+v", sets)
	}
}

func TestSeedScanFoldsWorkflowRuns(t *testing.T) {
	dir := t.TempDir()
	writeDay(t, dir, "2026-06-26",
		`{"ts":"2026-06-26T10:00:00Z","type":"workflow_start","session_id":"s1","workflow_run_id":"wf_a"}`,
		`{"ts":"2026-06-26T10:10:00Z","type":"workflow_stop","session_id":"s1","workflow_run_id":"wf_a"}`,
		`{"ts":"2026-06-26T10:20:00Z","type":"workflow_start","session_id":"s1","workflow_run_id":"wf_b"}`, // still running
		`{"ts":"2026-06-26T10:30:00Z","type":"workflow_start","session_id":"s1"}`,                          // no run id — contributes nothing
	)
	index, _, err := SeedScan(dir)
	if err != nil {
		t.Fatal(err)
	}
	s1 := index.Sets("s1")
	if len(s1.WorkflowStarted) != 2 || !s1.WorkflowStarted["wf_a"] || !s1.WorkflowStarted["wf_b"] {
		t.Errorf("started = %v, want {wf_a, wf_b}", s1.WorkflowStarted)
	}
	if len(s1.WorkflowStopped) != 1 || !s1.WorkflowStopped["wf_a"] {
		t.Errorf("stopped = %v, want {wf_a}", s1.WorkflowStopped)
	}
}

// The Aug-14 repair scripts rewrote span lines with Python json.dumps spacing
// (`"type": "subagent_stop"`), and future repairs may again. A prefilter keyed
// on the compact `"type":"…"` form would skip exactly those lines, and a
// missed spawn here re-emits as a duplicate span on the next restart — so the
// markers match the type VALUE and this test holds them to it.
func TestSeedScanToleratesPythonSpacedRepairLines(t *testing.T) {
	dir := t.TempDir()
	writeDay(t, dir, "2026-07-22",
		`{"ts": "2026-07-22T03:21:39Z", "type": "subagent_spawn", "session_id": "s1", "agent_id": "a1"}`,
		`{"ts": "2026-07-22T03:25:00Z", "type": "subagent_stop", "session_id": "s1", "agent_id": "a1"}`,
	)
	index, _, err := SeedScan(dir)
	if err != nil {
		t.Fatal(err)
	}
	s1 := index.Sets("s1")
	if !s1.Spawned["a1"] || !s1.Stopped["a1"] {
		t.Errorf("spaced repair lines were not folded: spawned=%v stopped=%v", s1.Spawned, s1.Stopped)
	}
}

// A line whose CONTENT quotes an event type is not that event: the markers
// only admit a line to the decode, the decoded type decides.
func TestSeedScanIgnoresTypeMentionsInContentAndTornLines(t *testing.T) {
	dir := t.TempDir()
	writeDay(t, dir, "2026-06-26",
		`{"ts":"2026-06-26T10:00:00Z","type":"transition","session_id":"s1","reason":"replayed a \"subagent_spawn\" event"}`,
		`{"ts":"2026-06-26T10:01:00Z","type":"subagent_spawn","session_id":"s1","agent_id":"a1"}`,
		`{"ts":"2026-06-26T10:02:00Z","type":"subagent_spawn","session_id":"s1","agent_`, // torn final line
	)
	index, _, err := SeedScan(dir)
	if err != nil {
		t.Fatal(err)
	}
	s1 := index.Sets("s1")
	if len(s1.Spawned) != 1 || !s1.Spawned["a1"] {
		t.Errorf("spawned = %v, want exactly {a1} — content mentions and torn lines contribute nothing", s1.Spawned)
	}
}

func TestSeedScanEmptyDirIsEmpty(t *testing.T) {
	index, stats, err := SeedScan(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(index) != 0 || stats.Files != 0 {
		t.Errorf("empty dir: index=%v stats=%+v, want nothing", index, stats)
	}
}
