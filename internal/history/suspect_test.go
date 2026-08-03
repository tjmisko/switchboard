package history

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

// The trailing-interval post-check, tested against the shape that motivated it:
// the 2026-07-22 episode, where three dead sessions rendered as 4½-hour bars
// because nothing ever wrote their session_end and the reader stretched each
// lane's final interval to `now`.
//
// The predicate under test is "UNCLOSED **and** a final interval past the cap",
// so every case below pairs the pathology with the legitimate shape it must not
// be confused with: a long lane that a real session_end closed, and a live idle
// session whose trailing interval is long but plausible.

// at builds a timestamp on the day of the episode.
func at(h, m, s int) time.Time {
	return time.Date(2026, 7, 22, h, m, s, 0, time.Local)
}

// ghostEvents is the primary fixture: pid 3407477 / session 4a9af989 starts at
// 10:14:50, does a few minutes of real work for a small cost, transitions to
// "working" at 10:28:09 — and is never heard from again. No session_end.
func ghostEvents() []Event {
	const pid, sid = 3407477, "4a9af989-5bdd-4757-8891-7290d50e6a90"
	return []Event{
		{Ts: at(10, 14, 50), Type: EventSessionStart, PID: pid, Agent: "claude", Project: "ar"},
		{Ts: at(10, 15, 3), Type: EventSessionLabel, PID: pid, SessionID: sid, Label: "debug-paused-agent-pump"},
		{Ts: at(10, 15, 3), Type: EventTransition, PID: pid, SessionID: sid, To: "working"},
		{Ts: at(10, 21, 0), Type: EventTransition, PID: pid, SessionID: sid, From: "working", To: "idle"},
		// A small cost against hours of "activity" is the ghost's telltale: minutes
		// of real work, then a stretched interval. Preserved so a future cost-rate
		// heuristic has the shape to test against.
		{Ts: at(10, 21, 0), Type: EventUsageSample, PID: pid, SessionID: sid,
			Model: "claude-sonnet-4-5", TokIn: 12_000, TokOut: 4_000},
		{Ts: at(10, 28, 9), Type: EventTransition, PID: pid, SessionID: sid, From: "idle", To: "working"},
	}
}

func TestFlagSuspectLanesPredicate(t *testing.T) {
	const twinSID = "a23b64b5-0000-4000-8000-000000000000"

	rows := []struct {
		name       string
		events     []Event
		end        time.Time
		bound      EndBound
		want       bool
		wantReason string // substring the reason must carry when flagged
	}{
		{
			name:       "should flag a lane as suspect when its final interval exceeds the cap and no session_end closed it",
			events:     ghostEvents(),
			end:        at(15, 8, 42),
			bound:      BoundNow,
			want:       true,
			wantReason: `silent 4h40m33s >= 4h0m0s cap; last status "working"`,
		},
		{
			// The ghost's real twin: the same session, resumed in a fresh pane, which
			// ran 4h2m53s and died cleanly. Long lane, short final interval, real end.
			// This is the case that fails if the predicate is written against lane
			// duration instead of the trailing interval.
			name: "should not flag a long lane when a session_end closed it",
			events: []Event{
				{Ts: at(10, 46, 1), Type: EventSessionStart, PID: 11569, Agent: "claude"},
				{Ts: at(10, 46, 5), Type: EventTransition, PID: 11569, SessionID: twinSID, To: "working"},
				{Ts: at(14, 45, 39), Type: EventTransition, PID: 11569, SessionID: twinSID, From: "working", To: "idle"},
				{Ts: at(14, 48, 54), Type: EventSessionEnd, PID: 11569, SessionID: twinSID},
			},
			end:   at(15, 8, 42),
			bound: BoundNow,
			want:  false,
		},
		{
			// The case the cap alone would get wrong: a real, attended, 8-hour session
			// whose FINAL interval is also 8 hours — a long unbroken stretch of work
			// that ended with a genuine session_end. Length is not the pathology;
			// length with no evidence behind it is. This is the row that fails if the
			// "unclosed" half of the conjunction is dropped.
			name: "should not flag an eight-hour final interval when a session_end closed the lane",
			events: []Event{
				{Ts: at(6, 0, 0), Type: EventSessionStart, PID: 606, Agent: "claude"},
				{Ts: at(6, 0, 0), Type: EventTransition, PID: 606, SessionID: "long-and-real", To: "working"},
				{Ts: at(14, 0, 0), Type: EventSessionEnd, PID: 606, SessionID: "long-and-real"},
			},
			end:   at(15, 8, 42),
			bound: BoundNow,
			want:  false,
		},
		{
			// The dashboard's single most common state. A cap low enough to flag this
			// makes the badge noise you learn to ignore — strictly worse than no badge.
			name: "should not flag a live idle session when its trailing interval is under the cap",
			events: []Event{
				{Ts: at(12, 0, 0), Type: EventSessionStart, PID: 4242, Agent: "claude"},
				{Ts: at(12, 0, 30), Type: EventTransition, PID: 4242, SessionID: "live-idle", To: "idle"},
			},
			end:   at(14, 30, 0),
			bound: BoundNow,
			want:  false,
		},
		{
			// The cap is inclusive: exactly at the threshold is suspect.
			name: "should flag a lane when its final interval lands exactly on the cap",
			events: []Event{
				{Ts: at(10, 0, 0), Type: EventSessionStart, PID: 7, Agent: "claude"},
				{Ts: at(10, 0, 0), Type: EventTransition, PID: 7, SessionID: "on-cap", To: "working"},
			},
			end:        at(14, 0, 0),
			bound:      BoundNow,
			want:       true,
			wantReason: `silent 4h0m0s >= 4h0m0s cap`,
		},
		{
			name: "should not flag a lane when its final interval falls one nanosecond short of the cap",
			events: []Event{
				{Ts: at(10, 0, 0), Type: EventSessionStart, PID: 8, Agent: "claude"},
				{Ts: at(10, 0, 0), Type: EventTransition, PID: 8, SessionID: "under-cap", To: "working"},
			},
			end:   at(14, 0, 0).Add(-time.Nanosecond),
			bound: BoundNow,
			want:  false,
		},
		{
			// The residual class still reachable today: a lone session_start whose
			// process died before any hook fired. It carries no session id and no
			// color, so the reason has to name the missing status rather than quote it.
			name: "should name the missing status when a lane ghosts before its first transition",
			events: []Event{
				{Ts: at(14, 6, 1), Type: EventSessionStart, PID: 1236334, Agent: "claude"},
			},
			end:        at(23, 59, 59),
			bound:      BoundNow,
			want:       true,
			wantReason: "last status unknown (no transition was ever recorded)",
		},
		{
			// A closed day: `end` is the next local midnight, not a wall clock. A
			// session that ran 18:00 → 02:00 has its session_end in the NEXT day's
			// file, so the first day sees a 6h trailing interval and flags it. That is
			// an accepted false positive of a day-partitioned query, and the reason
			// string has to say so — "window bound", not "now".
			name: "should attribute a flagged lane to the window bound when the query is a closed day",
			events: []Event{
				{Ts: at(18, 0, 0), Type: EventSessionStart, PID: 99, Agent: "claude"},
				{Ts: at(18, 0, 0), Type: EventTransition, PID: 99, SessionID: "cross-midnight", To: "working"},
			},
			end:        at(23, 59, 59).Add(time.Second),
			bound:      BoundWindow,
			want:       true,
			wantReason: "stretched to window bound",
		},
		{
			// The same session an hour later on the clock: 23:00 → midnight is only a
			// 1h trailing interval, so crossing midnight is not itself suspicious.
			name: "should not flag a session that crosses midnight when the first day sees less than the cap",
			events: []Event{
				{Ts: at(23, 0, 0), Type: EventSessionStart, PID: 98, Agent: "claude"},
				{Ts: at(23, 0, 0), Type: EventTransition, PID: 98, SessionID: "crosser", To: "working"},
			},
			end:   at(23, 59, 59).Add(time.Second),
			bound: BoundWindow,
			want:  false,
		},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			lanes := BuildSwimlanes(row.events, row.end)
			if len(lanes) != 1 {
				t.Fatalf("fixture built %d lanes, want exactly 1", len(lanes))
			}
			report := FlagSuspectLanes(lanes, DefaultSuspectPolicy(row.end, row.bound))

			if lanes[0].Suspect != row.want {
				t.Fatalf("Suspect = %v, want %v (reason %q, lane %v–%v)",
					lanes[0].Suspect, row.want, lanes[0].SuspectReason, lanes[0].Start, lanes[0].End)
			}
			if !row.want {
				if report.Any() {
					t.Errorf("report = %+v, want nothing flagged", report)
				}
				if lanes[0].SuspectReason != "" || !lanes[0].SuspectSince.IsZero() {
					t.Errorf("unflagged lane carries reason %q / since %v", lanes[0].SuspectReason, lanes[0].SuspectSince)
				}
				return
			}
			if !strings.Contains(lanes[0].SuspectReason, row.wantReason) {
				t.Errorf("reason = %q, want it to contain %q", lanes[0].SuspectReason, row.wantReason)
			}
			// SuspectSince is the last instant there was evidence for — the start of the
			// offending interval — and the lane itself is never truncated.
			last := lanes[0].Intervals[len(lanes[0].Intervals)-1]
			if !lanes[0].SuspectSince.Equal(last.Start) {
				t.Errorf("SuspectSince = %v, want the start of the final interval %v", lanes[0].SuspectSince, last.Start)
			}
			if !lanes[0].End.Equal(row.end) {
				t.Errorf("lane End = %v, want the raw bound %v — the check must never truncate", lanes[0].End, row.end)
			}
			if report.Lanes != 1 || report.Duration != row.end.Sub(last.Start) {
				t.Errorf("report = %+v, want 1 lane and %v of excluded time", report, row.end.Sub(last.Start))
			}
		})
	}
}

// GOLDEN STRING. The reason is an operator-facing sentence with a second
// producer: switchboard-dashboard's Arachne compiler
// (internal/arachne/compile.go) synthesizes unclosed lanes from its own inputs
// and must keep emitting the identical prefix —
//
//	unclosed lane stretched to <bound>: silent <d> >= <cap> cap
//
// so that the same condition reads the same way whichever side produced it. Only
// the daemon appends "; last status …"; Arachne's lane is a single synthesized
// "working" interval, so there the clause would be a constant and it stops at
// the prefix. Everything else in this file asserts on substrings, which is what
// lets a rewording drift past unnoticed — this test is the one that fails, and
// the divergence has to be argued here and in the other repo together.
func TestSuspectReasonGoldenString(t *testing.T) {
	rows := []struct {
		name   string
		events []Event
		end    time.Time
		want   string
	}{
		{
			// The common case: the lane showed a color, so the clause quotes it.
			name:   "should emit the exact reason when a lane ghosts after a transition",
			events: ghostEvents(),
			end:    at(15, 8, 42),
			want:   `unclosed lane stretched to now: silent 4h40m33s >= 4h0m0s cap; last status "working"`,
		},
		{
			// The lone session_start: no status to quote, so the clause carries its own
			// explanation instead. This is the shape the old phrasing could not fit —
			// the label used to land after an article it disagreed with.
			name:   "should emit the exact reason when a lane ghosts before its first transition",
			events: []Event{{Ts: at(14, 6, 1), Type: EventSessionStart, PID: 1236334, Agent: "claude"}},
			end:    at(23, 59, 59),
			want:   `unclosed lane stretched to now: silent 9h53m58s >= 4h0m0s cap; last status unknown (no transition was ever recorded)`,
		},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			lanes := BuildSwimlanes(row.events, row.end)
			FlagSuspectLanes(lanes, DefaultSuspectPolicy(row.end, BoundNow))
			if lanes[0].SuspectReason != row.want {
				t.Errorf("reason =\n\t%q\nwant\n\t%q", lanes[0].SuspectReason, row.want)
			}
		})
	}
}

// The threshold is a calibrated constant, not a magic number: pin the default so
// a change to it is a deliberate, reviewed edit rather than a silent drift.
//
// The two figures below are re-derived by
//
//	switchboard-ctl history calibrate [--dir D]
//
// which replays a real corpus and prints the two populations each cap sits
// between, the empty band between them, and how many samples the cap would get
// wrong in each direction (Calibrate, calibrate.go). This test deliberately does
// NOT call it: the analysis needs a month of somebody's activity log, which the
// repo does not carry, so the constants are pinned outright here and the command
// is the argument you run by hand before proposing to move one.
func TestSuspectCapDefaults(t *testing.T) {
	if DefaultSuspectTrailingCap != 4*time.Hour {
		t.Errorf("DefaultSuspectTrailingCap = %v, want 4h (see the calibration comment)", DefaultSuspectTrailingCap)
	}
	if DefaultSuspectSubagentCap != 2*time.Hour {
		t.Errorf("DefaultSuspectSubagentCap = %v, want 2h (see the calibration comment)", DefaultSuspectSubagentCap)
	}
}

// --suspect-cap is the operator's only knob on this check, so it has to move the
// whole check. These pin the scaling law rather than the arithmetic: the ratio is
// whatever the calibrated defaults encode, and a caller who supplies a lane cap
// gets a subagent cap in the same proportion.
func TestSuspectPolicyWithLaneCap(t *testing.T) {
	end := at(15, 0, 0)

	t.Run("should leave both caps at their calibrated defaults when given the default lane cap", func(t *testing.T) {
		p := DefaultSuspectPolicy(end, BoundNow).WithLaneCap(DefaultSuspectTrailingCap)
		if p.LaneCap != DefaultSuspectTrailingCap {
			t.Errorf("LaneCap = %v, want the default %v", p.LaneCap, DefaultSuspectTrailingCap)
		}
		if p.SubagentCap != DefaultSuspectSubagentCap {
			t.Errorf("SubagentCap = %v, want the default %v — scaling must be a no-op at the default",
				p.SubagentCap, DefaultSuspectSubagentCap)
		}
	})

	t.Run("should move both caps in proportion when given a non-default lane cap", func(t *testing.T) {
		for _, laneCap := range []time.Duration{30 * time.Minute, 10 * time.Hour, 36 * time.Hour} {
			p := DefaultSuspectPolicy(end, BoundNow).WithLaneCap(laneCap)
			if p.LaneCap != laneCap {
				t.Errorf("LaneCap = %v, want %v", p.LaneCap, laneCap)
			}
			// The same ratio the defaults sit at, so a loosened cap loosens the subagent
			// half too instead of leaving the operator flagged by the half they could
			// not reach. Compared as a ratio rather than against a recomputed duration
			// because the duration form (laneCap*subCap/laneCap) overflows int64.
			got := float64(p.SubagentCap) / float64(p.LaneCap)
			want := float64(DefaultSuspectSubagentCap) / float64(DefaultSuspectTrailingCap)
			if math.Abs(got-want) > 1e-9 {
				t.Errorf("SubagentCap/LaneCap = %v for a %v lane cap (subagent cap %v), want the default ratio %v",
					got, laneCap, p.SubagentCap, want)
			}
			if p.SubagentCap >= p.LaneCap {
				t.Errorf("SubagentCap %v >= LaneCap %v — the subagent half must stay the tighter one",
					p.SubagentCap, p.LaneCap)
			}
		}
	})

	t.Run("should carry the end bound through untouched", func(t *testing.T) {
		p := DefaultSuspectPolicy(end, BoundWindow).WithLaneCap(9 * time.Hour)
		if !p.End.Equal(end) || p.Bound != BoundWindow {
			t.Errorf("policy = %+v, want End %v / Bound %v preserved", p, end, BoundWindow)
		}
	})

	t.Run("should disable both halves when the lane cap is zero or negative", func(t *testing.T) {
		for _, laneCap := range []time.Duration{0, -time.Hour} {
			p := DefaultSuspectPolicy(end, BoundNow).WithLaneCap(laneCap)
			if p.LaneCap != 0 || p.SubagentCap != 0 {
				t.Errorf("caps = lane %v / subagent %v for %v, want both zeroed — the escape hatch must be total",
					p.LaneCap, p.SubagentCap, laneCap)
			}
			// …and the disabled policy really is a no-op on a lane the default flags.
			lanes := BuildSwimlanes(ghostEvents(), at(15, 8, 42))
			if report := FlagSuspectLanes(lanes, DefaultSuspectPolicy(at(15, 8, 42), BoundNow).WithLaneCap(laneCap)); report.Any() {
				t.Errorf("report = %+v for a %v cap, want the check disabled", report, laneCap)
			}
		}
	})
}

// The scaled subagent cap has to reach FlagSuspectLanes, not just the struct: a
// phantom span over the default 2h but under the scaled cap must come back clean.
func TestFlagSuspectLanesHonoursTheScaledSubagentCap(t *testing.T) {
	const sid = "s1"
	end := at(16, 0, 0)
	// A 3h unpaired span: suspect under the default 2h cap, plausible under the 5h
	// a 10h lane cap scales to.
	events := []Event{
		{Ts: at(10, 0, 0), Type: EventSessionStart, PID: 1, Agent: "claude"},
		{Ts: at(10, 0, 0), Type: EventTransition, PID: 1, SessionID: sid, To: "working"},
		{Ts: at(13, 0, 0), Type: EventSubagentSpawn, PID: 1, SessionID: sid, AgentID: "aphantom-0001"},
	}

	t.Run("should flag the span at the default cap", func(t *testing.T) {
		lanes := BuildSwimlanes(events, end)
		if rep := FlagSuspectLanes(lanes, DefaultSuspectPolicy(end, BoundNow)); rep.Subagents != 1 {
			t.Fatalf("report = %+v, want the 3h span flagged by the 2h default", rep)
		}
	})

	t.Run("should clear the span once a loosened lane cap scales the subagent cap past it", func(t *testing.T) {
		lanes := BuildSwimlanes(events, end)
		rep := FlagSuspectLanes(lanes, DefaultSuspectPolicy(end, BoundNow).WithLaneCap(10*time.Hour))
		if rep.Subagents != 0 {
			t.Errorf("report = %+v, want the 3h span cleared by the 5h scaled cap", rep)
		}
		if lanes[0].Subagents[0].Suspect {
			t.Errorf("span still flagged: %s", lanes[0].Subagents[0].SuspectReason)
		}
	})
}

func TestFlagSuspectLanesDisabled(t *testing.T) {
	t.Run("should flag nothing when the cap is zero", func(t *testing.T) {
		end := at(15, 8, 42)
		lanes := BuildSwimlanes(ghostEvents(), end)
		policy := DefaultSuspectPolicy(end, BoundNow)
		policy.LaneCap, policy.SubagentCap = 0, 0

		if report := FlagSuspectLanes(lanes, policy); report.Any() {
			t.Errorf("report = %+v, want the check to be a no-op when disabled", report)
		}
		if lanes[0].Suspect {
			t.Error("lane flagged with the check disabled")
		}
	})
}

// The aggregation policy: a suspect lane is still emitted in full, but the part of
// it that is inference stops inflating the headline numbers, and the amount left
// out is reported rather than quietly dropped.
func TestSummarizeExcludesTheSuspectTail(t *testing.T) {
	const sid = "s1"
	end := at(16, 0, 0)
	events := []Event{
		{Ts: at(10, 0, 0), Type: EventSessionStart, PID: 1, Agent: "claude"},
		{Ts: at(10, 10, 0), Type: EventTransition, PID: 1, SessionID: sid, To: "working"},
		{Ts: at(10, 20, 0), Type: EventTransition, PID: 1, SessionID: sid, From: "working", To: "idle"},
		{Ts: at(10, 30, 0), Type: EventTransition, PID: 1, SessionID: sid, From: "idle", To: "working"},
		// …and then nothing. The 10:30 "working" interval is stretched to 16:00.
	}

	t.Run("should count the whole stretched interval when the post-check has not run", func(t *testing.T) {
		before := Summarize(BuildSwimlanes(events, end), events)
		if want := 10*time.Minute + 5*time.Hour + 30*time.Minute; before.ByStatus["working"] != want {
			t.Errorf("working = %v, want %v — the un-checked baseline this guard exists to correct", before.ByStatus["working"], want)
		}
		if before.SuspectLanes != 0 || before.SuspectDuration != 0 {
			t.Errorf("summary reports %d suspect lanes / %v with no check run", before.SuspectLanes, before.SuspectDuration)
		}
	})

	t.Run("should exclude the suspect tail from every duration total and report what it excluded", func(t *testing.T) {
		lanes := BuildSwimlanes(events, end)
		FlagSuspectLanes(lanes, DefaultSuspectPolicy(end, BoundNow))
		after := Summarize(lanes, events)

		if want := 10 * time.Minute; after.ByStatus["working"] != want {
			t.Errorf("working = %v, want only the observed %v", after.ByStatus["working"], want)
		}
		if want := 10 * time.Minute; after.ByStatus["idle"] != want {
			t.Errorf("idle = %v, want %v", after.ByStatus["idle"], want)
		}
		if want := 10 * time.Minute; after.AttentionUnion != want || after.AttentionPerSession != want || after.AttentionFanout != want {
			t.Errorf("attention = union %v / per-session %v / fanout %v, want %v each",
				after.AttentionUnion, after.AttentionPerSession, after.AttentionFanout, want)
		}
		if want := 5*time.Hour + 30*time.Minute; after.SuspectDuration != want {
			t.Errorf("SuspectDuration = %v, want %v", after.SuspectDuration, want)
		}
		if after.SuspectLanes != 1 {
			t.Errorf("SuspectLanes = %d, want 1", after.SuspectLanes)
		}
		// The lane is annotated, never shortened: an operator must still be able to
		// see the full bar the daemon's belief produced.
		if !lanes[0].End.Equal(end) || len(lanes[0].Intervals) != 4 {
			t.Errorf("lane was mutated: End=%v intervals=%d, want the raw %v / 4", lanes[0].End, len(lanes[0].Intervals), end)
		}
		// To still brackets the lanes in full — the window drawn is not the window counted.
		if !after.To.Equal(end) {
			t.Errorf("summary.To = %v, want the lane's real end %v", after.To, end)
		}
	})
}

// Proof of life inside a long status interval.
//
// usage_sample, session_label, subagent_spawn and subagent_stop accrue onto a
// lane WITHOUT opening or closing an interval, so a session can be demonstrably
// alive well inside a status interval that never ends. The gap the check
// measures therefore runs from the last evidence, not from the interval's start
// — otherwise the single shape this tool exists to measure, a long unattended
// run that keeps reporting usage, reads as hours of silence and has its work
// subtracted from every total.
func TestFlagSuspectLanesUsesTheLastEvidenceNotTheIntervalStart(t *testing.T) {
	const pid, sid = 991, "live-0000-4000-8000-000000000000"
	// One "working" interval opening at 08:00 and never closing, with the session
	// reporting token usage every two hours inside it.
	live := func(samples ...time.Time) []Event {
		evs := []Event{
			{Ts: at(8, 0, 0), Type: EventSessionStart, PID: pid, Agent: "claude", Project: "ar"},
			{Ts: at(8, 0, 0), Type: EventTransition, PID: pid, SessionID: sid, To: "working"},
		}
		for _, ts := range samples {
			evs = append(evs, Event{Ts: ts, Type: EventUsageSample, PID: pid, SessionID: sid,
				Model: "claude-sonnet-4-5", TokIn: 1000, TokOut: 500})
		}
		return evs
	}

	t.Run("should not flag a session still reporting usage inside its final interval", func(t *testing.T) {
		end := at(13, 30, 0) // 5h30m after the interval opened — past the 4h cap
		events := live(at(9, 0, 0), at(11, 0, 0), at(13, 0, 0))
		lanes := BuildSwimlanes(events, end)
		rep := FlagSuspectLanes(lanes, DefaultSuspectPolicy(end, BoundNow))

		if lanes[0].Suspect || rep.Lanes != 0 {
			t.Fatalf("flagged a session that reported usage 30m ago: %s", lanes[0].SuspectReason)
		}
		// …and its work is credited in full, which is the number the flag would have zeroed.
		MarkDelegationDormant(lanes)
		if got, want := Summarize(lanes, events).ByStatus["working"], 5*time.Hour+30*time.Minute; got != want {
			t.Errorf("working = %v, want the observed %v", got, want)
		}
	})

	t.Run("should date the stretch from the last evidence when a session goes quiet mid-interval", func(t *testing.T) {
		// Alive at 09:00, then silent. The gap 09:00→15:00 is 6h, past the cap, so it
		// IS a ghost — but only the hours after 09:00 are inference.
		end := at(15, 0, 0)
		events := live(at(9, 0, 0))
		lanes := BuildSwimlanes(events, end)
		FlagSuspectLanes(lanes, DefaultSuspectPolicy(end, BoundNow))

		if !lanes[0].Suspect {
			t.Fatal("a session silent for 6h should be flagged")
		}
		if !lanes[0].SuspectSince.Equal(at(9, 0, 0)) {
			t.Errorf("SuspectSince = %v, want the last usage sample at 09:00 — not the 08:00 interval start",
				lanes[0].SuspectSince.Format("15:04:05"))
		}
		MarkDelegationDormant(lanes)
		// The hour it was provably working is kept; only the silence is subtracted.
		if got, want := Summarize(lanes, events).ByStatus["working"], time.Hour; got != want {
			t.Errorf("working = %v, want %v", got, want)
		}
	})

	t.Run("should still flag a ghost whose last evidence predates its final transition", func(t *testing.T) {
		// The 2026-07-22 shape: the usage sample lands at 10:21, BEFORE the 10:28
		// transition that opens the stretched interval, so the interval start is
		// still the last evidence and the detector must be unchanged.
		end := at(15, 8, 42)
		lanes := BuildSwimlanes(ghostEvents(), end)
		FlagSuspectLanes(lanes, DefaultSuspectPolicy(end, BoundNow))

		if !lanes[0].Suspect {
			t.Fatal("the primary ghost fixture must still flag")
		}
		if !lanes[0].SuspectSince.Equal(at(10, 28, 9)) {
			t.Errorf("SuspectSince = %v, want the 10:28:09 transition", lanes[0].SuspectSince.Format("15:04:05"))
		}
	})
}

// The subagent half, one level down. A spawn with no matching stop is capped at
// the lane's bound by exactly the same logic that produced the three 4h46m
// phantom bars on 2026-07-22.
func TestFlagSuspectSubagentSpans(t *testing.T) {
	const sid = "s1"
	end := at(15, 30, 0)
	events := []Event{
		{Ts: at(10, 0, 0), Type: EventSessionStart, PID: 1, Agent: "claude"},
		{Ts: at(10, 0, 0), Type: EventTransition, PID: 1, SessionID: sid, To: "working"},
		{Ts: at(10, 5, 0), Type: EventSubagentSpawn, PID: 1, SessionID: sid, AgentID: "areal-0001"},
		{Ts: at(10, 35, 0), Type: EventSubagentStop, PID: 1, SessionID: sid, AgentID: "areal-0001"},
		{Ts: at(10, 5, 0), Type: EventSubagentSpawn, PID: 1, SessionID: sid, AgentID: "aphantom-0002"},
		// …no stop for the phantom.
		{Ts: at(12, 30, 0), Type: EventTransition, PID: 1, SessionID: sid, From: "working", To: "idle"},
	}
	// The lane's own trailing interval is 3h — under the 4h lane cap — so this
	// isolates the subagent predicate from the lane predicate.
	lanes := BuildSwimlanes(events, end)
	report := FlagSuspectLanes(lanes, DefaultSuspectPolicy(end, BoundNow))

	t.Run("should flag an unpaired subagent span when it outlives the cap even though its lane is not suspect", func(t *testing.T) {
		if lanes[0].Suspect {
			t.Fatalf("lane flagged (%s); the fixture is meant to keep the lane under the lane cap", lanes[0].SuspectReason)
		}
		if report.Subagents != 1 {
			t.Fatalf("report = %+v, want exactly one flagged span", report)
		}
		byID := map[string]SubagentSpan{}
		for _, sp := range lanes[0].Subagents {
			byID[sp.AgentID] = sp
		}
		if !byID["aphantom-0002"].Suspect {
			t.Errorf("the unpaired span was not flagged: %+v", byID["aphantom-0002"])
		}
		if byID["areal-0001"].Suspect {
			t.Error("the 30m span that paired with a real subagent_stop must never be flagged")
		}
		// Flagged, not removed: the span stays on the lane for the operator to see.
		if len(lanes[0].Subagents) != 2 {
			t.Errorf("lane has %d spans, want both kept", len(lanes[0].Subagents))
		}
	})

	t.Run("should not credit a phantom subagent span as compute", func(t *testing.T) {
		s := Summarize(lanes, events)
		// Parent working 10:00–12:30 (2h30m) with 30m of it reattributed to the real
		// subagent, plus that subagent's own 30m: fanout counts 2h30m, not 2h30m plus
		// the phantom's 5h25m.
		if want := 2*time.Hour + 30*time.Minute; s.AttentionFanout != want {
			t.Errorf("AttentionFanout = %v, want %v — the phantom must not be credited", s.AttentionFanout, want)
		}
	})

	t.Run("should not reattribute the parent's work to a phantom subagent", func(t *testing.T) {
		marked := BuildSwimlanes(events, end)
		FlagSuspectLanes(marked, DefaultSuspectPolicy(end, BoundNow))
		MarkDelegationDormant(marked)
		var dormant time.Duration
		for _, iv := range marked[0].Intervals {
			if iv.Status == "dormant" {
				dormant += iv.Dur()
			}
		}
		if want := 30 * time.Minute; dormant != want {
			t.Errorf("dormant = %v, want %v (the real subagent's span only)", dormant, want)
		}
	})
}

// The 2026-07-22 amplifier, pinned as a regression: four teammates spawned on the
// original pid, one stopping there and three stopping only after the session had
// resumed on a NEW pid. Under the pid-keyed grouping the reader used at the time,
// those three stops routed to the new pid's lane, found no matching agent_id, were
// silently dropped, and their spawns were capped at the bound — three phantom
// ~4h46m bars. Session-id grouping pairs all four.
func TestBuildSwimlanesPairsSubagentsAcrossAResumedPid(t *testing.T) {
	t.Run("should pair a subagent stop to its spawn when the session resumed on a new pid", func(t *testing.T) {
		const oldPID, newPID, sid = 3407477, 11569, "4a9af989"
		end := at(15, 8, 42)
		events := []Event{
			{Ts: at(10, 14, 50), Type: EventSessionStart, PID: oldPID, Agent: "claude"},
			{Ts: at(10, 15, 0), Type: EventTransition, PID: oldPID, SessionID: sid, To: "working"},
			// Named Arachne teammates: no tool_use_id, so the pairing must go through
			// agent_id (eventAgentKey).
			{Ts: at(10, 22, 20), Type: EventSubagentSpawn, PID: oldPID, SessionID: sid, AgentID: "averify-f71-065ee92469bd5e64"},
			{Ts: at(10, 22, 29), Type: EventSubagentSpawn, PID: oldPID, SessionID: sid, AgentID: "averify-f72-1a2b3c4d5e6f7081"},
			{Ts: at(10, 22, 41), Type: EventSubagentSpawn, PID: oldPID, SessionID: sid, AgentID: "averify-f73-88343a493c7fcb76"},
			{Ts: at(10, 22, 50), Type: EventSubagentSpawn, PID: oldPID, SessionID: sid, AgentID: "averify-f74-347c6c9435d4067c"},
			// The control: stops on the pid it spawned from.
			{Ts: at(10, 23, 27), Type: EventSubagentStop, PID: oldPID, SessionID: sid, AgentID: "averify-f72-1a2b3c4d5e6f7081"},
			// The session is resumed in a fresh pane; same id, new pid.
			{Ts: at(10, 46, 5), Type: EventSessionStart, PID: newPID, SessionID: sid, Agent: "claude"},
			{Ts: at(10, 57, 52), Type: EventSubagentStop, PID: newPID, SessionID: sid, AgentID: "averify-f73-88343a493c7fcb76"},
			{Ts: at(11, 4, 12), Type: EventSubagentStop, PID: newPID, SessionID: sid, AgentID: "averify-f71-065ee92469bd5e64"},
			{Ts: at(11, 4, 12), Type: EventSubagentStop, PID: newPID, SessionID: sid, AgentID: "averify-f74-347c6c9435d4067c"},
			{Ts: at(12, 0, 0), Type: EventSessionEnd, PID: newPID, SessionID: sid},
		}

		lanes := BuildSwimlanes(events, end)
		if len(lanes) != 1 {
			t.Fatalf("got %d lanes, want the resumed session to stay one lane", len(lanes))
		}
		if n := len(lanes[0].Subagents); n != 4 {
			t.Fatalf("got %d subagent spans, want 4", n)
		}
		for _, sp := range lanes[0].Subagents {
			if d := sp.End.Sub(sp.Start); d > 42*time.Minute {
				t.Errorf("span %s ran %v — a stop across the pid change was dropped and the span capped at the bound", sp.AgentID, d)
			}
		}
		if report := FlagSuspectLanes(lanes, DefaultSuspectPolicy(end, BoundNow)); report.Any() {
			t.Errorf("report = %+v, want nothing flagged: every span paired and a session_end closed the lane", report)
		}
	})
}

// A lane closed by the NEXT session claiming its pid is bounded by observed
// evidence, not by the bound — even though no session_end was written for it. The
// flag has to key on how the lane was closed, not on comparing End against the
// bound.
func TestFlagSuspectLanesIgnoresALaneClosedByItsSuccessor(t *testing.T) {
	t.Run("should not flag a lane when the next session on its pid closed it", func(t *testing.T) {
		end := at(23, 59, 59)
		events := []Event{
			{Ts: at(6, 0, 0), Type: EventSessionStart, PID: 5, Agent: "claude"},
			{Ts: at(6, 0, 0), Type: EventTransition, PID: 5, SessionID: "first", To: "working"},
			// /clear at 14:00 mints a new session id on the same process: the first
			// session ended the instant the second wrote its first event.
			{Ts: at(14, 0, 0), Type: EventTransition, PID: 5, SessionID: "second", To: "working"},
		}
		lanes := BuildSwimlanes(events, end)
		if len(lanes) != 2 {
			t.Fatalf("got %d lanes, want 2 (sequential sessions on one pid)", len(lanes))
		}
		FlagSuspectLanes(lanes, DefaultSuspectPolicy(end, BoundWindow))
		if lanes[0].Suspect {
			t.Errorf("the 8h lane closed by its successor was flagged: %s", lanes[0].SuspectReason)
		}
		if !lanes[1].Suspect {
			t.Error("the successor lane, unclosed and stretched ~10h to the bound, should be flagged")
		}
	})
}

func TestSwimlaneJSON_shouldOmitSuspectFieldsEntirelyWhenTheLaneIsClean(t *testing.T) {
	// The --json envelope is the contract a dashboard consumes, and the check is
	// meant to be purely additive. `omitempty` does nothing for a struct value, so
	// this pins the one field that needs `omitzero`: without it every clean lane on
	// every day carries "suspect_since":"0001-01-01T00:00:00Z", which is both a
	// nonsense timestamp and truthy to a consumer testing the field for presence.
	raw, err := json.Marshal(Swimlane{SessionID: "clean", PID: 42})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"suspect", "suspect_reason", "suspect_since"} {
		if strings.Contains(string(raw), field) {
			t.Errorf("clean lane emitted %q: %s", field, raw)
		}
	}
}

func TestSwimlaneJSON_shouldEmitSuspectSinceWhenTheLaneIsFlagged(t *testing.T) {
	raw, err := json.Marshal(Swimlane{
		SessionID:    "ghost",
		Suspect:      true,
		SuspectSince: time.Date(2026, 7, 22, 10, 28, 9, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"suspect_since":"2026-07-22T10:28:09Z"`) {
		t.Errorf("flagged lane lost its suspect_since: %s", raw)
	}
}
