package fanout

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/state"
)

// env is a throwaway session layout: a parent transcript at <base>/<sid>.jsonl and
// its sibling subagents dir <base>/<sid>/subagents, plus a separate history dir.
type env struct {
	base, sid, transcript, subdir, historyDir string
	sess                                      *state.Session
	c                                         *state.AgentInfo
}

func newEnv(t *testing.T) env {
	t.Helper()
	base := t.TempDir()
	sid := "sess-abc123"
	transcript := filepath.Join(base, sid+".jsonl")
	subdir := filepath.Join(base, sid, "subagents")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return env{
		base: base, sid: sid, transcript: transcript, subdir: subdir,
		historyDir: t.TempDir(),
		sess:       &state.Session{PID: 1234, Agent: "claude", CWD: "/proj"},
		c:          &state.AgentInfo{SessionID: sid, Transcript: transcript},
	}
}

// writeSub writes one subagent's meta.json + jsonl. stopReason "" => still running
// (stop_reason null); "end_turn" => finished. Returns the jsonl path.
func writeSub(t *testing.T, subdir, id, metaJSON, stopReason string) string {
	t.Helper()
	if metaJSON != "" {
		if err := os.WriteFile(filepath.Join(subdir, "agent-"+id+".meta.json"), []byte(metaJSON), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	line := `{"type":"assistant","message":{"role":"assistant","stop_reason":null}}`
	if stopReason != "" {
		line = `{"type":"assistant","message":{"role":"assistant","stop_reason":"` + stopReason + `"}}`
	}
	p := filepath.Join(subdir, "agent-"+id+".jsonl")
	if err := os.WriteFile(p, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func countType(evs []history.Event, typ string) int {
	n := 0
	for _, e := range evs {
		if e.Type == typ {
			n++
		}
	}
	return n
}

func hasEvent(evs []history.Event, typ, agentID string) bool {
	for _, e := range evs {
		if e.Type == typ && e.AgentID == agentID {
			return true
		}
	}
	return false
}

func metaClassic(depth int, toolUseID string) string {
	return `{"agentType":"Explore","description":"probe","spawnDepth":` +
		strconv.Itoa(depth) + `,"toolUseId":"` + toolUseID + `"}`
}

func TestReconcile_spawnOnceCompletionAndInflight(t *testing.T) {
	e := newEnv(t)
	writeSub(t, e.subdir, "a1", metaClassic(1, "toolu_a1"), "")         // running
	writeSub(t, e.subdir, "a2", metaClassic(1, "toolu_a2"), "end_turn") // finished
	obs := NewObserver(e.historyDir)
	now := time.Now()

	ev := obs.Reconcile(e.sess, e.c, now)
	if !hasEvent(ev, history.EventSubagentSpawn, "a1") || !hasEvent(ev, history.EventSubagentSpawn, "a2") {
		t.Fatalf("first pass should spawn a1 and a2; got %+v", ev)
	}
	if !hasEvent(ev, history.EventSubagentStop, "a2") {
		t.Fatalf("a2 is finished; expected a stop; got %+v", ev)
	}
	if hasEvent(ev, history.EventSubagentStop, "a1") {
		t.Fatalf("a1 still running; must not stop")
	}
	if e.c.InFlightSubagents != 1 {
		t.Fatalf("inflight = %d, want 1 (a1 running, a2 done)", e.c.InFlightSubagents)
	}

	// Idempotent: a second pass with no change emits nothing and holds the count.
	if ev := obs.Reconcile(e.sess, e.c, now); len(ev) != 0 {
		t.Fatalf("second pass should emit nothing; got %+v", ev)
	}
	if e.c.InFlightSubagents != 1 {
		t.Fatalf("inflight drifted to %d", e.c.InFlightSubagents)
	}

	// a1 finishes -> exactly one stop, inflight drops to 0.
	writeSub(t, e.subdir, "a1", metaClassic(1, "toolu_a1"), "end_turn")
	ev = obs.Reconcile(e.sess, e.c, now)
	if countType(ev, history.EventSubagentStop) != 1 || !hasEvent(ev, history.EventSubagentStop, "a1") {
		t.Fatalf("a1 finishing should emit one stop for a1; got %+v", ev)
	}
	if e.c.InFlightSubagents != 0 {
		t.Fatalf("inflight = %d, want 0", e.c.InFlightSubagents)
	}
}

func TestReconcile_grandchildNotCountedButEmitted(t *testing.T) {
	e := newEnv(t)
	writeSub(t, e.subdir, "g1", metaClassic(2, "toolu_g1"), "") // depth 2 grandchild, running
	obs := NewObserver(e.historyDir)

	ev := obs.Reconcile(e.sess, e.c, time.Now())
	if !hasEvent(ev, history.EventSubagentSpawn, "g1") {
		t.Fatalf("grandchild spawn should still be recorded for fidelity; got %+v", ev)
	}
	if e.c.InFlightSubagents != 0 {
		t.Fatalf("grandchild (spawnDepth>=2) must not count toward main inflight; got %d", e.c.InFlightSubagents)
	}
}

func TestReconcile_teammateCountsWithoutToolUseID(t *testing.T) {
	e := newEnv(t)
	// In-process teammate: spawnDepth 0, no toolUseId, taskKind set.
	writeSub(t, e.subdir, "tm1", `{"agentType":"general-purpose","spawnDepth":0,"taskKind":"in_process_teammate"}`, "")
	obs := NewObserver(e.historyDir)

	ev := obs.Reconcile(e.sess, e.c, time.Now())
	if !hasEvent(ev, history.EventSubagentSpawn, "tm1") {
		t.Fatalf("teammate spawn should be detected via agent_id; got %+v", ev)
	}
	if e.c.InFlightSubagents != 1 {
		t.Fatalf("a running depth-0 teammate must count; inflight = %d", e.c.InFlightSubagents)
	}
}

func TestReconcile_seedsFromHistoryNoReemitOnRestart(t *testing.T) {
	e := newEnv(t)
	// Simulate a prior daemon run: history already recorded r1's spawn (no stop —
	// it was in flight at restart) for THIS session, plus an unrelated session.
	day := filepath.Join(e.historyDir, "2026-06-27.jsonl")
	lines := `{"ts":"2026-06-27T12:00:00Z","type":"subagent_spawn","session_id":"sess-abc123","agent_id":"r1"}
{"ts":"2026-06-27T12:00:00Z","type":"subagent_spawn","session_id":"other","agent_id":"x9"}
`
	if err := os.WriteFile(day, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	// r1 finished during the downtime; n1 is a brand-new running fanout.
	writeSub(t, e.subdir, "r1", metaClassic(1, "toolu_r1"), "end_turn")
	writeSub(t, e.subdir, "n1", metaClassic(1, "toolu_n1"), "")
	obs := NewObserver(e.historyDir)

	ev := obs.Reconcile(e.sess, e.c, time.Now())
	if hasEvent(ev, history.EventSubagentSpawn, "r1") {
		t.Fatalf("r1 spawn was already emitted before restart; must NOT re-emit (G1 double-count)")
	}
	if !hasEvent(ev, history.EventSubagentStop, "r1") {
		t.Fatalf("r1 finished during downtime; the missed stop should be emitted; got %+v", ev)
	}
	if !hasEvent(ev, history.EventSubagentSpawn, "n1") {
		t.Fatalf("a genuinely new fanout n1 should still spawn; got %+v", ev)
	}
	if e.c.InFlightSubagents != 1 {
		t.Fatalf("inflight = %d, want 1 (only n1)", e.c.InFlightSubagents)
	}
}

func TestReconcile_staleCapForceClosesLeak(t *testing.T) {
	e := newEnv(t)
	jsonl := writeSub(t, e.subdir, "s1", metaClassic(1, "toolu_s1"), "") // running, never terminal
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(jsonl, old, old); err != nil {
		t.Fatal(err)
	}
	obs := NewObserver(e.historyDir)
	obs.SetStaleCap(30 * time.Minute)

	ev := obs.Reconcile(e.sess, e.c, time.Now())
	if !hasEvent(ev, history.EventSubagentStop, "s1") {
		t.Fatalf("a quiescent subagent past the stale cap must be force-closed; got %+v", ev)
	}
	if e.c.InFlightSubagents != 0 {
		t.Fatalf("inflight = %d, want 0 after force-close", e.c.InFlightSubagents)
	}
}

func TestReconcile_toolResultCompletesViaCursor(t *testing.T) {
	e := newEnv(t)
	// Classic fanout whose own jsonl never reaches end_turn; completion must come
	// from the parent transcript's tool_result, caught by the forward cursor.
	writeSub(t, e.subdir, "c1", metaClassic(1, "toolu_c1"), "")
	obs := NewObserver(e.historyDir)
	now := time.Now()

	if ev := obs.Reconcile(e.sess, e.c, now); !hasEvent(ev, history.EventSubagentSpawn, "c1") {
		t.Fatalf("c1 should spawn; got %+v", ev)
	}
	if e.c.InFlightSubagents != 1 {
		t.Fatalf("inflight = %d, want 1", e.c.InFlightSubagents)
	}
	// The tool_result lands in the parent transcript AFTER first sight.
	appendLine(t, e.transcript, `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_c1"}]}}`)
	ev := obs.Reconcile(e.sess, e.c, now)
	if !hasEvent(ev, history.EventSubagentStop, "c1") {
		t.Fatalf("c1's tool_result should complete it via the cursor; got %+v", ev)
	}
	if e.c.InFlightSubagents != 0 {
		t.Fatalf("inflight = %d, want 0", e.c.InFlightSubagents)
	}
}

func TestReconcile_noopsWithoutSessionOrTranscript(t *testing.T) {
	obs := NewObserver(t.TempDir())
	now := time.Now()
	if ev := obs.Reconcile(nil, &state.AgentInfo{SessionID: "s", Transcript: "/x"}, now); ev != nil {
		t.Fatalf("nil session must no-op")
	}
	if ev := obs.Reconcile(&state.Session{}, &state.AgentInfo{SessionID: "", Transcript: "/x"}, now); ev != nil {
		t.Fatalf("empty session-id must no-op (cannot seed/key safely)")
	}
	if ev := obs.Reconcile(&state.Session{}, &state.AgentInfo{SessionID: "s", Transcript: ""}, now); ev != nil {
		t.Fatalf("empty transcript must no-op")
	}
}

func TestPrune_dropsDeadSessions(t *testing.T) {
	e := newEnv(t)
	writeSub(t, e.subdir, "a1", metaClassic(1, "toolu_a1"), "")
	obs := NewObserver(e.historyDir)
	obs.Reconcile(e.sess, e.c, time.Now())
	obs.Prune(map[string]bool{"some-other-session": true}) // e.sid not live
	obs.mu.Lock()
	_, present := obs.sessions[e.sid]
	obs.mu.Unlock()
	if present {
		t.Fatalf("pruned session state should be dropped")
	}
}

func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}
