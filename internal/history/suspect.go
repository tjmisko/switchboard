package history

import (
	"fmt"
	"time"
)

// The trailing-interval plausibility post-check — the reader-side backstop for a
// lost `session_end` (docs/session-lifecycle-hazards.md §5, L7/L8).
//
// BuildSwimlanes closes every lane still open at end-of-stream at the caller's
// `end` bound. That close is INFERENCE: nothing in the log said the session
// stopped there. When the daemon really did miss the death — a restart orphaning
// the pidfd watch, a dropped Sink.Record, a torn line — the lane's final interval
// is stretched from the last thing actually observed all the way to the bound,
// and every duration-derived number for the day inherits the stretch. That is the
// 2026-07-22 shape: three dead sessions rendered as 4½-hour bars.
//
// This check does not repair the lane and never removes anything from it. It
// marks the lane, records the last instant there was evidence for, and lets
// Summarize hold the aggregates to that instant. The bug being fixed is bad
// inference from missing data; deleting data to fix it would be the same mistake
// pointing the other way, and would hide the daemon-side hole that a suspect lane
// is the live symptom of.

// EndBound names how a caller derived the `end` it passed to BuildSwimlanes. It
// rides into a suspect lane's reason string because the two bounds mean very
// different things to an operator: a lane stretched to `now` on a live day is a
// session the daemon still believes is running, while a lane stretched to a
// window bound is at least partly an artifact of the question — a session that
// ran across midnight legitimately has no `session_end` inside the first day's
// file, so its trailing interval is long by construction rather than by defect.
type EndBound string

const (
	BoundNow    EndBound = "now"
	BoundWindow EndBound = "window bound"
)

// DefaultSuspectTrailingCap is how long an UNCLOSED lane's final status interval
// may run before the reader stops believing it.
//
// Calibrated by replaying every day-file in the corpus (31 days, 651 lanes)
// through BuildSwimlanes at end = next local midnight and measuring the trailing
// interval of every lane that had no session_end:
//
//   - the largest LEGITIMATE trailing interval anywhere is 2h25m25s (a real idle
//     session that had burned $52.83), with 2h22m47s ($27.67) right behind it;
//     everything else legitimate is under 1h5m;
//   - the smallest GHOST is 8h57m58s in the repaired corpus, and 4h35m56s in the
//     2026-07-22 pre-repair file — the episode this check exists for.
//
// 4h sits in the empty band between those two populations: ~1.65x headroom over
// the observed legitimate maximum, and it catches all three 2026-07-22 lanes.
//
// The threshold errs LOW on purpose. Because the check only annotates, a false
// positive costs one misleading badge on a live idle lane, while a false negative
// costs a silently wrong headline number. But it must not go below the observed
// legitimate ceiling: a cap of 30m — or anything under ~2h30m — would flag
// genuinely-live idle sessions, the dashboard's single most common state, and an
// overnight unattended delegation run (exactly what the dashboard exists to
// measure) legitimately shows a multi-hour trailing interval. A cap of 12h would
// clear the entire observed ghost class and leave the reported bug unfixed.
const DefaultSuspectTrailingCap = 4 * time.Hour

// DefaultSuspectSubagentCap is the same check for a subagent span the reader had
// to cap at the end bound because no `subagent_stop` ever paired with its spawn.
// It is lower than the lane cap because a subagent is a bounded unit of work: the
// fanout Observer force-closes a quiescent one at fanout.DefaultStaleCap (30m),
// so a span still open hours later is one the observer would have closed if it
// were real.
//
// Calibrated the same way. Across the corpus, 204 properly-paired spans top out
// at 1h28m53s, while all 25 reader-capped spans are 10h4m or longer; the
// 2026-07-22 pre-repair phantoms were 4h45m52s–4h46m21s. 2h clears the real
// population by ~1.35x and sits well below every phantom observed. (Note this is
// deliberately higher than the 1h a naive 2x-the-observer-cap rule suggests —
// real spans demonstrably run past 1h.)
const DefaultSuspectSubagentCap = 2 * time.Hour

// SuspectPolicy configures FlagSuspectLanes. A zero LaneCap or SubagentCap
// disables that half of the check.
type SuspectPolicy struct {
	// End is the same bound that was passed to BuildSwimlanes, and Bound says how
	// the caller derived it. Both are reported, not re-derived: the history package
	// has no way to know whether a timestamp is a wall clock or a midnight.
	End   time.Time
	Bound EndBound

	LaneCap     time.Duration
	SubagentCap time.Duration
}

// DefaultSuspectPolicy is the calibrated policy for a given end bound.
func DefaultSuspectPolicy(end time.Time, bound EndBound) SuspectPolicy {
	return SuspectPolicy{
		End:         end,
		Bound:       bound,
		LaneCap:     DefaultSuspectTrailingCap,
		SubagentCap: DefaultSuspectSubagentCap,
	}
}

// SuspectReport is what a pass of FlagSuspectLanes flagged: how many lanes and
// subagent spans, and how much lane wall-clock was marked as inference rather
// than observation (the same figure Summarize reports as SuspectDuration).
type SuspectReport struct {
	Lanes     int
	Subagents int
	Duration  time.Duration
}

// Any reports whether anything at all was flagged.
func (r SuspectReport) Any() bool { return r.Lanes > 0 || r.Subagents > 0 }

// FlagSuspectLanes marks lanes (and subagent spans) whose length is an artifact
// of the end bound rather than of anything observed. Lanes are mutated in place;
// nothing is deleted, truncated, or reordered. Call it immediately after
// BuildSwimlanes and BEFORE MarkDelegationDormant and Summarize, so every
// consumer — the text renderer, --json, and each dashboard provider — reads the
// same flag and the same aggregates.
//
// The predicate is a conjunction, and both halves are load-bearing:
//
//  1. the lane is UNCLOSED — no session_end (and no successor session on its pid)
//     ever bounded it, so BuildSwimlanes had to close it at the caller's bound.
//     This is tracked explicitly at the close rather than by comparing the lane's
//     End against the bound, so a session that genuinely died in the same
//     nanosecond as the bound is not misread. It is what makes the check
//     zero-false-positive for every properly-closed lane, however long it ran.
//  2. its FINAL interval is at least LaneCap. A ghost is not "a long lane" — it is
//     "one enormous synthesized final interval". The 2026-07-22 twins prove the
//     distinction: pid 11569 ran 4h2m53s and was entirely legitimate, because its
//     final interval was 3m16s and a real session_end closed it.
//
// Calling it twice is idempotent for a given policy: it only ever sets flags.
func FlagSuspectLanes(lanes []Swimlane, p SuspectPolicy) SuspectReport {
	var rep SuspectReport
	if p.LaneCap <= 0 && p.SubagentCap <= 0 {
		return rep // the check is disabled entirely
	}
	for i := range lanes {
		lane := &lanes[i]
		// Only a lane the READER had to close can be suspect. A lane bounded by its
		// own session_end — or by the next session claiming its pid — ends where the
		// evidence says it ends, no matter how long that is.
		if !lane.closedByBound {
			continue
		}
		if p.LaneCap > 0 && len(lane.Intervals) > 0 {
			last := lane.Intervals[len(lane.Intervals)-1]
			if d := last.Dur(); d >= p.LaneCap {
				lane.Suspect = true
				// SuspectSince is the last instant the session was actually observed:
				// everything from here to lane.End is the stretch. Start/End/Intervals are
				// left intact so a consumer can still compute either number.
				lane.SuspectSince = last.Start
				lane.SuspectReason = fmt.Sprintf("unclosed lane stretched to %s: final %s interval %s >= %s cap",
					p.Bound, statusLabel(last.Status), roundSec(d), roundSec(p.LaneCap))
				rep.Lanes++
				rep.Duration += lane.End.Sub(lane.SuspectSince)
			}
		}
		if p.SubagentCap <= 0 {
			continue
		}
		for j := range lane.Subagents {
			sp := &lane.Subagents[j]
			// Same shape one level down: the span never saw its subagent_stop, so
			// finish() capped it at the lane's bound and its length is synthesized.
			if !sp.closedByBound {
				continue
			}
			d := sp.End.Sub(sp.Start)
			if d < p.SubagentCap {
				continue
			}
			sp.Suspect = true
			sp.SuspectReason = fmt.Sprintf("unpaired subagent stretched to %s: span %s >= %s cap",
				p.Bound, roundSec(d), roundSec(p.SubagentCap))
			rep.Subagents++
		}
	}
	return rep
}

// trustedEnd is the last instant of a lane there is evidence for: its End
// normally, or the start of the suspect trailing interval once the post-check has
// flagged one. Summarize holds every duration total to this instant, so a ghost
// stops inflating the headline numbers while the lane itself is still emitted in
// full and stays visible to the operator.
func trustedEnd(lane Swimlane) time.Time {
	if lane.Suspect && !lane.SuspectSince.IsZero() {
		return lane.SuspectSince
	}
	return lane.End
}

// statusLabel names a status for a reason string. The empty status is the lead-in
// before a session's first transition; a lane that ghosts while still in it never
// showed a color at all (a lone `session_start` and nothing else), which is a
// materially different report from a stretched "working" or "idle".
func statusLabel(status string) string {
	if status == "" {
		return `unknown-status (no transition was ever recorded)`
	}
	return `"` + status + `"`
}

// roundSec trims sub-second noise from a duration in a human-facing string.
func roundSec(d time.Duration) time.Duration { return d.Round(time.Second) }
