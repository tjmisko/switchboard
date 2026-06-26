package history

import (
	"testing"
	"time"
)

func ts(sec int) time.Time {
	return time.Date(2026, 6, 26, 14, 0, sec, 0, time.UTC)
}

func tr(pid int, sid string, sec int, from, to string, sub int) Event {
	return Event{Ts: ts(sec), Type: EventTransition, PID: pid, SessionID: sid, From: from, To: to, Subagents: sub}
}

func TestBuildSwimlanesIntervals(t *testing.T) {
	evs := []Event{
		{Ts: ts(0), Type: EventSessionStart, PID: 1, Agent: "claude", Project: "sb"},
		tr(1, "s1", 5, "idle", "working", 0),
		tr(1, "s1", 15, "working", "idle", 0),
		{Ts: ts(20), Type: EventSessionEnd, PID: 1, SessionID: "s1"},
	}
	lanes := BuildSwimlanes(evs, ts(99))
	if len(lanes) != 1 {
		t.Fatalf("got %d lanes, want 1", len(lanes))
	}
	l := lanes[0]
	if l.SessionID != "s1" || l.Project != "sb" || l.PID != 1 {
		t.Errorf("lane identity = %+v", l)
	}
	// Expect: unknown 0-5, working 5-15, idle 15-20. (Closed at session_end, not the `end` clamp.)
	want := []Interval{
		{Status: "", Start: ts(0), End: ts(5)},
		{Status: "working", Start: ts(5), End: ts(15)},
		{Status: "idle", Start: ts(15), End: ts(20)},
	}
	if len(l.Intervals) != len(want) {
		t.Fatalf("intervals = %+v, want %d", l.Intervals, len(want))
	}
	for i, iv := range l.Intervals {
		if iv.Status != want[i].Status || !iv.Start.Equal(want[i].Start) || !iv.End.Equal(want[i].End) {
			t.Errorf("interval %d = %+v, want %+v", i, iv, want[i])
		}
	}
}

func TestBuildSwimlanesClampsOpenLaneToEnd(t *testing.T) {
	evs := []Event{
		{Ts: ts(0), Type: EventSessionStart, PID: 1},
		tr(1, "s1", 5, "idle", "working", 0),
		// no session_end — still running
	}
	lanes := BuildSwimlanes(evs, ts(30))
	last := lanes[0].Intervals[len(lanes[0].Intervals)-1]
	if last.Status != "working" || !last.End.Equal(ts(30)) {
		t.Errorf("open working interval should extend to end clamp: %+v", last)
	}
}

func TestBuildSwimlanesSplitsOnPidReuse(t *testing.T) {
	evs := []Event{
		{Ts: ts(0), Type: EventSessionStart, PID: 1, SessionID: ""},
		tr(1, "first", 2, "idle", "working", 0),
		{Ts: ts(10), Type: EventSessionEnd, PID: 1, SessionID: "first"},
		{Ts: ts(11), Type: EventSessionStart, PID: 1}, // pid reused
		tr(1, "second", 12, "idle", "working", 0),
	}
	lanes := BuildSwimlanes(evs, ts(20))
	if len(lanes) != 2 {
		t.Fatalf("pid reuse should yield 2 lanes, got %d", len(lanes))
	}
	if lanes[0].SessionID != "first" || lanes[1].SessionID != "second" {
		t.Errorf("lanes mis-keyed: %q, %q", lanes[0].SessionID, lanes[1].SessionID)
	}
}

func TestBuildSwimlanesSuspendResume(t *testing.T) {
	evs := []Event{
		{Ts: ts(0), Type: EventSessionStart, PID: 1},
		tr(1, "s1", 2, "idle", "working", 0),
		{Ts: ts(5), Type: EventSuspend, PID: 1},
		{Ts: ts(9), Type: EventResume, PID: 1},
	}
	lanes := BuildSwimlanes(evs, ts(12))
	ivs := lanes[0].Intervals
	// unknown 0-2, working 2-5, suspended 5-9, working 9-12.
	if len(ivs) != 4 {
		t.Fatalf("intervals = %+v, want 4", ivs)
	}
	if ivs[2].Status != "suspended" || !ivs[2].Start.Equal(ts(5)) || !ivs[2].End.Equal(ts(9)) {
		t.Errorf("suspended span = %+v", ivs[2])
	}
	if ivs[3].Status != "working" {
		t.Errorf("resume should restore working, got %q", ivs[3].Status)
	}
}

func TestSummarizeAttentionStats(t *testing.T) {
	// One session: working 0-10 (no subagents), idle 10-15, working 15-25 (2 subagents).
	evs := []Event{
		{Ts: ts(0), Type: EventSessionStart, PID: 1},
		tr(1, "s1", 0, "", "working", 0),
		tr(1, "s1", 10, "working", "idle", 0),
		tr(1, "s1", 15, "idle", "working", 2),
		{Ts: ts(25), Type: EventSessionEnd, PID: 1, SessionID: "s1"},
	}
	s := Summarize(BuildSwimlanes(evs, ts(25)), evs)

	if got := s.ByStatus["working"]; got != 20*time.Second {
		t.Errorf("working total = %v, want 20s", got)
	}
	if got := s.ByStatus["idle"]; got != 5*time.Second {
		t.Errorf("idle total = %v, want 5s", got)
	}
	if s.AttentionPerSession != 20*time.Second {
		t.Errorf("attention B (per-session) = %v, want 20s", s.AttentionPerSession)
	}
	// C: 10s×1 (no subagents) + 10s×(1+2) = 10 + 30 = 40s.
	if s.AttentionFanout != 40*time.Second {
		t.Errorf("attention C (fanout) = %v, want 40s", s.AttentionFanout)
	}
	// Single session, so union == per-session.
	if s.AttentionUnion != 20*time.Second {
		t.Errorf("attention A (union) = %v, want 20s", s.AttentionUnion)
	}
}

func TestSummarizeUnionDisjointSumsBothAcrossGap(t *testing.T) {
	// Two sessions whose work does not overlap: a works 0-10, b works 20-30 (a 10s
	// gap between). With no overlap to dedupe, the union sums both = 20s.
	evs := []Event{
		{Ts: ts(0), Type: EventSessionStart, PID: 1},
		tr(1, "a", 0, "", "working", 0),
		tr(1, "a", 10, "working", "idle", 0),
		{Ts: ts(20), Type: EventSessionStart, PID: 2},
		tr(2, "b", 20, "", "working", 0),
		tr(2, "b", 30, "working", "idle", 0),
	}
	s := Summarize(BuildSwimlanes(evs, ts(40)), evs)
	if s.AttentionUnion != 20*time.Second {
		t.Errorf("disjoint union = %v, want 20s (both intervals summed across the gap)", s.AttentionUnion)
	}
}

func TestSummarizeUnionNestedCountsOuterOnly(t *testing.T) {
	// b's work (5-10) is fully nested inside a's (0-20). The union counts the outer
	// span only = 20s, while the per-session sum is 25s — proving the nested
	// interval is folded into the outer rather than added on top.
	evs := []Event{
		{Ts: ts(0), Type: EventSessionStart, PID: 1},
		tr(1, "a", 0, "", "working", 0),
		tr(1, "a", 20, "working", "idle", 0),
		{Ts: ts(5), Type: EventSessionStart, PID: 2},
		tr(2, "b", 5, "", "working", 0),
		tr(2, "b", 10, "working", "idle", 0),
	}
	s := Summarize(BuildSwimlanes(evs, ts(40)), evs)
	if s.AttentionUnion != 20*time.Second {
		t.Errorf("nested union = %v, want 20s (outer interval only)", s.AttentionUnion)
	}
	if s.AttentionPerSession != 25*time.Second {
		t.Errorf("per-session = %v, want 25s (the nested interval is still counted per-session)", s.AttentionPerSession)
	}
}

func TestSummarizeUnionAdjacentMergesContiguous(t *testing.T) {
	// a 0-10 and b 10-20 are exactly adjacent (b.Start == a.End). The merge treats
	// the boundary as continuous — one 0-20 span — so the union is 20s with no gap
	// and no double-counted boundary instant.
	evs := []Event{
		{Ts: ts(0), Type: EventSessionStart, PID: 1},
		tr(1, "a", 0, "", "working", 0),
		tr(1, "a", 10, "working", "idle", 0),
		{Ts: ts(10), Type: EventSessionStart, PID: 2},
		tr(2, "b", 10, "", "working", 0),
		tr(2, "b", 20, "working", "idle", 0),
	}
	s := Summarize(BuildSwimlanes(evs, ts(40)), evs)
	if s.AttentionUnion != 20*time.Second {
		t.Errorf("adjacent union = %v, want 20s (contiguous merge)", s.AttentionUnion)
	}
}

func TestAggregateTotalsSumsTokensAndCountsSpawns(t *testing.T) {
	evs := []Event{
		{Ts: ts(0), Type: EventUsageSample, TokIn: 100, TokOut: 50, TokCacheRead: 1000, TokCacheCreate: 200},
		{Ts: ts(1), Type: EventSubagentSpawn, AgentType: "Explore"},
		{Ts: ts(2), Type: EventUsageSample, TokIn: 5, TokOut: 7, TokCacheRead: 30, TokCacheCreate: 3},
		{Ts: ts(3), Type: EventSubagentSpawn, AgentType: "general-purpose"},
		{Ts: ts(4), Type: EventSubagentStop, AgentType: "Explore"}, // a stop is not a spawn
		{Ts: ts(5), Type: EventTransition, To: "working"},          // ignored by token/spawn totals
	}
	got := AggregateTotals(evs)
	want := Totals{TokIn: 105, TokOut: 57, TokCacheRead: 1030, TokCacheCreate: 203, Subagents: 2}
	if got != want {
		t.Errorf("AggregateTotals = %+v, want %+v", got, want)
	}
	if total := got.TotalTokens(); total != 1395 { // 105+57+1030+203
		t.Errorf("TotalTokens = %d, want 1395", total)
	}
}

func TestBuildSwimlanesAutoCreatesLaneFromTransition(t *testing.T) {
	// A bare transition (no preceding session_start) must still open a lane.
	evs := []Event{tr(1, "s1", 5, "idle", "working", 0)}
	lanes := BuildSwimlanes(evs, ts(20))
	if len(lanes) != 1 {
		t.Fatalf("transition without session_start should auto-create a lane, got %d", len(lanes))
	}
	ivs := lanes[0].Intervals
	if len(ivs) != 1 || ivs[0].Status != "working" || !ivs[0].Start.Equal(ts(5)) || !ivs[0].End.Equal(ts(20)) {
		t.Errorf("auto-created lane intervals = %+v, want one working 5-20", ivs)
	}
}

func TestBuildSwimlanesSortsShuffledInput(t *testing.T) {
	// Events arrive out of timestamp order; the output intervals must still be
	// chronological (BuildSwimlanes sorts before replaying).
	evs := []Event{
		tr(1, "s1", 20, "working", "idle", 0),
		{Ts: ts(0), Type: EventSessionStart, PID: 1},
		tr(1, "s1", 10, "idle", "working", 0),
	}
	lanes := BuildSwimlanes(evs, ts(30))
	ivs := lanes[0].Intervals
	// unknown 0-10, working 10-20, idle 20-30.
	if len(ivs) != 3 {
		t.Fatalf("intervals = %+v, want 3", ivs)
	}
	for i := 1; i < len(ivs); i++ {
		if ivs[i].Start.Before(ivs[i-1].Start) {
			t.Errorf("intervals not time-ordered at %d: %+v", i, ivs)
		}
	}
	if ivs[1].Status != "working" || !ivs[1].Start.Equal(ts(10)) || !ivs[1].End.Equal(ts(20)) {
		t.Errorf("middle interval = %+v, want working 10-20", ivs[1])
	}
}

func TestBuildSwimlanesSameTimestampDropsZeroLengthInterval(t *testing.T) {
	// Two transitions share a timestamp — the swallowed status must not emit a
	// zero-length or negative interval.
	evs := []Event{
		{Ts: ts(0), Type: EventSessionStart, PID: 1},
		tr(1, "s1", 10, "idle", "working", 0),
		tr(1, "s1", 10, "working", "delegating", 0), // same ts as the prior edge
	}
	lanes := BuildSwimlanes(evs, ts(20))
	for _, iv := range lanes[0].Intervals {
		if iv.Dur() <= 0 {
			t.Errorf("zero/negative-length interval emitted: %+v (all: %+v)", iv, lanes[0].Intervals)
		}
	}
	// The zero-length "working" span is dropped; delegating runs 10-20.
	last := lanes[0].Intervals[len(lanes[0].Intervals)-1]
	if last.Status != "delegating" || !last.Start.Equal(ts(10)) || !last.End.Equal(ts(20)) {
		t.Errorf("final interval = %+v, want delegating 10-20", last)
	}
}

func TestSummarizeUnionMergesParallelSessions(t *testing.T) {
	// Two sessions working with overlap: A 0-10, B 5-20. Union = 0-20 = 20s;
	// per-session sum = 10 + 15 = 25s. Union < per-session proves the merge.
	evs := []Event{
		{Ts: ts(0), Type: EventSessionStart, PID: 1},
		tr(1, "a", 0, "", "working", 0),
		tr(1, "a", 10, "working", "idle", 0),
		{Ts: ts(5), Type: EventSessionStart, PID: 2},
		tr(2, "b", 5, "", "working", 0),
		tr(2, "b", 20, "working", "idle", 0),
	}
	s := Summarize(BuildSwimlanes(evs, ts(30)), evs)
	if s.AttentionPerSession != 25*time.Second {
		t.Errorf("per-session = %v, want 25s", s.AttentionPerSession)
	}
	if s.AttentionUnion != 20*time.Second {
		t.Errorf("union = %v, want 20s (overlap counted once)", s.AttentionUnion)
	}
}

// --- v2 derive helpers ---------------------------------------------------

func focusEv(sec int, sid string) Event {
	return Event{Ts: ts(sec), Type: EventFocus, SessionID: sid}
}

func activityEv(sec int, to string) Event {
	return Event{Ts: ts(sec), Type: EventActivity, To: to}
}

func usageEv(pid, sec int, model string, in, out, cr, cc int64) Event {
	return Event{Ts: ts(sec), Type: EventUsageSample, PID: pid, Model: model,
		TokIn: in, TokOut: out, TokCacheRead: cr, TokCacheCreate: cc}
}

// --- A1 labels -----------------------------------------------------------

func TestBuildSwimlanesLabelsOverTime(t *testing.T) {
	evs := []Event{
		{Ts: ts(0), Type: EventSessionStart, PID: 1, SessionID: "s1"},
		{Ts: ts(0), Type: EventSessionLabel, PID: 1, Label: "alpha"},
		{Ts: ts(20), Type: EventSessionLabel, PID: 1, Label: "beta"},
		{Ts: ts(40), Type: EventSessionEnd, PID: 1, SessionID: "s1"},
	}
	lanes := BuildSwimlanes(evs, ts(99))
	want := []LabelSpan{
		{Label: "alpha", Start: ts(0), End: ts(20)},
		{Label: "beta", Start: ts(20), End: ts(40)},
	}
	got := lanes[0].Labels
	if len(got) != len(want) {
		t.Fatalf("labels = %+v, want %d spans", got, len(want))
	}
	for i := range want {
		if got[i].Label != want[i].Label || !got[i].Start.Equal(want[i].Start) || !got[i].End.Equal(want[i].End) {
			t.Errorf("label %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestBuildSwimlanesLabelDedupAndOpenCapsAtEnd(t *testing.T) {
	// A repeated label is ignored (keeps the original start); the final, never-
	// re-labeled span is capped at the lane end (no session_end).
	evs := []Event{
		{Ts: ts(0), Type: EventSessionStart, PID: 1, SessionID: "s1"},
		{Ts: ts(5), Type: EventSessionLabel, PID: 1, Label: "alpha"},
		{Ts: ts(10), Type: EventSessionLabel, PID: 1, Label: "alpha"}, // dup — ignored
	}
	lanes := BuildSwimlanes(evs, ts(30))
	got := lanes[0].Labels
	if len(got) != 1 {
		t.Fatalf("labels = %+v, want 1 span (dup ignored)", got)
	}
	if got[0].Label != "alpha" || !got[0].Start.Equal(ts(5)) || !got[0].End.Equal(ts(30)) {
		t.Errorf("label span = %+v, want alpha 5-30 (capped at end)", got[0])
	}
}

// --- A3 subagents --------------------------------------------------------

func TestBuildSwimlanesSubagentSpansPairAndCap(t *testing.T) {
	evs := []Event{
		{Ts: ts(0), Type: EventSessionStart, PID: 1, SessionID: "s1"},
		{Ts: ts(5), Type: EventSubagentSpawn, PID: 1, ToolUseID: "t1", AgentType: "Explore", Description: "search"},
		{Ts: ts(10), Type: EventSubagentSpawn, PID: 1, ToolUseID: "t2", AgentType: "general-purpose"},
		{Ts: ts(20), Type: EventSubagentStop, PID: 1, ToolUseID: "t1"},
		{Ts: ts(30), Type: EventSessionEnd, PID: 1, SessionID: "s1"}, // t2 still open → capped at 30
	}
	lanes := BuildSwimlanes(evs, ts(99))
	got := lanes[0].Subagents
	want := []SubagentSpan{
		{AgentType: "Explore", ToolUseID: "t1", Description: "search", Start: ts(5), End: ts(20)},
		{AgentType: "general-purpose", ToolUseID: "t2", Start: ts(10), End: ts(30)},
	}
	if len(got) != len(want) {
		t.Fatalf("subagents = %+v, want %d (sorted by start)", got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("subagent %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestBuildSwimlanesSubagentUnpairedStopDropped(t *testing.T) {
	// A stop whose tool_use_id was never spawned (spawn before the window) pairs
	// with nothing and is dropped rather than fabricating a span.
	evs := []Event{
		{Ts: ts(0), Type: EventSessionStart, PID: 1, SessionID: "s1"},
		{Ts: ts(10), Type: EventSubagentStop, PID: 1, ToolUseID: "orphan"},
		{Ts: ts(20), Type: EventSessionEnd, PID: 1, SessionID: "s1"},
	}
	lanes := BuildSwimlanes(evs, ts(99))
	if len(lanes[0].Subagents) != 0 {
		t.Errorf("orphan stop should yield no spans, got %+v", lanes[0].Subagents)
	}
}

// --- A2 per-lane cost + tokens ------------------------------------------

func TestBuildSwimlanesPerLaneCostAndTokens(t *testing.T) {
	// opus input 5/MTok: 400k → $2.00; sonnet output 15/MTok: 200k → $3.00.
	evs := []Event{
		{Ts: ts(0), Type: EventSessionStart, PID: 1, SessionID: "s1"},
		usageEv(1, 5, "claude-opus-4-8", 400_000, 0, 0, 0),
		usageEv(1, 10, "claude-sonnet-4-6", 0, 200_000, 0, 0),
		{Ts: ts(20), Type: EventSessionEnd, PID: 1, SessionID: "s1"},
	}
	lane := BuildSwimlanes(evs, ts(99))[0]
	if lane.TokIn != 400_000 || lane.TokOut != 200_000 {
		t.Errorf("per-lane tokens = in %d out %d, want 400000/200000", lane.TokIn, lane.TokOut)
	}
	if lane.TokCacheRead != 0 || lane.TokCacheCreate != 0 {
		t.Errorf("per-lane cache tokens = %d/%d, want 0/0", lane.TokCacheRead, lane.TokCacheCreate)
	}
	if !approx(lane.CostUSD, 5.0) {
		t.Errorf("per-lane cost = $%.4f, want $5.00 (2 opus + 3 sonnet)", lane.CostUSD)
	}
}

func TestAggregateTotalsCostUSD(t *testing.T) {
	evs := []Event{
		usageEv(1, 5, "claude-opus-4-8", 1_000_000, 0, 0, 0), // input 5/MTok → $5.00
		usageEv(1, 6, "unknown-model", 9_999_999, 0, 0, 0),   // unpriced → $0
	}
	tot := AggregateTotals(evs)
	if !approx(tot.CostUSD, 5.0) {
		t.Errorf("totals cost = $%.4f, want $5.00 (unknown model contributes nothing)", tot.CostUSD)
	}
}

// --- A4 plan window ------------------------------------------------------

func TestAggregatePlanWindow(t *testing.T) {
	from, to := ts(0), ts(0).Add(5*time.Hour)
	evs := []Event{
		usageEv(1, 5, "claude-opus-4-8", 1_000_000, 0, 200_000, 0), // $5 input + $0.10 cacheRead
		{Ts: ts(6), Type: EventTransition, To: "working"},          // not a usage_sample → ignored
	}
	pw := AggregatePlanWindow(evs, from, to)
	if pw.Hours != 5 {
		t.Errorf("hours = %v, want 5", pw.Hours)
	}
	if !pw.From.Equal(from) || !pw.To.Equal(to) {
		t.Errorf("bounds = %v..%v, want %v..%v", pw.From, pw.To, from, to)
	}
	if pw.TokIn != 1_000_000 || pw.TokCacheRead != 200_000 {
		t.Errorf("tokens = in %d cacheRead %d, want 1000000/200000", pw.TokIn, pw.TokCacheRead)
	}
	if !approx(pw.CostUSD, 5.10) { // 5.00 input + 0.10 cacheRead (0.5/MTok × 0.2M)
		t.Errorf("plan-window cost = $%.4f, want $5.10", pw.CostUSD)
	}
}

// --- C1 focus intervals --------------------------------------------------

func TestBuildSwimlanesFocusSpansPerSession(t *testing.T) {
	// Focus toggles s1 → s2 → s1 → none. Each lane gets only its own spans.
	evs := []Event{
		{Ts: ts(0), Type: EventSessionStart, PID: 1, SessionID: "s1"},
		{Ts: ts(0), Type: EventSessionStart, PID: 2, SessionID: "s2"},
		focusEv(5, "s1"),
		focusEv(15, "s2"),
		focusEv(25, "s1"),
		focusEv(35, ""), // focus left all agents — closes s1's span, opens nothing
	}
	lanes := BuildSwimlanes(evs, ts(50))
	byID := map[string]Swimlane{}
	for _, l := range lanes {
		byID[l.SessionID] = l
	}
	wantS1 := []FocusSpan{{Start: ts(5), End: ts(15)}, {Start: ts(25), End: ts(35)}}
	wantS2 := []FocusSpan{{Start: ts(15), End: ts(25)}}
	if !equalFocus(byID["s1"].Focus, wantS1) {
		t.Errorf("s1 focus = %+v, want %+v", byID["s1"].Focus, wantS1)
	}
	if !equalFocus(byID["s2"].Focus, wantS2) {
		t.Errorf("s2 focus = %+v, want %+v", byID["s2"].Focus, wantS2)
	}
}

func TestBuildSwimlanesFocusOpenSpanCapsAtLaneEnd(t *testing.T) {
	// Focus never leaves s1; its span caps at the lane's end (session_end).
	evs := []Event{
		{Ts: ts(0), Type: EventSessionStart, PID: 1, SessionID: "s1"},
		focusEv(5, "s1"),
		{Ts: ts(40), Type: EventSessionEnd, PID: 1, SessionID: "s1"},
	}
	lanes := BuildSwimlanes(evs, ts(99))
	want := []FocusSpan{{Start: ts(5), End: ts(40)}}
	if !equalFocus(lanes[0].Focus, want) {
		t.Errorf("focus = %+v, want %+v (capped at lane end 40, not stream end 99)", lanes[0].Focus, want)
	}
}

// --- C3 delegation metrics ----------------------------------------------

func TestSummarizeDelegationMetrics(t *testing.T) {
	// One session working 0-60 (then idle to 80). Focus on s1 over [10,30] and
	// [50,70]. User active over [0,20] and [40,75] (presumed active at start,
	// idle@20, active@40, idle@75).
	evs := []Event{
		{Ts: ts(0), Type: EventSessionStart, PID: 1, SessionID: "s1"},
		tr(1, "s1", 0, "", "working", 0),
		tr(1, "s1", 60, "working", "idle", 0),
		{Ts: ts(80), Type: EventSessionEnd, PID: 1, SessionID: "s1"},
		focusEv(10, "s1"), focusEv(30, ""), focusEv(50, "s1"), focusEv(70, ""),
		activityEv(20, "idle"), activityEv(40, "active"), activityEv(75, "idle"),
	}
	lanes := BuildSwimlanes(evs, ts(99))
	s := Summarize(lanes, evs)

	// attendedMask = focus ∧ active = [10,20] ∪ [50,70].
	// attended = agent-active(0-60) ∧ attendedMask = [10,20] ∪ [50,60] = 20s.
	if s.AttendedActive != 20*time.Second {
		t.Errorf("attended = %v, want 20s", s.AttendedActive)
	}
	// delegated = agent-active − attendedMask = [0,10] ∪ [20,50] ∪ [60,?]… = 40s.
	if s.DelegatedActive != 40*time.Second {
		t.Errorf("delegated = %v, want 40s", s.DelegatedActive)
	}
	// prompt = focus ∧ active = [10,20] ∪ [50,70] = 30s (includes [60,70] when the
	// agent was idle — at the prompt but not attending live work).
	if s.PromptActive != 30*time.Second {
		t.Errorf("prompt = %v, want 30s", s.PromptActive)
	}
	// effectiveness = 40 / (40+20) = 0.6667.
	if !approx(s.DelegationEffectiveness, 40.0/60.0) {
		t.Errorf("effectiveness = %v, want 0.6667", s.DelegationEffectiveness)
	}
	// Invariant: delegated + attended == per-session agent-active.
	if s.DelegatedActive+s.AttendedActive != s.AttentionPerSession {
		t.Errorf("delegated+attended (%v) != attention-B (%v)", s.DelegatedActive+s.AttendedActive, s.AttentionPerSession)
	}
}

func TestSummarizeDelegationDegradesWithoutFocusOrActivity(t *testing.T) {
	// No focus, no activity events: all agent-active time reads as delegated,
	// effectiveness is 1, and there is no divide-by-zero.
	evs := []Event{
		{Ts: ts(0), Type: EventSessionStart, PID: 1, SessionID: "s1"},
		tr(1, "s1", 0, "", "working", 0),
		tr(1, "s1", 30, "working", "idle", 0),
		{Ts: ts(40), Type: EventSessionEnd, PID: 1, SessionID: "s1"},
	}
	s := Summarize(BuildSwimlanes(evs, ts(99)), evs)
	if s.DelegatedActive != 30*time.Second {
		t.Errorf("delegated = %v, want 30s (all active, no attendance signal)", s.DelegatedActive)
	}
	if s.AttendedActive != 0 || s.PromptActive != 0 {
		t.Errorf("attended/prompt = %v/%v, want 0/0", s.AttendedActive, s.PromptActive)
	}
	if !approx(s.DelegationEffectiveness, 1.0) {
		t.Errorf("effectiveness = %v, want 1.0", s.DelegationEffectiveness)
	}
}

func TestSummarizeDelegationNoActiveNoDivideByZero(t *testing.T) {
	// A session that is never agent-active: denominator is zero → effectiveness 0.
	evs := []Event{
		{Ts: ts(0), Type: EventSessionStart, PID: 1, SessionID: "s1"},
		tr(1, "s1", 0, "", "idle", 0),
		{Ts: ts(40), Type: EventSessionEnd, PID: 1, SessionID: "s1"},
		focusEv(5, "s1"), activityEv(10, "idle"),
	}
	s := Summarize(BuildSwimlanes(evs, ts(99)), evs)
	if s.DelegatedActive != 0 || s.AttendedActive != 0 {
		t.Errorf("no agent-active → delegated/attended = %v/%v, want 0/0", s.DelegatedActive, s.AttendedActive)
	}
	if s.DelegationEffectiveness != 0 {
		t.Errorf("effectiveness = %v, want 0 (no active time, no divide by zero)", s.DelegationEffectiveness)
	}
}

// --- interval algebra ----------------------------------------------------

func TestIntersectSpans(t *testing.T) {
	a := []span{{ts(0), ts(10)}, {ts(20), ts(30)}}
	b := []span{{ts(5), ts(25)}}
	got := totalDur(intersectSpans(a, b))
	// [0,10]∩[5,25]=[5,10] (5s); [20,30]∩[5,25]=[20,25] (5s) → 10s.
	if got != 10*time.Second {
		t.Errorf("intersect total = %v, want 10s", got)
	}
}

func TestSubtractSpans(t *testing.T) {
	a := []span{{ts(0), ts(30)}}
	b := []span{{ts(5), ts(10)}, {ts(20), ts(25)}}
	got := subtractSpans(a, b)
	want := []span{{ts(0), ts(5)}, {ts(10), ts(20)}, {ts(25), ts(30)}}
	if len(got) != len(want) {
		t.Fatalf("subtract = %+v, want %+v", got, want)
	}
	for i := range want {
		if !got[i].start.Equal(want[i].start) || !got[i].end.Equal(want[i].end) {
			t.Errorf("subtract[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if totalDur(got) != 20*time.Second { // 5 + 10 + 5
		t.Errorf("subtract total = %v, want 20s", totalDur(got))
	}
}

func TestUserActiveSpansPresumesActiveAtStart(t *testing.T) {
	// First event is idle@20 → presumed active [0,20]; resume active@40 → idle
	// [20,40]; trailing active [40,60].
	evs := []Event{activityEv(20, "idle"), activityEv(40, "active")}
	got := userActiveSpans(evs, ts(0), ts(60))
	want := []span{{ts(0), ts(20)}, {ts(40), ts(60)}}
	if len(got) != len(want) {
		t.Fatalf("active spans = %+v, want %+v", got, want)
	}
	for i := range want {
		if !got[i].start.Equal(want[i].start) || !got[i].end.Equal(want[i].end) {
			t.Errorf("active[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if userActiveSpans(nil, ts(0), ts(60)) != nil {
		t.Errorf("no activity events should yield nil (no idle signal)")
	}
}

func approx(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}

func equalFocus(got, want []FocusSpan) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if !got[i].Start.Equal(want[i].Start) || !got[i].End.Equal(want[i].End) {
			return false
		}
	}
	return true
}
