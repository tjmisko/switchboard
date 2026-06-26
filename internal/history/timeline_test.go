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
	s := Summarize(BuildSwimlanes(evs, ts(25)))

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
	s := Summarize(BuildSwimlanes(evs, ts(40)))
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
	s := Summarize(BuildSwimlanes(evs, ts(40)))
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
	s := Summarize(BuildSwimlanes(evs, ts(40)))
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
	s := Summarize(BuildSwimlanes(evs, ts(30)))
	if s.AttentionPerSession != 25*time.Second {
		t.Errorf("per-session = %v, want 25s", s.AttentionPerSession)
	}
	if s.AttentionUnion != 20*time.Second {
		t.Errorf("union = %v, want 20s (overlap counted once)", s.AttentionUnion)
	}
}
