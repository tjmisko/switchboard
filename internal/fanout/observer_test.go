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

// seedHistory writes the prior-run history the seeding tests share: r1's spawn
// was already emitted for this session, plus an unrelated session's.
func seedHistory(t *testing.T, e env) string {
	t.Helper()
	day := filepath.Join(e.historyDir, "2026-06-27.jsonl")
	lines := `{"ts":"2026-06-27T12:00:00Z","type":"subagent_spawn","session_id":"sess-abc123","agent_id":"r1"}
{"ts":"2026-06-27T12:00:00Z","type":"subagent_spawn","session_id":"other","agent_id":"x9"}
`
	if err := os.WriteFile(day, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	return day
}

// This is the test that fails without the hoist, and the trick is deleting the
// history between the two calls.
//
// Prime is claimed to do the seed read so that Reconcile — which runs under the
// store lock — does not. "Did not read" is otherwise unobservable from outside,
// so the history is removed after Prime: if Reconcile still seeded from disk it
// would find nothing, and r1's already-emitted spawn would come back out. A
// passing run therefore proves the seed came from Prime.
func TestPrime_seedsBeforeTheLockSoReconcileDoesNotReadHistory(t *testing.T) {
	e := newEnv(t)
	day := seedHistory(t, e)
	writeSub(t, e.subdir, "r1", metaClassic(1, "toolu_r1"), "end_turn")
	obs := NewObserver(e.historyDir)

	obs.Prime(e.c.SessionID, e.c.Transcript)
	if err := os.Remove(day); err != nil {
		t.Fatal(err)
	}

	ev := obs.Reconcile(e.sess, e.c, time.Now())
	if hasEvent(ev, history.EventSubagentSpawn, "r1") {
		t.Fatalf("r1's spawn was re-emitted, so Reconcile seeded from a history that Prime should already have read; got %+v", ev)
	}
	if !hasEvent(ev, history.EventSubagentStop, "r1") {
		t.Fatalf("r1 finished during downtime; the missed stop should still be emitted; got %+v", ev)
	}
}

func TestPrime_leavesAnAlreadySeededSessionAlone(t *testing.T) {
	e := newEnv(t)
	seedHistory(t, e)
	writeSub(t, e.subdir, "r1", metaClassic(1, "toolu_r1"), "end_turn")
	obs := NewObserver(e.historyDir)

	// Reconcile seeds lazily and emits r1's stop, marking it stopped.
	if ev := obs.Reconcile(e.sess, e.c, time.Now()); !hasEvent(ev, history.EventSubagentStop, "r1") {
		t.Fatalf("setup: expected r1's stop on the first Reconcile; got %+v", ev)
	}
	// A later Prime must not reset that back to unseeded — doing so would re-emit
	// the stop, since the seen-sets it installs come from history alone.
	obs.Prime(e.c.SessionID, e.c.Transcript)

	if ev := obs.Reconcile(e.sess, e.c, time.Now()); len(ev) != 0 {
		t.Fatalf("a second Prime+Reconcile must emit nothing; got %+v", ev)
	}
}

// The sample carries reads taken before the lock, so the cursor it was taken
// against can move before it is applied — the hook trigger reconciles the same
// session independently. Applying a stale sample would rewind the cursor to its
// older newOffset and re-fold signals that were already folded in. The guard is
// exact rather than serialized, so this pins that it actually rejects.
func TestReconcileFrom_ignoresASampleTakenAgainstAMovedCursor(t *testing.T) {
	e := newEnv(t)
	writeSub(t, e.subdir, "n1", metaClassic(1, "toolu_n1"), "")
	obs := NewObserver(e.historyDir)

	stale := obs.Sample(e.c.SessionID, e.c.Transcript)

	// Something else reconciles first and advances the cursor past the sample.
	if err := os.WriteFile(e.transcript, []byte(`{"type":"system"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	obs.Reconcile(e.sess, e.c, time.Now())

	// n1 was already spawned by that Reconcile, so a correctly-rejected stale
	// sample yields no second spawn — and the cursor must not go backwards.
	before := obs.cursorFor(e.c.SessionID)
	if ev := obs.ReconcileFrom(stale, e.sess, e.c, time.Now()); hasEvent(ev, history.EventSubagentSpawn, "n1") {
		t.Fatalf("a stale sample must not re-emit n1's spawn; got %+v", ev)
	}
	if after := obs.cursorFor(e.c.SessionID); after < before {
		t.Fatalf("cursor went backwards: %d -> %d; a stale sample was applied", before, after)
	}
}

func TestReconcileFrom_appliesAFreshSampleWithoutReadingAgain(t *testing.T) {
	e := newEnv(t)
	writeSub(t, e.subdir, "n1", metaClassic(1, "toolu_n1"), "")
	obs := NewObserver(e.historyDir)

	s := obs.Sample(e.c.SessionID, e.c.Transcript)

	// Delete the subagents dir the sample already read. If ReconcileFrom went back
	// to disk, it would now find nothing and emit no spawn.
	if err := os.RemoveAll(filepath.Join(e.base, e.sid)); err != nil {
		t.Fatal(err)
	}

	if ev := obs.ReconcileFrom(s, e.sess, e.c, time.Now()); !hasEvent(ev, history.EventSubagentSpawn, "n1") {
		t.Fatalf("n1's spawn should come from the sample taken before the dir was removed; got %+v", ev)
	}
	if e.c.InFlightSubagents != 1 {
		t.Fatalf("inflight = %d, want 1 from the sampled dir scan", e.c.InFlightSubagents)
	}
}

// The reproduced defect: the freshness guard used to check only the transcript
// byte cursor, which covers the TasksSince half of a sample and nothing else. The
// subagents dir scan has no cursor, so a sample could carry a dir listing a
// competing Reconcile had already superseded — and applying it RESURRECTED the
// superseded state.
//
// A stamp on the dir cannot close this. The terminal entry is appended to the
// subagent's OWN jsonl, and appending to an existing file does not move its
// directory's mtime (only creating/removing/renaming an entry does), so a dir
// stamp is blind to exactly the write that matters here. What IS detectable
// exactly, and for free, is the thing that actually invalidates the sample:
// another producer reconciled this session in between.
func TestReconcileFrom_doesNotResurrectASubagentTheStopHookAlreadyDrained(t *testing.T) {
	e := newEnv(t)
	writeSub(t, e.subdir, "a1", metaClassic(1, "toolu_a1"), "") // running
	obs := NewObserver(e.historyDir)
	now := time.Now()

	// The tick samples while a1 runs: subs=[a1 running], taken at cursor 0.
	s := obs.Sample(e.c.SessionID, e.c.Transcript)

	// a1 finishes. Its terminal entry goes to its own jsonl; the PARENT transcript
	// is untouched because a1's tool_result has not landed yet — so the cursor
	// cannot move, and a cursor-only guard sees nothing at all happen.
	writeSub(t, e.subdir, "a1", metaClassic(1, "toolu_a1"), "end_turn")

	// The SubagentStop hook reconciles inline, ahead of the tick that sampled.
	if ev := obs.Reconcile(e.sess, e.c, now); !hasEvent(ev, history.EventSubagentStop, "a1") {
		t.Fatalf("setup: the hook's Reconcile should stop a1; got %+v", ev)
	}
	if e.c.InFlightSubagents != 0 {
		t.Fatalf("setup: inflight = %d, want 0 once the hook drained a1", e.c.InFlightSubagents)
	}

	// Only now does the tick apply the sample it took before any of that.
	obs.ReconcileFrom(s, e.sess, e.c, now)
	if e.c.InFlightSubagents != 0 {
		t.Fatalf("a stale sample resurrected a drained subagent: inflight = %d, want 0 "+
			"(selfHealStuckStatus reads this and paints a phantom delegating span for a full tick)",
			e.c.InFlightSubagents)
	}
}

// The mirror of the case above, equally reachable: a subagent that appears between
// Sample and apply is counted by the SubagentStart hook, then zeroed by the stale
// sample's empty dir listing.
func TestReconcileFrom_doesNotZeroASubagentTheStartHookAlreadyCounted(t *testing.T) {
	e := newEnv(t)
	obs := NewObserver(e.historyDir)
	now := time.Now()

	// Sampled with an empty subagents dir: subs=[], cursor 0.
	s := obs.Sample(e.c.SessionID, e.c.Transcript)

	// A fanout starts and the SubagentStart hook reconciles it in.
	writeSub(t, e.subdir, "a1", metaClassic(1, "toolu_a1"), "")
	if ev := obs.Reconcile(e.sess, e.c, now); !hasEvent(ev, history.EventSubagentSpawn, "a1") {
		t.Fatalf("setup: the hook's Reconcile should spawn a1; got %+v", ev)
	}

	obs.ReconcileFrom(s, e.sess, e.c, now)
	if e.c.InFlightSubagents != 1 {
		t.Fatalf("a stale sample zeroed a live subagent: inflight = %d, want 1", e.c.InFlightSubagents)
	}
}

// A session id is not by itself a unique key for a transcript: two panes resumed
// onto one conversation, or a hook payload carrying transcript_path without
// session_id (which handleHook tolerates), put two store sessions on one id with
// different transcripts. usableFor must compare the path — its sibling
// signalSample.freshFor always has — or one session's dir scan is applied to the
// other's sessions.
func TestReconcileFrom_rejectsASampleTakenAgainstADifferentTranscript(t *testing.T) {
	e := newEnv(t)
	writeSub(t, e.subdir, "a1", metaClassic(1, "toolu_a1"), "")

	// A second transcript wearing the SAME session id, with its own subagents dir.
	other := filepath.Join(e.base, "other.jsonl")
	otherSub := filepath.Join(e.base, "other", "subagents")
	if err := os.MkdirAll(otherSub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	writeSub(t, otherSub, "b1", metaClassic(1, "toolu_b1"), "")

	obs := NewObserver(e.historyDir)
	s := obs.Sample(e.c.SessionID, e.transcript) // sampled against transcript A

	c := &state.AgentInfo{SessionID: e.sid, Transcript: other} // applied to transcript B
	ev := obs.ReconcileFrom(s, e.sess, c, time.Now())
	if hasEvent(ev, history.EventSubagentSpawn, "a1") {
		t.Fatalf("transcript A's dir scan was applied to transcript B; got %+v", ev)
	}
	if !hasEvent(ev, history.EventSubagentSpawn, "b1") {
		t.Fatalf("a sample for another transcript should fall back to an inline read of B's own dir; got %+v", ev)
	}
}

// A sample whose reads FAILED carries no facts, but it used to satisfy the guard
// anyway (valid was set before the reads, never revised by them). ReconcileFrom
// therefore skipped the inline fallback and then bailed on !subsOK, giving up for
// the whole tick where plain Reconcile would have retried and recovered in it.
// Claude Code recreates <session>/subagents/ during a /clear, so a momentarily
// unreadable dir is reachable in ordinary use.
func TestReconcileFrom_readsInlineWhenTheSampleReadsFailed(t *testing.T) {
	e := newEnv(t)
	writeSub(t, e.subdir, "a1", metaClassic(1, "toolu_a1"), "")
	obs := NewObserver(e.historyDir)

	if err := os.Chmod(e.subdir, 0o000); err != nil {
		t.Fatal(err)
	}
	s := obs.Sample(e.c.SessionID, e.c.Transcript)
	if err := os.Chmod(e.subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if s.subsOK {
		t.Skip("the dir scan succeeded despite mode 0000 — running as root?")
	}

	if ev := obs.ReconcileFrom(s, e.sess, e.c, time.Now()); !hasEvent(ev, history.EventSubagentSpawn, "a1") {
		t.Fatalf("a sample whose reads failed must fall back to an inline read; got %+v", ev)
	}
	if e.c.InFlightSubagents != 1 {
		t.Fatalf("inflight = %d, want 1 after the inline recovery", e.c.InFlightSubagents)
	}
}

func TestPrime_isANoopWithoutASessionID(t *testing.T) {
	e := newEnv(t)
	obs := NewObserver(e.historyDir)
	obs.Prime("", e.c.Transcript) // must not panic, must not create state
	if len(obs.sessions) != 0 {
		t.Fatalf("an empty session id must not create observer state; got %v", obs.sessions)
	}
}

func TestPrime_servesConcurrentCallersWithoutRacingTheSeed(t *testing.T) {
	e := newEnv(t)
	seedHistory(t, e)
	writeSub(t, e.subdir, "r1", metaClassic(1, "toolu_r1"), "end_turn")
	obs := NewObserver(e.historyDir)

	// Two producers prime the same cold session at once — the tick and the hook
	// trigger can genuinely overlap. Both may read; neither may corrupt the state.
	// Under -race this also covers the lock discipline in Prime.
	done := make(chan struct{}, 2)
	for i := 0; i < 2; i++ {
		go func() {
			obs.Prime(e.c.SessionID, e.c.Transcript)
			done <- struct{}{}
		}()
	}
	<-done
	<-done

	if ev := obs.Reconcile(e.sess, e.c, time.Now()); hasEvent(ev, history.EventSubagentSpawn, "r1") {
		t.Fatalf("r1's spawn must stay suppressed after a concurrent prime; got %+v", ev)
	}
}

// The seed is what stops a restart or `--resume` re-announcing every subagent the
// session ever had. A FAILED history read says nothing about what was already
// emitted, so it must not be mistaken for "nothing was emitted": marking the
// session seeded on that would hand the authoritative dir scan an empty
// already-emitted set, and it would re-announce the lot (G1).
//
// The read fails for ordinary reasons — EMFILE under load, a permission blip
// during a backup, an over-long event line — so this is a path, not a theory.
func TestReconcile_doesNotEmitOrMarkSeededWhenTheHistoryReadFails(t *testing.T) {
	e := newEnv(t)
	day := seedHistory(t, e)
	writeSub(t, e.subdir, "r1", metaClassic(1, "toolu_r1"), "end_turn")
	obs := NewObserver(e.historyDir)

	if err := os.Chmod(day, 0o000); err != nil {
		t.Fatal(err)
	}
	if f, err := os.Open(day); err == nil {
		f.Close()
		t.Skip("the history file is still readable at mode 0000 — running as root?")
	}

	if ev := obs.Reconcile(e.sess, e.c, time.Now()); len(ev) != 0 {
		t.Fatalf("a tick that could not read history must emit nothing; got %+v", ev)
	}

	// The history comes back. r1's spawn was already recorded before the restart, so
	// the recovered tick must emit its missed STOP and not a second spawn.
	if err := os.Chmod(day, 0o644); err != nil {
		t.Fatal(err)
	}
	ev := obs.Reconcile(e.sess, e.c, time.Now())
	if hasEvent(ev, history.EventSubagentSpawn, "r1") {
		t.Fatalf("r1's spawn was re-emitted: the session was marked seeded from a failed read; got %+v", ev)
	}
	if !hasEvent(ev, history.EventSubagentStop, "r1") {
		t.Fatalf("the retry should seed and then emit r1's missed stop; got %+v", ev)
	}
}

// G5 durability. On /clear or compaction the transcript shrinks below the cursor.
// The reset must be written to the durable cursor whether or not the read that
// followed it succeeded — a truncation and an unreadable file coincide exactly
// when the file is being replaced, and a cursor left past EOF skips everything
// written between it and the file's regrowth.
func TestReconcile_resetsTheCursorOnShrinkEvenWhenTheReadFails(t *testing.T) {
	e := newEnv(t)
	obs := NewObserver(e.historyDir)
	now := time.Now()

	// Push the cursor out to a real offset.
	appendLine(t, e.transcript, `{"type":"system","note":"a reasonably long first line to advance the cursor"}`)
	obs.Reconcile(e.sess, e.c, now)
	if obs.cursorFor(e.c.SessionID) == 0 {
		t.Fatal("setup: the cursor should have advanced past the first line")
	}

	// A /clear truncates the transcript AND the file is momentarily unreadable —
	// os.Stat still answers (so the shrink is visible) while the read cannot open it.
	if err := os.WriteFile(e.transcript, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(e.transcript, 0o000); err != nil {
		t.Fatal(err)
	}
	if f, err := os.Open(e.transcript); err == nil {
		f.Close()
		t.Skip("the transcript is still readable at mode 0000 — running as root?")
	}

	obs.Reconcile(e.sess, e.c, now)
	if got := obs.cursorFor(e.c.SessionID); got != 0 {
		t.Fatalf("cursor = %d after a truncation whose read failed, want 0: it is now past EOF, "+
			"and everything written before the file passes it again is lost", got)
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

func TestReconcile_tagsBackgroundFromParentToolUse(t *testing.T) {
	e := newEnv(t)
	obs := NewObserver(e.historyDir)
	now := time.Now()

	// First sight primes the cursor to EOF (empty dir, empty transcript). The
	// backgrounded tool_use and the subagent's dir entry then both land, so the
	// cursor learns run_in_background BEFORE the dir scan emits the spawn.
	obs.Reconcile(e.sess, e.c, now)
	appendLine(t, e.transcript, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_bg","name":"Agent","input":{"subagent_type":"general-purpose","run_in_background":true}}]}}`)
	writeSub(t, e.subdir, "bg1", metaClassic(1, "toolu_bg"), "")
	ev := obs.Reconcile(e.sess, e.c, now)

	var spawn *history.Event
	for i := range ev {
		if ev[i].Type == history.EventSubagentSpawn && ev[i].AgentID == "bg1" {
			spawn = &ev[i]
		}
	}
	if spawn == nil {
		t.Fatalf("expected a spawn for bg1; got %+v", ev)
	}
	if !spawn.Background {
		t.Fatalf("spawn should be tagged Background (run_in_background tool_use); got %+v", *spawn)
	}
}

func TestReconcile_backgroundIgnoresSpawnAckResult(t *testing.T) {
	// Regression: a run_in_background fanout gets an immediate "Spawned
	// successfully" tool_result that is NOT completion. The Observer must NOT treat
	// that as done — it would stop every background agent ~1s after it starts.
	e := newEnv(t)
	obs := NewObserver(e.historyDir)
	now := time.Now()

	obs.Reconcile(e.sess, e.c, now) // prime cursor to EOF (empty)

	// The backgrounded tool_use AND its immediate spawn-ack tool_result both land,
	// and the subagent's dir entry appears — but its jsonl is still running.
	appendLine(t, e.transcript, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_bg","name":"Agent","input":{"subagent_type":"Explore","run_in_background":true}}]}}`)
	appendLine(t, e.transcript, `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_bg","content":"Spawned successfully. agent_id: bg1"}]}}`)
	writeSub(t, e.subdir, "bg1", metaClassic(1, "toolu_bg"), "") // running, NOT terminal

	ev := obs.Reconcile(e.sess, e.c, now)
	if hasEvent(ev, history.EventSubagentStop, "bg1") {
		t.Fatalf("background fanout must NOT stop on its spawn-ack tool_result; got %+v", ev)
	}
	if e.c.InFlightSubagents != 1 {
		t.Fatalf("background fanout should still be in flight; inflight = %d, want 1", e.c.InFlightSubagents)
	}

	// It genuinely finishes: jsonl reaches end_turn -> now it stops.
	writeSub(t, e.subdir, "bg1", metaClassic(1, "toolu_bg"), "end_turn")
	ev = obs.Reconcile(e.sess, e.c, now)
	if !hasEvent(ev, history.EventSubagentStop, "bg1") {
		t.Fatalf("a finished background fanout (jsonl end_turn) should stop; got %+v", ev)
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
