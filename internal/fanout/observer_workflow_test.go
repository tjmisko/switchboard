package fanout

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/transcript"
)

// writeWorkflowRun lays out one workflow run the way Claude Code writes it:
// <base>/<sid>/subagents/workflows/<runID>/ plus, when scriptName is set, the
// persisted script <base>/<sid>/workflows/scripts/<scriptName>-<runID>.js that
// carries the workflow's name. Returns the run dir.
func writeWorkflowRun(t *testing.T, e env, runID, scriptName string) string {
	t.Helper()
	runDir := filepath.Join(e.subdir, "workflows", runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if scriptName != "" {
		scriptsDir := filepath.Join(e.base, e.sid, "workflows", "scripts")
		if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		script := filepath.Join(scriptsDir, scriptName+"-"+runID+".js")
		if err := os.WriteFile(script, []byte("export const meta = {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return runDir
}

// writeJournal (over)writes the run's journal.jsonl with the given lines and
// returns its path.
func writeJournal(t *testing.T, runDir string, lines []string) string {
	t.Helper()
	p := filepath.Join(runDir, "journal.jsonl")
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// writeWorkflowAgent writes one workflow agent's jsonl + the constant meta the
// engine records ({"agentType":"workflow-subagent","spawnDepth":1}). The jsonl
// content is arbitrary — for workflow agents only its MTIME (the activity
// heartbeat) matters, never its last line.
func writeWorkflowAgent(t *testing.T, runDir, id string) string {
	t.Helper()
	meta := filepath.Join(runDir, "agent-"+id+".meta.json")
	if err := os.WriteFile(meta, []byte(`{"agentType":"workflow-subagent","spawnDepth":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(runDir, "agent-"+id+".jsonl")
	line := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_x"}]}}`
	if err := os.WriteFile(p, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func wfStarted(id string) string {
	return `{"type":"started","key":"v2:k","agentId":"` + id + `"}`
}

func wfResult(id string) string {
	return `{"type":"result","key":"v2:k","agentId":"` + id + `","result":{"ok":true}}`
}

func hasWorkflowEvent(evs []history.Event, typ, runID string) bool {
	for _, e := range evs {
		if e.Type == typ && e.WorkflowRunID == runID {
			return true
		}
	}
	return false
}

func TestReconcile_shouldCountWorkflowAgentsAndAnnounceRunWhenJournalShowsWork(t *testing.T) {
	e := newEnv(t)
	runDir := writeWorkflowRun(t, e, "wf_abc-123", "simplification-audit")
	writeJournal(t, runDir, []string{wfStarted("wa1"), wfStarted("wa2"), wfResult("wa1")})
	writeWorkflowAgent(t, runDir, "wa1")
	writeWorkflowAgent(t, runDir, "wa2")
	obs := NewObserver(e.historyDir)
	now := time.Now()

	ev := obs.Reconcile(e.sess, e.c, now)
	if !hasWorkflowEvent(ev, history.EventWorkflowStart, "wf_abc-123") {
		t.Fatalf("first sight of a live run should emit workflow_start; got %+v", ev)
	}
	if !hasEvent(ev, history.EventSubagentSpawn, "wa1") || !hasEvent(ev, history.EventSubagentSpawn, "wa2") {
		t.Fatalf("journal-started agents should spawn; got %+v", ev)
	}
	if !hasEvent(ev, history.EventSubagentStop, "wa1") {
		t.Fatalf("wa1 resulted in the journal; expected its stop; got %+v", ev)
	}
	if hasEvent(ev, history.EventSubagentStop, "wa2") {
		t.Fatalf("wa2 has no result; must not stop")
	}
	for _, x := range ev {
		if x.Type == history.EventSubagentSpawn && x.AgentID == "wa1" {
			if x.WorkflowRunID != "wf_abc-123" || x.AgentType != transcript.WorkflowAgentType {
				t.Fatalf("workflow agent spawn should carry run id + workflow-subagent type; got %+v", x)
			}
		}
		if x.Type == history.EventWorkflowStart && x.Label != "simplification-audit" {
			t.Fatalf("workflow_start should carry the script's name in Label; got %+v", x)
		}
	}
	if e.c.InFlightSubagents != 1 {
		t.Fatalf("inflight = %d, want 1 (wa2 running)", e.c.InFlightSubagents)
	}
	if len(e.c.Workflows) != 1 {
		t.Fatalf("Workflows = %+v, want one active run", e.c.Workflows)
	}
	w := e.c.Workflows[0]
	if w.RunID != "wf_abc-123" || w.Name != "simplification-audit" ||
		w.AgentsStarted != 2 || w.AgentsDone != 1 || w.InFlight != 1 {
		t.Fatalf("WorkflowStatus = %+v, want wf_abc-123/simplification-audit 2 started 1 done 1 in flight", w)
	}

	// Idempotent: an unchanged second pass emits nothing and holds the summary.
	if ev := obs.Reconcile(e.sess, e.c, now); len(ev) != 0 {
		t.Fatalf("second pass should emit nothing; got %+v", ev)
	}
	if e.c.InFlightSubagents != 1 || len(e.c.Workflows) != 1 {
		t.Fatalf("counts drifted: inflight=%d workflows=%+v", e.c.InFlightSubagents, e.c.Workflows)
	}
}

func TestReconcile_shouldCombineWorkflowAgentsWithFlatFanoutsInOneCount(t *testing.T) {
	e := newEnv(t)
	writeSub(t, e.subdir, "flat1", metaClassic(1, "toolu_f1"), "") // classic running fanout
	runDir := writeWorkflowRun(t, e, "wf_mix-1", "")
	writeJournal(t, runDir, []string{wfStarted("wm1")})
	writeWorkflowAgent(t, runDir, "wm1")
	obs := NewObserver(e.historyDir)

	obs.Reconcile(e.sess, e.c, time.Now())
	if e.c.InFlightSubagents != 2 {
		t.Fatalf("inflight = %d, want 2 (one flat + one workflow agent)", e.c.InFlightSubagents)
	}
}

func TestReconcile_shouldBridgeBetweenWavesWhileJournalIsFresh(t *testing.T) {
	e := newEnv(t)
	runDir := writeWorkflowRun(t, e, "wf_wave-1", "")
	writeJournal(t, runDir, []string{wfStarted("wv1"), wfResult("wv1")})
	writeWorkflowAgent(t, runDir, "wv1")
	obs := NewObserver(e.historyDir)

	ev := obs.Reconcile(e.sess, e.c, time.Now())
	if hasWorkflowEvent(ev, history.EventWorkflowStop, "wf_wave-1") {
		t.Fatalf("a fresh journal with everything resulted is between waves, not done; got %+v", ev)
	}
	if len(e.c.Workflows) != 1 || e.c.Workflows[0].InFlight != 0 {
		t.Fatalf("Workflows = %+v, want the run still listed with 0 in flight", e.c.Workflows)
	}
}

func TestReconcile_shouldStopRunWhenDrainedAndJournalGoesQuiet(t *testing.T) {
	e := newEnv(t)
	runDir := writeWorkflowRun(t, e, "wf_done-1", "review-changes")
	journal := writeJournal(t, runDir, []string{wfStarted("wd1"), wfResult("wd1")})
	writeWorkflowAgent(t, runDir, "wd1")
	obs := NewObserver(e.historyDir)
	now := time.Now()

	// While fresh: announced and active.
	ev := obs.Reconcile(e.sess, e.c, now)
	if !hasWorkflowEvent(ev, history.EventWorkflowStart, "wf_done-1") {
		t.Fatalf("expected workflow_start; got %+v", ev)
	}

	// Journal quiet past the grace with nothing in flight: the run is over.
	old := now.Add(-time.Hour)
	if err := os.Chtimes(journal, old, old); err != nil {
		t.Fatal(err)
	}
	ev = obs.Reconcile(e.sess, e.c, now)
	if !hasWorkflowEvent(ev, history.EventWorkflowStop, "wf_done-1") {
		t.Fatalf("a drained, quiet run should emit workflow_stop; got %+v", ev)
	}
	if e.c.Workflows != nil {
		t.Fatalf("Workflows = %+v, want nil after the run drains", e.c.Workflows)
	}
	if e.c.InFlightSubagents != 0 {
		t.Fatalf("inflight = %d, want 0", e.c.InFlightSubagents)
	}

	// And exactly once.
	if ev := obs.Reconcile(e.sess, e.c, now); countType(ev, history.EventWorkflowStop) != 0 {
		t.Fatalf("workflow_stop must not repeat; got %+v", ev)
	}
}

func TestReconcile_shouldForceCloseKilledRunsOrphansAfterStaleCap(t *testing.T) {
	e := newEnv(t)
	runDir := writeWorkflowRun(t, e, "wf_kill-1", "")
	// Killed mid-run: started with no result, every record long quiet. Journals
	// record no terminal event, so only staleness can drain this.
	journal := writeJournal(t, runDir, []string{wfStarted("wk1")})
	agent := writeWorkflowAgent(t, runDir, "wk1")
	old := time.Now().Add(-time.Hour)
	for _, p := range []string{journal, agent} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	obs := NewObserver(e.historyDir)
	obs.SetStaleCap(30 * time.Minute)

	ev := obs.Reconcile(e.sess, e.c, time.Now())
	if !hasEvent(ev, history.EventSubagentStop, "wk1") {
		t.Fatalf("a stale orphan must be force-closed; got %+v", ev)
	}
	if e.c.InFlightSubagents != 0 {
		t.Fatalf("inflight = %d, want 0 — a killed run must not pin the count", e.c.InFlightSubagents)
	}
	if e.c.Workflows != nil {
		t.Fatalf("Workflows = %+v, want nil for a dead run", e.c.Workflows)
	}
}

func TestReconcile_shouldNotReannounceWorkflowAfterDaemonRestart(t *testing.T) {
	e := newEnv(t)
	runDir := writeWorkflowRun(t, e, "wf_seed-1", "big-audit")
	writeJournal(t, runDir, []string{wfStarted("ws1")})
	writeWorkflowAgent(t, runDir, "ws1")
	// The prior daemon already announced the run and spawned ws1.
	day := filepath.Join(e.historyDir, "2026-08-05.jsonl")
	lines := `{"ts":"2026-08-05T12:00:00Z","type":"workflow_start","session_id":"sess-abc123","workflow_run_id":"wf_seed-1"}
{"ts":"2026-08-05T12:00:01Z","type":"subagent_spawn","session_id":"sess-abc123","agent_id":"ws1","workflow_run_id":"wf_seed-1"}
`
	if err := os.WriteFile(day, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	obs := NewObserver(e.historyDir)

	ev := obs.Reconcile(e.sess, e.c, time.Now())
	if hasWorkflowEvent(ev, history.EventWorkflowStart, "wf_seed-1") {
		t.Fatalf("workflow_start was already emitted before restart; must not re-emit")
	}
	if hasEvent(ev, history.EventSubagentSpawn, "ws1") {
		t.Fatalf("ws1 spawn was already emitted before restart; must not re-emit")
	}
	if e.c.InFlightSubagents != 1 {
		t.Fatalf("inflight = %d, want 1 — the restart must still recover the running agent", e.c.InFlightSubagents)
	}
	if len(e.c.Workflows) != 1 || e.c.Workflows[0].Name != "big-audit" {
		t.Fatalf("Workflows = %+v, want the seeded run still summarized", e.c.Workflows)
	}
}
