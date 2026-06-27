package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/statustune"
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

func taskUse(id, agentType, desc string) string {
	return `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"` + id +
		`","name":"Task","input":{"subagent_type":"` + agentType + `","description":"` + desc + `"}}]}}`
}

func taskResult(id string) string {
	return `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + id + `"}]}}`
}

func TestObserveFanoutSpawnThenStop(t *testing.T) {
	dir := t.TempDir()
	tpath := filepath.Join(dir, "t.jsonl")
	writeLines(t, tpath, taskUse("toolu_a", "Explore", "map the auth code"))

	histDir := t.TempDir()
	sink := history.NewSink(history.Config{Enabled: true, Detail: history.DetailFull, Dir: histDir})
	rs := newReconcileState()
	sess := &state.Session{PID: 1, Agent: "claude", CWD: "/home/u/proj",
		Claude: &state.AgentInfo{SessionID: "s1", Transcript: tpath}}

	rs.observe(sink, sess, sess.Claude, time.Now(), statustune.Default())
	if sess.Claude.InFlightSubagents != 1 {
		t.Errorf("in-flight = %d, want 1", sess.Claude.InFlightSubagents)
	}

	// The subagent finishes: its result lands in the main transcript.
	writeLines(t, tpath, taskUse("toolu_a", "Explore", "map the auth code"), taskResult("toolu_a"))
	rs.observe(sink, sess, sess.Claude, time.Now(), statustune.Default())
	if sess.Claude.InFlightSubagents != 0 {
		t.Errorf("after drain, in-flight = %d, want 0", sess.Claude.InFlightSubagents)
	}

	// A third observe with no change must not re-emit.
	rs.observe(sink, sess, sess.Claude, time.Now(), statustune.Default())
	sink.Close()

	evs := readEvents(t, histDir)
	spawns := eventsOfType(evs, history.EventSubagentSpawn)
	stops := eventsOfType(evs, history.EventSubagentStop)
	if len(spawns) != 1 {
		t.Fatalf("got %d spawn events, want exactly 1: %+v", len(spawns), spawns)
	}
	if spawns[0].AgentType != "Explore" || spawns[0].Description != "map the auth code" || spawns[0].ToolUseID != "toolu_a" {
		t.Errorf("spawn metadata = %+v", spawns[0])
	}
	if len(stops) != 1 || stops[0].ToolUseID != "toolu_a" {
		t.Errorf("got %d stop events, want 1 for toolu_a: %+v", len(stops), stops)
	}
}

func TestObserveFanoutKeepsCountWhenTranscriptUnreadable(t *testing.T) {
	dir := t.TempDir()
	tpath := filepath.Join(dir, "t.jsonl")
	writeLines(t, tpath, taskUse("toolu_a", "Explore", "map the auth code")) // 1 in-flight

	histDir := t.TempDir()
	sink := history.NewSink(history.Config{Enabled: true, Detail: history.DetailFull, Dir: histDir})
	rs := newReconcileState()
	sess := &state.Session{PID: 1, Agent: "claude", CWD: "/home/u/proj",
		Claude: &state.AgentInfo{SessionID: "s1", Transcript: tpath}}

	rs.observe(sink, sess, sess.Claude, time.Now(), statustune.Default())
	if sess.Claude.InFlightSubagents != 1 {
		t.Fatalf("setup: in-flight = %d, want 1", sess.Claude.InFlightSubagents)
	}

	// The transcript becomes unreadable (rotated/removed mid-session): transcript.Tasks
	// returns an error, so the count must stay at its last-known value rather than
	// flap to 0.
	sess.Claude.Transcript = filepath.Join(dir, "gone.jsonl")
	rs.observe(sink, sess, sess.Claude, time.Now(), statustune.Default())
	if sess.Claude.InFlightSubagents != 1 {
		t.Errorf("after unreadable transcript, in-flight = %d, want last-known 1 (not flapped to 0)", sess.Claude.InFlightSubagents)
	}
	sink.Close()
}

func assistantUsageModelLine(model string, in, out int64) string {
	return `{"type":"assistant","message":{"role":"assistant","model":"` + model +
		`","content":[],"usage":{"input_tokens":` + itoa(in) + `,"output_tokens":` + itoa(out) + `}}}`
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestObserveUsageEmitsOneSamplePerModel(t *testing.T) {
	dir := t.TempDir()
	tpath := filepath.Join(dir, "t.jsonl")
	writeLines(t, tpath, `{"type":"system"}`) // a baseline line so priming has something to skip past

	histDir := t.TempDir()
	sink := history.NewSink(history.Config{Enabled: true, Detail: history.DetailFull, Dir: histDir})
	rs := newReconcileState()
	sess := &state.Session{PID: 7, Agent: "claude", CWD: "/home/u/proj",
		Claude: &state.AgentInfo{SessionID: "s7", Transcript: tpath}}

	// First observe primes the usage cursor to EOF — no sample for the baseline.
	rs.observe(sink, sess, sess.Claude, time.Now(), statustune.Default())

	// Two models accrue tokens while we watch.
	f, _ := os.OpenFile(tpath, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(assistantUsageModelLine("claude-opus-4-8", 100, 40) + "\n")
	f.WriteString(assistantUsageModelLine("claude-haiku-4-5", 10, 5) + "\n")
	f.WriteString(assistantUsageModelLine("claude-opus-4-8", 20, 8) + "\n")
	f.Close()

	rs.observe(sink, sess, sess.Claude, time.Now(), statustune.Default())
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
	rs := newReconcileState()
	// pid 424242 has no ~/.claude/sessions file, so label.RawName falls to the
	// wezterm window title — the name we control here.
	sess := &state.Session{PID: 424242, Agent: "claude", CWD: "/home/u/proj",
		Wezterm: &state.WeztermInfo{WindowTitle: "first-name"},
		Claude:  &state.AgentInfo{SessionID: "s1", Transcript: tpath}}

	rs.observe(sink, sess, sess.Claude, time.Now(), statustune.Default()) // emit "first-name"
	rs.observe(sink, sess, sess.Claude, time.Now(), statustune.Default()) // unchanged → no emit

	sess.Wezterm.WindowTitle = "second-name"                              // user renamed the session
	rs.observe(sink, sess, sess.Claude, time.Now(), statustune.Default()) // emit "second-name"
	rs.observe(sink, sess, sess.Claude, time.Now(), statustune.Default()) // unchanged → no emit
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
	rs := newReconcileState()
	sess := &state.Session{PID: 1, Agent: "claude", CWD: "/home/u/proj",
		Claude: &state.AgentInfo{SessionID: "s1", Transcript: tpath}}

	// First observe primes the usage cursor to EOF — no sample for the backlog.
	rs.observe(sink, sess, sess.Claude, time.Now(), statustune.Default())

	// New usage accrues while we watch.
	f, _ := os.OpenFile(tpath, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(`{"type":"assistant","message":{"role":"assistant","content":[],"usage":{"input_tokens":120,"output_tokens":34}}}` + "\n")
	f.Close()

	rs.observe(sink, sess, sess.Claude, time.Now(), statustune.Default())
	sink.Close()

	samples := eventsOfType(readEvents(t, histDir), history.EventUsageSample)
	if len(samples) != 1 {
		t.Fatalf("got %d usage samples, want 1 (backlog primed away): %+v", len(samples), samples)
	}
	if samples[0].TokIn != 120 || samples[0].TokOut != 34 {
		t.Errorf("usage sample = %+v, want only the post-priming delta (120/34)", samples[0])
	}
}
