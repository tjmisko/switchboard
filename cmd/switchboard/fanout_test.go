package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

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

// The wiring test for the pre-lock reads. The Observer's own tests cover Sample
// and Prime; this one covers sampleFanout actually reaching the sessions in the
// store, since a Sample that is never taken is exactly as slow as no Sample.
//
// Same trick as the Observer test: the history is deleted after sampleFanout, so
// a Reconcile that still seeded from disk would find nothing and re-emit r1's
// already-recorded spawn.
func TestSampleFanoutSeedsEverySessionInTheSnapshot(t *testing.T) {
	base := t.TempDir()
	sid := "s-primed"
	tpath := filepath.Join(base, sid+".jsonl")
	writeLines(t, tpath, `{"type":"system"}`)
	subdir := filepath.Join(base, sid, "subagents")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	// r1 ran and finished; its spawn was already emitted on a previous daemon run.
	if err := os.WriteFile(filepath.Join(subdir, "agent-r1.meta.json"),
		[]byte(`{"agentType":"Explore","description":"probe","spawnDepth":1,"toolUseId":"toolu_r1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subdir, "agent-r1.jsonl"),
		[]byte(`{"type":"assistant","message":{"role":"assistant","stop_reason":"end_turn"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	obsDir := t.TempDir()
	day := filepath.Join(obsDir, "2026-06-27.jsonl")
	if err := os.WriteFile(day,
		[]byte(`{"ts":"2026-06-27T12:00:00Z","type":"subagent_spawn","session_id":"`+sid+`","agent_id":"r1"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	histDir := t.TempDir()
	sink := history.NewSink(history.Config{Enabled: true, Detail: history.DetailFull, Dir: histDir})
	rs := newReconcileState(fanout.NewObserver(obsDir), nil)
	sess := &state.Session{PID: 11, Agent: "claude", CWD: "/home/u/proj",
		Claude: &state.AgentInfo{SessionID: sid, Transcript: tpath}}

	store := state.New(filepath.Join(t.TempDir(), "state.json"))
	store.Apply(func(m map[int]*state.Session) { m[sess.PID] = sess })

	rs.sampleFanout(store)
	if err := os.Remove(day); err != nil {
		t.Fatal(err)
	}

	rs.observe(sink, sess, sess.Claude, time.Now())
	sink.Close()

	for _, ev := range eventsOfType(readEvents(t, histDir), history.EventSubagentSpawn) {
		if ev.AgentID == "r1" {
			t.Fatalf("r1's spawn was re-emitted, so primeFanout did not seed this session before observe ran: %+v", ev)
		}
	}
}

// The seed test above passes even if observeFanout throws the sample away, since
// sampleFanout seeds as a side effect. This one covers the plumbing itself: the
// subagents dir is removed after sampling, so the spawn can ONLY come from the
// sample. It fails if observeFanout reads inline instead.
func TestObserveFanoutAppliesTheSampledDirScan(t *testing.T) {
	base := t.TempDir()
	sid := "s-sampled"
	tpath := filepath.Join(base, sid+".jsonl")
	writeLines(t, tpath, `{"type":"system"}`)
	subdir := filepath.Join(base, sid, "subagents")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subdir, "agent-n1.meta.json"),
		[]byte(`{"agentType":"Explore","description":"probe","spawnDepth":1,"toolUseId":"toolu_n1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subdir, "agent-n1.jsonl"),
		[]byte(`{"type":"assistant","message":{"role":"assistant","stop_reason":null}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	histDir := t.TempDir()
	sink := history.NewSink(history.Config{Enabled: true, Detail: history.DetailFull, Dir: histDir})
	rs := newReconcileState(fanout.NewObserver(t.TempDir()), nil)
	sess := &state.Session{PID: 12, Agent: "claude", CWD: "/home/u/proj",
		Claude: &state.AgentInfo{SessionID: sid, Transcript: tpath}}

	store := state.New(filepath.Join(t.TempDir(), "state.json"))
	store.Apply(func(m map[int]*state.Session) { m[sess.PID] = sess })

	rs.sampleFanout(store)
	if err := os.RemoveAll(filepath.Join(base, sid)); err != nil {
		t.Fatal(err)
	}

	rs.observe(sink, sess, sess.Claude, time.Now())
	sink.Close()

	var spawned bool
	for _, ev := range eventsOfType(readEvents(t, histDir), history.EventSubagentSpawn) {
		if ev.AgentID == "n1" {
			spawned = true
		}
	}
	if !spawned {
		t.Fatal("n1's spawn must come from the pre-lock sample; observeFanout appears to have read inline instead")
	}
	if sess.Claude.InFlightSubagents != 1 {
		t.Fatalf("inflight = %d, want 1 from the sampled dir scan", sess.Claude.InFlightSubagents)
	}
}

func TestObserveUsageEmitsOneSamplePerModel(t *testing.T) {
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
	if len(samples) != 2 {
		t.Fatalf("got %d usage samples, want one per distinct model: %+v", len(samples), samples)
	}
	byModel := map[string]history.Event{}
	for _, s := range samples {
		byModel[s.Model] = s
	}
	if o := byModel["claude-opus-4-8"]; o.TokIn != 120 || o.TokOut != 48 {
		t.Errorf("opus sample = %+v, want summed 120/48", o)
	}
	if h := byModel["claude-haiku-4-5"]; h.TokIn != 10 || h.TokOut != 5 {
		t.Errorf("haiku sample = %+v, want 10/5", h)
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

func TestObserveUsagePrimesThenSamples(t *testing.T) {
	dir := t.TempDir()
	tpath := filepath.Join(dir, "t.jsonl")
	// Pre-existing backlog: must NOT be counted (it predates our watching).
	writeLines(t, tpath, `{"type":"assistant","message":{"role":"assistant","content":[],"usage":{"input_tokens":9999,"output_tokens":9999}}}`)

	histDir := t.TempDir()
	sink := history.NewSink(history.Config{Enabled: true, Detail: history.DetailFull, Dir: histDir})
	rs := newReconcileState(fanout.NewObserver(t.TempDir()), nil)
	sess := &state.Session{PID: 1, Agent: "claude", CWD: "/home/u/proj",
		Claude: &state.AgentInfo{SessionID: "s1", Transcript: tpath}}

	// First observe primes the usage cursor to EOF — no sample for the backlog.
	rs.observe(sink, sess, sess.Claude, time.Now())

	// New usage accrues while we watch.
	f, _ := os.OpenFile(tpath, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(`{"type":"assistant","message":{"role":"assistant","content":[],"usage":{"input_tokens":120,"output_tokens":34}}}` + "\n")
	f.Close()

	rs.observe(sink, sess, sess.Claude, time.Now())
	sink.Close()

	samples := eventsOfType(readEvents(t, histDir), history.EventUsageSample)
	if len(samples) != 1 {
		t.Fatalf("got %d usage samples, want 1 (backlog primed away): %+v", len(samples), samples)
	}
	if samples[0].TokIn != 120 || samples[0].TokOut != 34 {
		t.Errorf("usage sample = %+v, want only the post-priming delta (120/34)", samples[0])
	}
}
