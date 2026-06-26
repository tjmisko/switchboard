package history

import (
	"sort"
	"time"
)

// Status strings as they appear in events (kept as local literals so this stays
// a dependency-free leaf — they mirror state.Status*). "suspended" is synthetic:
// a suspend/resume pair brackets an interval of it; "" is the unknown lead-in
// before a session's first status edge.
const (
	statusWorking    = "working"
	statusDelegating = "delegating"
	statusSuspended  = "suspended"
)

// isActive reports whether a status counts as agent work for the attention
// stats: the main thread working, or delegating (a teammate working by proxy).
// Idle (your turn), permission (waiting on you), and suspended (paused) do not.
func isActive(status string) bool {
	return status == statusWorking || status == statusDelegating
}

// Interval is one colored span in a session's swimlane: a single status held
// from Start to End, with the subagent count observed when it began (the S
// dimension, sampled at the opening transition — see Summary.AttentionFanout).
type Interval struct {
	Status    string    `json:"status"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	Subagents int       `json:"subagents,omitempty"`
}

// Dur is the interval's length.
func (iv Interval) Dur() time.Duration { return iv.End.Sub(iv.Start) }

// Swimlane is one session's lane on the timeline: its identity plus the ordered
// intervals it passed through. Stacking swimlanes (each keyed by a distinct
// session) is the parallel-sessions timeline.
type Swimlane struct {
	SessionID string     `json:"session_id,omitempty"`
	PID       int        `json:"pid"`
	Agent     string     `json:"agent,omitempty"`
	Project   string     `json:"project,omitempty"`
	Start     time.Time  `json:"start"`
	End       time.Time  `json:"end"`
	Intervals []Interval `json:"intervals"`
}

// laneBuilder accumulates one session's intervals as events are replayed.
type laneBuilder struct {
	lane       Swimlane
	curStatus  string
	curStart   time.Time
	curSub     int
	preSuspend string // status to restore when a suspend resolves
}

func (b *laneBuilder) absorb(ev Event) {
	if ev.SessionID != "" {
		b.lane.SessionID = ev.SessionID
	}
	if ev.Agent != "" {
		b.lane.Agent = ev.Agent
	}
	if ev.Project != "" {
		b.lane.Project = ev.Project
	}
}

// closeInterval ends the open interval at t (appending it if non-empty) and
// reopens at t. A zero-length or backwards interval is dropped.
func (b *laneBuilder) closeInterval(t time.Time) {
	if b.curStart.IsZero() {
		b.curStart = t
		return
	}
	if t.After(b.curStart) {
		b.lane.Intervals = append(b.lane.Intervals, Interval{
			Status: b.curStatus, Start: b.curStart, End: t, Subagents: b.curSub,
		})
	}
	b.curStart = t
}

// BuildSwimlanes replays an event stream into per-session swimlanes. Events are
// grouped by pid, with a fresh lane begun at each session_start (so pid reuse
// within the window splits into distinct lanes rather than merging). A lane
// still open at the end of the stream is closed at `end` — pass `now` for a live
// day so a running session's last interval extends to the present; pass the
// window's upper bound otherwise. Events need not be pre-sorted.
func BuildSwimlanes(events []Event, end time.Time) []Swimlane {
	evs := append([]Event(nil), events...)
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].Ts.Before(evs[j].Ts) })

	open := map[int]*laneBuilder{}
	var done []Swimlane
	finish := func(b *laneBuilder, t time.Time) {
		b.closeInterval(t)
		b.lane.End = t
		done = append(done, b.lane)
	}

	for _, ev := range evs {
		b := open[ev.PID]
		switch ev.Type {
		case EventSessionStart:
			if b != nil {
				finish(b, ev.Ts)
			}
			nb := &laneBuilder{curStart: ev.Ts, lane: Swimlane{PID: ev.PID, Start: ev.Ts}}
			nb.absorb(ev)
			open[ev.PID] = nb
		case EventTransition:
			if b == nil {
				b = &laneBuilder{curStart: ev.Ts, lane: Swimlane{PID: ev.PID, Start: ev.Ts}}
				open[ev.PID] = b
			}
			b.closeInterval(ev.Ts)
			b.curStatus = ev.To
			b.curSub = ev.Subagents
			b.absorb(ev)
		case EventSuspend:
			if b == nil {
				continue
			}
			b.closeInterval(ev.Ts)
			b.preSuspend = b.curStatus
			b.curStatus = statusSuspended
			b.curSub = 0
		case EventResume:
			if b == nil {
				continue
			}
			b.closeInterval(ev.Ts)
			b.curStatus = b.preSuspend
		case EventSessionEnd:
			if b == nil {
				continue
			}
			b.absorb(ev)
			finish(b, ev.Ts)
			delete(open, ev.PID)
		}
	}
	for _, b := range open {
		finish(b, end)
	}
	sort.SliceStable(done, func(i, j int) bool {
		if !done[i].Start.Equal(done[j].Start) {
			return done[i].Start.Before(done[j].Start)
		}
		return done[i].PID < done[j].PID
	})
	return done
}

// Summary aggregates swimlanes into the headline numbers a dashboard shows. The
// three attention figures answer different questions (all are kept — see the
// usage-history plan §7.2):
//
//   - AttentionUnion (A): wall-clock during which ≥1 session was active. "How
//     long was something happening for me." ≤ real elapsed time.
//   - AttentionPerSession (B): Σ over sessions of active time. Rewards
//     parallelism (3 sessions working 1h = 3h).
//   - AttentionFanout (C): Σ active time × (1 + subagents). The total agent
//     compute directed, counting teammates. Approximate in this phase — the
//     subagent count is sampled at each opening transition, so Tasks launched
//     mid-interval (no status edge) are undercounted until Phase 4 wires
//     subagent_spawn/stop events.
type Summary struct {
	From                time.Time                `json:"from"`
	To                  time.Time                `json:"to"`
	Sessions            int                      `json:"sessions"`
	ByStatus            map[string]time.Duration `json:"by_status"`
	AttentionUnion      time.Duration            `json:"attention_union"`
	AttentionPerSession time.Duration            `json:"attention_per_session"`
	AttentionFanout     time.Duration            `json:"attention_fanout"`
}

// Summarize folds swimlanes into a Summary.
func Summarize(lanes []Swimlane) Summary {
	s := Summary{Sessions: len(lanes), ByStatus: map[string]time.Duration{}}
	var active []Interval
	for _, lane := range lanes {
		if s.From.IsZero() || (!lane.Start.IsZero() && lane.Start.Before(s.From)) {
			s.From = lane.Start
		}
		if lane.End.After(s.To) {
			s.To = lane.End
		}
		for _, iv := range lane.Intervals {
			d := iv.Dur()
			if d <= 0 {
				continue
			}
			s.ByStatus[iv.Status] += d
			if isActive(iv.Status) {
				s.AttentionPerSession += d
				s.AttentionFanout += time.Duration(int64(d) * int64(1+iv.Subagents))
				active = append(active, iv)
			}
		}
	}
	s.AttentionUnion = unionDuration(active)
	return s
}

// Totals aggregates the event-counted parts of a stream that are not derivable
// from status intervals: token usage (from usage_sample events) and the number
// of subagents launched (subagent_spawn events). It complements Summary, which
// is interval-derived.
type Totals struct {
	TokIn          int64 `json:"tok_in"`
	TokOut         int64 `json:"tok_out"`
	TokCacheRead   int64 `json:"tok_cache_read"`
	TokCacheCreate int64 `json:"tok_cache_create"`
	Subagents      int   `json:"subagents"` // subagent_spawn count
}

// TotalTokens is the grand total across all four token classes.
func (t Totals) TotalTokens() int64 {
	return t.TokIn + t.TokOut + t.TokCacheRead + t.TokCacheCreate
}

// AggregateTotals sums the usage_sample tokens and counts the subagent_spawn
// events in a stream.
func AggregateTotals(events []Event) Totals {
	var t Totals
	for _, ev := range events {
		switch ev.Type {
		case EventUsageSample:
			t.TokIn += ev.TokIn
			t.TokOut += ev.TokOut
			t.TokCacheRead += ev.TokCacheRead
			t.TokCacheCreate += ev.TokCacheCreate
		case EventSubagentSpawn:
			t.Subagents++
		}
	}
	return t
}

// unionDuration is the total wall-clock covered by the intervals, counting
// overlaps once (the merge behind AttentionUnion).
func unionDuration(intervals []Interval) time.Duration {
	if len(intervals) == 0 {
		return 0
	}
	iv := append([]Interval(nil), intervals...)
	sort.Slice(iv, func(i, j int) bool { return iv[i].Start.Before(iv[j].Start) })
	var total time.Duration
	curStart, curEnd := iv[0].Start, iv[0].End
	for _, x := range iv[1:] {
		if x.Start.After(curEnd) {
			total += curEnd.Sub(curStart)
			curStart, curEnd = x.Start, x.End
			continue
		}
		if x.End.After(curEnd) {
			curEnd = x.End
		}
	}
	return total + curEnd.Sub(curStart)
}
