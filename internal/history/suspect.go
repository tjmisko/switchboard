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

// DefaultSuspectTrailingCap is how long an UNCLOSED lane may go without emitting
// anything before the reader stops believing its trailing interval.
//
// Calibrated by replaying every day-file in the corpus (31 days, 651 lanes)
// through BuildSwimlanes at end = next local midnight and measuring the trailing
// interval of every lane that had no session_end. Those figures predate the
// switch to measuring SILENCE rather than interval length; re-running the same
// replay over 33 days confirms the two coincide on this corpus — the largest
// legitimate lane measures 2h25m25s by interval and 2h25m21s by silence, and no
// lane changes which side of the cap it falls on — so the band below still holds:
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
// Both error directions cost a wrong number, so the threshold is NOT free to err
// low. Summarize holds every total to the trusted end, so a false positive
// silently UNDERCOUNTS real work just as a false negative silently inflates it —
// there is no "it only shows a badge" side of this trade. What keeps the cap
// defensible is the empty band above: it must not go below the observed
// legitimate ceiling, since a cap of 30m — or anything under ~2h30m — would flag
// genuinely-live idle sessions, the dashboard's single most common state, and an
// overnight unattended delegation run (exactly what the dashboard exists to
// measure) legitimately shows a multi-hour trailing interval. A cap of 12h would
// clear the entire observed ghost class and leave the reported bug unfixed.
//
// The gap is measured from the last EVIDENCE the lane emitted, not from its final
// interval's start (see laneEvidence), which removes the largest false-positive
// class this cap would otherwise have to absorb on its own: a session that is
// still reporting usage is never silent, however long its current status has been
// held.
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

// suspectSubagentCapRatio is how much lower the subagent cap sits than the lane
// cap. It is DERIVED from the two calibrated constants rather than written as a
// literal 0.5, so the pair cannot drift apart: whatever a re-calibration does to
// either constant, the scaled policy keeps reproducing the same relationship the
// calibration comments above argue for.
const suspectSubagentCapRatio = float64(DefaultSuspectSubagentCap) /
	float64(DefaultSuspectTrailingCap)

// WithLaneCap rescales BOTH halves of the check around an operator-supplied lane
// cap, preserving the ratio the defaults encode. Passing
// DefaultSuspectTrailingCap therefore reproduces DefaultSuspectSubagentCap
// exactly, and the defaults are unchanged by going through here.
//
// Both halves move because the flag exists for the case where the calibration is
// simply wrong for someone's working pattern, and the two caps ask the same
// question one level apart: a session that legitimately goes quiet long enough to
// trip the lane cap is running subagents that legitimately outlive the span cap.
// Raising only the lane cap would leave that operator flagged anyway, by the half
// the flag never reached, which reads as the flag not working.
//
// The ratio is applied in float64 because the obvious integer form,
// cap*DefaultSuspectSubagentCap/DefaultSuspectTrailingCap, multiplies two
// nanosecond counts and overflows int64 for any cap past a few milliseconds.
//
// A cap of zero or less disables the check outright, both halves: the escape
// hatch has to be total, since a half-disabled check still subtracts hours from
// the totals.
func (p SuspectPolicy) WithLaneCap(laneCap time.Duration) SuspectPolicy {
	if laneCap <= 0 {
		p.LaneCap, p.SubagentCap = 0, 0
		return p
	}
	p.LaneCap = laneCap
	p.SubagentCap = time.Duration(float64(laneCap) * suspectSubagentCapRatio)
	return p
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
//
//  2. it has been SILENT for at least LaneCap — nothing routed to the lane, and no
//     new status interval opened, in that whole stretch. A ghost is not "a long
//     lane" but "a long silence the reader papered over". The 2026-07-22 twins
//     prove the distinction: pid 11569 ran 4h2m53s and was entirely legitimate,
//     because its final interval was 3m16s and a real session_end closed it.
//
//     Silence is measured from laneEvidence, not from the final interval's start,
//     because usage_sample (and label/subagent events) accrue onto a lane without
//     bounding an interval. A session holding one "working" status for six hours
//     while reporting usage throughout is loud, not silent, and must not be
//     flagged — Summarize would subtract every hour of it.
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
			// The gap is measured from the last EVIDENCE, not from the interval start.
			// They are the same instant for a ghost — nothing was ever heard from it
			// again — but a live session reporting token usage inside a long-running
			// status interval is observed well after that interval opened, and
			// measuring from the interval start would read it as hours of silence.
			since := laneEvidence(*lane, last)
			if d := lane.End.Sub(since); d >= p.LaneCap {
				lane.Suspect = true
				// SuspectSince is the last instant the session was actually observed:
				// everything from here to lane.End is the stretch. Start/End/Intervals are
				// left intact so a consumer can still compute either number.
				lane.SuspectSince = since
				lane.SuspectReason = fmt.Sprintf("unclosed lane stretched to %s: %s since the last evidence, in a %s interval, >= %s cap",
					p.Bound, roundSec(d), statusLabel(last.Status), roundSec(p.LaneCap))
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

// laneEvidence is the last instant a lane was demonstrably alive: the later of
// its final interval's start and the last event routed to it. The two differ
// only for events that accrue onto a lane without bounding an interval —
// usage_sample above all — which is exactly the signal that separates a session
// still doing work from one nothing has been heard from.
//
// Clamped to lane.End so a stray event past the caller's bound cannot produce a
// negative gap (which would read as "no silence at all" and skip the check).
func laneEvidence(lane Swimlane, last Interval) time.Time {
	since := last.Start
	if lane.lastEvidence.After(since) {
		since = lane.lastEvidence
	}
	if since.After(lane.End) {
		return lane.End
	}
	return since
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
