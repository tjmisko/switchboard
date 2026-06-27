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
	statusDormant    = "dormant"
)

// Activity stream values (EventActivity.To): the user's global idle/active state
// as reported by an idle daemon (idle on timeout, active on resume).
const (
	activityIdle   = "idle"
	activityActive = "active"
)

// isActive reports whether a status counts as the parent thread's own agent work
// for the attention stats: the main thread working, or delegating (a teammate
// working by proxy). Idle (your turn), permission (waiting on you), suspended
// (paused), and dormant (the parent waiting on a subagent it launched — the
// subagent carries the compute, see Summarize) do not.
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

// LabelSpan is one stretch during which a session carried a given label (its
// name). A session_label event closes the prior span and opens the next; the
// final span runs to the lane's end. The sequence is the multilabel-over-time
// history a dashboard shows on the lane. (A1.)
type LabelSpan struct {
	Label string    `json:"label"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// SubagentSpan is one launched subagent's lifetime, paired from a subagent_spawn
// and its matching subagent_stop by tool_use_id. A span still open at the lane's
// end is capped there. AgentType is minimal-safe; Description is full-tier. (A3.)
type SubagentSpan struct {
	AgentType   string    `json:"agent_type,omitempty"`
	ToolUseID   string    `json:"tool_use_id,omitempty"`
	Description string    `json:"description,omitempty"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
}

// FocusSpan is one stretch during which this session's window held OS focus,
// derived from the global focus stream (EventFocus). The next focus event to a
// different session (or to none) closes the span; an unclosed span caps at the
// lane's end. (C1.)
type FocusSpan struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// Swimlane is one session's lane on the timeline: its identity plus the ordered
// intervals it passed through. Stacking swimlanes (each keyed by a distinct
// session) is the parallel-sessions timeline. The v2 enrichments add the
// session's name history, its launched subagents, the spans it held focus, and
// its token usage + recomputed dollar cost.
type Swimlane struct {
	SessionID string `json:"session_id,omitempty"`
	PID       int    `json:"pid"`
	Agent     string `json:"agent,omitempty"`
	Project   string `json:"project,omitempty"`
	// Name is the one canonical display name for the lane, picked from its label
	// history by canonicalLaneName: the short slug you gave it (`/name`) wins over
	// the long auto-generated title, so a consumer can render a single title
	// without re-deriving it. Empty when the session was never labelled, leaving
	// the consumer to fall back to project/agent/pid.
	Name      string     `json:"name,omitempty"`
	Start     time.Time  `json:"start"`
	End       time.Time  `json:"end"`
	Intervals []Interval `json:"intervals"`

	// v2 enrichments (all omitempty — additive to the existing contract).
	Labels []LabelSpan `json:"labels,omitempty"`
	// Names is the slug-only subsequence of Labels: each stretch the session
	// carried a `/name`-style slug, in order, so a consumer can render the name
	// changing over time without the noise of the default ("Claude Code") and the
	// long auto-generated titles that Labels also records. Empty until the first
	// slug is set. Name is the last of these.
	Names     []LabelSpan    `json:"names,omitempty"`
	Subagents []SubagentSpan `json:"subagents,omitempty"`
	Focus     []FocusSpan    `json:"focus,omitempty"`

	CostUSD        float64 `json:"cost_usd,omitempty"`
	TokIn          int64   `json:"tok_in,omitempty"`
	TokOut         int64   `json:"tok_out,omitempty"`
	TokCacheRead   int64   `json:"tok_cache_read,omitempty"`
	TokCacheCreate int64   `json:"tok_cache_create,omitempty"`
}

// laneBuilder accumulates one session's intervals as events are replayed.
type laneBuilder struct {
	lane       Swimlane
	curStatus  string
	curStart   time.Time
	curSub     int
	preSuspend string // status to restore when a suspend resolves

	curLabel      string    // the session's current name (A1)
	curLabelStart time.Time // when the current label span opened

	openSubs map[string]*SubagentSpan // tool_use_id → still-running subagent (A3)
}

// newLaneBuilder opens a fresh lane at an event's instant, seeding its identity.
func newLaneBuilder(ev Event) *laneBuilder {
	b := &laneBuilder{curStart: ev.Ts, lane: Swimlane{PID: ev.PID, Start: ev.Ts}}
	b.absorb(ev)
	return b
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

// closeLabel ends the open label span at t (appending it if the session has a
// current label and the span is non-empty) and reopens the cursor at t. The
// lead-in before a session's first label has an empty curLabel and emits no span.
func (b *laneBuilder) closeLabel(t time.Time) {
	if b.curLabel != "" && !b.curLabelStart.IsZero() && t.After(b.curLabelStart) {
		b.lane.Labels = append(b.lane.Labels, LabelSpan{Label: b.curLabel, Start: b.curLabelStart, End: t})
	}
	b.curLabelStart = t
}

// BuildSwimlanes replays an event stream into per-session swimlanes. Events are
// grouped by pid. A fresh lane begins at each session_start whose pid has no lane
// currently open — so genuine pid reuse (death emits session_end, which closes
// the lane, then a new process reuses the pid) splits into distinct lanes. A
// session_start for a pid whose lane is still open is a rediscovery artifact (a
// daemon restart re-scans the running processes and re-emits session_start for
// each) and continues the existing lane rather than orphaning the live session
// into a second, label-less lane. A lane still open at the end of the stream is
// closed at `end` — pass `now` for a live day so a running session's last
// interval extends to the present; pass the window's upper bound otherwise.
// Events need not be pre-sorted.
//
// Beyond the status intervals it also derives, per lane: the session-name spans
// (session_label), the launched-subagent spans (subagent_spawn↔stop by
// tool_use_id), the accumulated token usage + recomputed cost (usage_sample),
// and the spans the session held window focus (the global focus stream).
func BuildSwimlanes(events []Event, end time.Time) []Swimlane {
	evs := append([]Event(nil), events...)
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].Ts.Before(evs[j].Ts) })

	open := map[int]*laneBuilder{}
	var done []Swimlane
	finish := func(b *laneBuilder, t time.Time) {
		b.closeInterval(t)
		b.closeLabel(t)
		for id, sp := range b.openSubs {
			sp.End = t
			b.lane.Subagents = append(b.lane.Subagents, *sp)
			delete(b.openSubs, id)
		}
		sort.SliceStable(b.lane.Subagents, func(i, j int) bool {
			return b.lane.Subagents[i].Start.Before(b.lane.Subagents[j].Start)
		})
		b.lane.End = t
		done = append(done, b.lane)
	}

	for _, ev := range evs {
		b := open[ev.PID]
		switch ev.Type {
		case EventSessionStart:
			if b != nil {
				// A session_start for a pid whose lane is still open (no
				// session_end has closed it ⇒ the process never died) is a
				// rediscovery artifact: a daemon restart re-scans the running
				// processes and re-emits session_start. Splitting here would
				// orphan the live session into a second, label-less lane, so
				// treat it as a continuation of the same lane instead.
				b.absorb(ev)
				continue
			}
			open[ev.PID] = newLaneBuilder(ev)
		case EventTransition:
			if b == nil {
				b = newLaneBuilder(ev)
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
		case EventSessionLabel:
			if b == nil {
				b = newLaneBuilder(ev)
				open[ev.PID] = b
			}
			if ev.Label == b.curLabel {
				continue // already on this name (emitter dedups; defensive)
			}
			b.closeLabel(ev.Ts)
			b.curLabel = ev.Label
			b.absorb(ev)
		case EventUsageSample:
			if b == nil {
				b = newLaneBuilder(ev)
				open[ev.PID] = b
			}
			b.lane.TokIn += ev.TokIn
			b.lane.TokOut += ev.TokOut
			b.lane.TokCacheRead += ev.TokCacheRead
			b.lane.TokCacheCreate += ev.TokCacheCreate
			b.lane.CostUSD += CostUSD(ev.Model, ev.TokIn, ev.TokOut, ev.TokCacheRead, ev.TokCacheCreate)
			b.absorb(ev)
		case EventSubagentSpawn:
			if b == nil {
				b = newLaneBuilder(ev)
				open[ev.PID] = b
			}
			if b.openSubs == nil {
				b.openSubs = map[string]*SubagentSpan{}
			}
			b.openSubs[ev.ToolUseID] = &SubagentSpan{
				AgentType: ev.AgentType, ToolUseID: ev.ToolUseID, Description: ev.Description, Start: ev.Ts,
			}
			b.absorb(ev)
		case EventSubagentStop:
			if b == nil {
				continue
			}
			if sp, ok := b.openSubs[ev.ToolUseID]; ok {
				sp.End = ev.Ts
				b.lane.Subagents = append(b.lane.Subagents, *sp)
				delete(b.openSubs, ev.ToolUseID)
			}
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

	// Focus (C1): the focus stream is global (keyed by the focused session id, not
	// pid), so it is replayed separately and attached to each lane by session id,
	// clamped to the lane's lifetime.
	focusBySession := buildFocusSpans(evs, end)
	for i := range done {
		if id := done[i].SessionID; id != "" {
			done[i].Focus = clampFocus(focusBySession[id], done[i].Start, done[i].End)
		}
		done[i].Names = slugSpans(done[i])
		done[i].Name = canonicalLaneName(done[i])
	}
	return done
}

// slugSpans is the slug-only subsequence of a lane's label spans — the stretches
// it carried a `/name`-style slug, in order. A session is typically named in
// stages: the default ("Claude Code"), then the long auto-generated title Claude
// writes to the window ("Debug agents not recording data"), then the short slug
// you set with `/name` ("debug-agents-data-recording"). Keeping just the slugs
// lets a consumer render the name as it changed without that noise. Spans are
// left as-is (not coalesced): a `/name` persists as the session's name until the
// next change, so consecutive slug spans are already distinct names.
func slugSpans(lane Swimlane) []LabelSpan {
	var out []LabelSpan
	for _, ls := range lane.Labels {
		if isSlug(ls.Label) {
			out = append(out, ls)
		}
	}
	return out
}

// canonicalLaneName picks the single best display name for a lane: the most
// recent slug (the name you chose with `/name`), else the most recent label of
// any shape, else "" (the consumer falls back to project/pid).
func canonicalLaneName(lane Swimlane) string {
	if n := len(lane.Names); n > 0 {
		return lane.Names[n-1].Label
	}
	for i := len(lane.Labels) - 1; i >= 0; i-- {
		if lane.Labels[i].Label != "" {
			return lane.Labels[i].Label
		}
	}
	return ""
}

// isSlug reports whether s looks like a `/name` slug rather than a prose title:
// non-empty, no whitespace, and only lowercase letters, digits, and the
// separators a slug uses ('-', '_', '.'). Prose titles ("Debug agents not
// recording data") have spaces and capitals and so are rejected.
func isSlug(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// buildFocusSpans replays the global focus stream into per-session spans. A focus
// event opens a span for its session that the next focus event — to a different
// session, or to none (empty SessionID) — closes; a still-open span caps at
// `end`. Consecutive events for the same session are coalesced.
func buildFocusSpans(evs []Event, end time.Time) map[string][]FocusSpan {
	out := map[string][]FocusSpan{}
	var curSession string
	var curStart time.Time
	closeCur := func(t time.Time) {
		if curSession != "" && !curStart.IsZero() && t.After(curStart) {
			out[curSession] = append(out[curSession], FocusSpan{Start: curStart, End: t})
		}
	}
	for _, ev := range evs {
		if ev.Type != EventFocus {
			continue
		}
		if ev.SessionID == curSession {
			continue // no real change in the focused window
		}
		closeCur(ev.Ts)
		curSession = ev.SessionID
		curStart = ev.Ts
	}
	closeCur(end)
	return out
}

// clampFocus restricts focus spans to [lo, hi] (a session's lifetime), dropping
// anything that falls empty after clamping.
func clampFocus(spans []FocusSpan, lo, hi time.Time) []FocusSpan {
	var out []FocusSpan
	for _, s := range spans {
		start, end := s.Start, s.End
		if !lo.IsZero() && start.Before(lo) {
			start = lo
		}
		if !hi.IsZero() && end.After(hi) {
			end = hi
		}
		if end.After(start) {
			out = append(out, FocusSpan{Start: start, End: end})
		}
	}
	return out
}

// Summary aggregates swimlanes into the headline numbers a dashboard shows. The
// three attention figures answer different questions (all are kept — see the
// usage-history plan §7.2):
//
//   - AttentionUnion (A): wall-clock during which ≥1 session was active. "How
//     long was something happening for me." ≤ real elapsed time.
//   - AttentionPerSession (B): Σ over sessions of active time. Rewards
//     parallelism (3 sessions working 1h = 3h).
//   - AttentionFanout (C): Σ over sessions of parent-own work + every subagent
//     run (parallel subagents each count). The total agent compute directed,
//     counting teammates. Derived from the real subagent_spawn/stop spans — while
//     a subagent runs the parent is "dormant" (waiting on it), so the parent's
//     overlapping time is removed from its own work and the subagent span is
//     credited instead. Compute is reattributed parent→subagent, never lost.
//
// The delegation figures (C3) split agent-active time by whether you were
// attending it — focused on that session while active at the keyboard:
//
//   - DelegatedActive: agent-active ∧ ¬(focused-on-it ∧ user-active) — true delegation.
//   - AttendedActive:  agent-active ∧  (focused-on-it ∧ user-active) — supervising.
//   - PromptActive:    focused-on-any-agent ∧ user-active — time at an agent's prompt.
//   - DelegationEffectiveness = delegated / (delegated + attended), in [0,1].
//
// They degrade gracefully: with no focus/activity data (older days or the feature
// off) all agent-active time reads as delegated and effectiveness is 1 (or 0 when
// there is no agent-active time at all — never a divide by zero).
type Summary struct {
	From                time.Time                `json:"from"`
	To                  time.Time                `json:"to"`
	Sessions            int                      `json:"sessions"`
	ByStatus            map[string]time.Duration `json:"by_status"`
	AttentionUnion      time.Duration            `json:"attention_union"`
	AttentionPerSession time.Duration            `json:"attention_per_session"`
	AttentionFanout     time.Duration            `json:"attention_fanout"`

	PromptActive            time.Duration `json:"prompt_active"`
	AttendedActive          time.Duration `json:"attended_active"`
	DelegatedActive         time.Duration `json:"delegated_active"`
	DelegationEffectiveness float64       `json:"delegation_effectiveness"`
}

// Summarize folds swimlanes into a Summary. The events stream supplies the global
// user-activity (idle/active) timeline behind the delegation metrics; pass the
// same events given to BuildSwimlanes (the per-lane focus spans come off `lanes`).
func Summarize(lanes []Swimlane, events []Event) Summary {
	s := Summary{Sessions: len(lanes), ByStatus: map[string]time.Duration{}}
	var unionActive []span
	for _, lane := range lanes {
		if s.From.IsZero() || (!lane.Start.IsZero() && lane.Start.Before(s.From)) {
			s.From = lane.Start
		}
		if lane.End.After(s.To) {
			s.To = lane.End
		}
		for _, iv := range lane.Intervals {
			if d := iv.Dur(); d > 0 {
				s.ByStatus[iv.Status] += d
			}
		}
		// Credit subagents as working compute. parentNet is the parent's own work
		// with any subagent overlap removed (that overlap reads as "dormant"), so
		// adding the subagent spans back never double-counts. This is robust to the
		// MarkDelegationDormant pass not having run — it subtracts the overlap here
		// regardless — while by_status reflects "dormant" only once that pass has.
		subs := subagentSpans(lane)
		parentNet := subtractSpans(activeSpans(lane), subs)
		var rawSubDur time.Duration
		for _, sp := range subs {
			rawSubDur += sp.end.Sub(sp.start)
		}
		s.ByStatus[statusWorking] += rawSubDur

		agentActive := mergeSpans(append(append([]span(nil), parentNet...), subs...))
		s.AttentionPerSession += totalDur(agentActive)
		// C (fanout): parent-own work + every subagent run; parallel subagents each
		// count, so this uses the raw (un-merged) subagent total.
		s.AttentionFanout += totalDur(parentNet) + rawSubDur
		unionActive = append(unionActive, agentActive...)
	}
	s.AttentionUnion = totalDur(mergeSpans(unionActive))

	// Delegation metrics (C3): user-active is a global timeline; focus and
	// agent-active are per-lane. agent-active includes the lane's subagents (they
	// are work happening on your behalf). attendedMask = focused-on-this ∧ active.
	userActive := userActiveSpans(events, s.From, s.To)
	var allFocus []span
	for _, lane := range lanes {
		agentActive := mergeSpans(append(activeSpans(lane), subagentSpans(lane)...))
		focus := focusToSpans(lane.Focus)
		allFocus = append(allFocus, focus...)
		attendedMask := intersectSpans(focus, userActive)
		s.AttendedActive += totalDur(intersectSpans(agentActive, attendedMask))
		s.DelegatedActive += totalDur(subtractSpans(agentActive, attendedMask))
	}
	s.PromptActive = totalDur(intersectSpans(allFocus, userActive))
	if denom := s.DelegatedActive + s.AttendedActive; denom > 0 {
		s.DelegationEffectiveness = float64(s.DelegatedActive) / float64(denom)
	}
	return s
}

// Totals aggregates the event-counted parts of a stream that are not derivable
// from status intervals: token usage and recomputed cost (from usage_sample
// events) and the number of subagents launched (subagent_spawn events). It
// complements Summary, which is interval-derived.
type Totals struct {
	TokIn          int64   `json:"tok_in"`
	TokOut         int64   `json:"tok_out"`
	TokCacheRead   int64   `json:"tok_cache_read"`
	TokCacheCreate int64   `json:"tok_cache_create"`
	Subagents      int     `json:"subagents"` // subagent_spawn count
	CostUSD        float64 `json:"cost_usd"`  // recomputed window total ($)
}

// TotalTokens is the grand total across all four token classes.
func (t Totals) TotalTokens() int64 {
	return t.TokIn + t.TokOut + t.TokCacheRead + t.TokCacheCreate
}

// AggregateTotals sums the usage_sample tokens, recomputes their dollar cost
// (per-sample, per-model), and counts the subagent_spawn events in a stream.
func AggregateTotals(events []Event) Totals {
	var t Totals
	for _, ev := range events {
		switch ev.Type {
		case EventUsageSample:
			t.TokIn += ev.TokIn
			t.TokOut += ev.TokOut
			t.TokCacheRead += ev.TokCacheRead
			t.TokCacheCreate += ev.TokCacheCreate
			t.CostUSD += CostUSD(ev.Model, ev.TokIn, ev.TokOut, ev.TokCacheRead, ev.TokCacheCreate)
		case EventSubagentSpawn:
			t.Subagents++
		}
	}
	return t
}

// PlanWindow is the rolling cost/token total over a recent fixed window (the
// plan's ~5-hour usage window), self-computed from usage_sample events. It is the
// dollar half of the dashboard's plan gauge; the official utilization % comes
// from a separate cached file the dashboard reads. (A4.)
type PlanWindow struct {
	Hours          float64   `json:"hours"`
	From           time.Time `json:"from"`
	To             time.Time `json:"to"`
	CostUSD        float64   `json:"cost_usd"`
	TokIn          int64     `json:"tok_in"`
	TokOut         int64     `json:"tok_out"`
	TokCacheRead   int64     `json:"tok_cache_read"`
	TokCacheCreate int64     `json:"tok_cache_create"`
}

// AggregatePlanWindow sums the usage_sample tokens and recomputes the dollar cost
// (per-sample, per-model) across `events`, tagging the [from, to] bounds and the
// window width in hours. The producer owns this pricing so the dashboard never
// duplicates it. Pass the events from ReadRange(from, to).
func AggregatePlanWindow(events []Event, from, to time.Time) PlanWindow {
	pw := PlanWindow{Hours: to.Sub(from).Hours(), From: from, To: to}
	for _, ev := range events {
		if ev.Type != EventUsageSample {
			continue
		}
		pw.TokIn += ev.TokIn
		pw.TokOut += ev.TokOut
		pw.TokCacheRead += ev.TokCacheRead
		pw.TokCacheCreate += ev.TokCacheCreate
		pw.CostUSD += CostUSD(ev.Model, ev.TokIn, ev.TokOut, ev.TokCacheRead, ev.TokCacheCreate)
	}
	return pw
}

// span is a half-open [start, end) time interval, the unit of the interval
// algebra (merge/intersect/subtract) behind the delegation metrics.
type span struct {
	start time.Time
	end   time.Time
}

// activeSpans is a lane's agent-active intervals (working/delegating) as spans.
func activeSpans(lane Swimlane) []span {
	var out []span
	for _, iv := range lane.Intervals {
		if isActive(iv.Status) && iv.End.After(iv.Start) {
			out = append(out, span{iv.Start, iv.End})
		}
	}
	return out
}

// subagentSpans is a lane's launched-subagent runs as spans (raw — parallel
// subagents are kept distinct, not merged).
func subagentSpans(lane Swimlane) []span {
	var out []span
	for _, sp := range lane.Subagents {
		if sp.End.After(sp.Start) {
			out = append(out, span{sp.Start, sp.End})
		}
	}
	return out
}

// MarkDelegationDormant reattributes each lane's "working" time that overlaps one
// of the lane's own subagent runs to "dormant": while a launched subagent does the
// work, the parent is effectively waiting on it, not computing. The subagent span
// itself is the credited working compute (see Summarize). Call this after
// BuildSwimlanes and before Summarize / JSON encoding so the swimlane intervals,
// by_status, and the attention metrics all agree. Lanes are mutated in place;
// calling it twice is a no-op (only "working" intervals are ever resliced).
func MarkDelegationDormant(lanes []Swimlane) {
	for i := range lanes {
		subs := mergeSpans(subagentSpans(lanes[i]))
		if len(subs) == 0 {
			continue
		}
		var out []Interval
		for _, iv := range lanes[i].Intervals {
			if iv.Status != statusWorking {
				out = append(out, iv)
				continue
			}
			out = append(out, splitWorkingByDormancy(iv, subs)...)
		}
		lanes[i].Intervals = out
	}
}

// splitWorkingByDormancy slices one working interval against the (merged, sorted)
// subagent spans: portions overlapping a subagent run become "dormant", the rest
// stay "working". The interval's sampled Subagents count rides along on each piece.
// Pieces are emitted in time order.
func splitWorkingByDormancy(iv Interval, subs []span) []Interval {
	var out []Interval
	emit := func(start, end time.Time, status string) {
		if end.After(start) {
			out = append(out, Interval{Status: status, Start: start, End: end, Subagents: iv.Subagents})
		}
	}
	cur := iv.Start
	for _, sp := range subs {
		os, oe := maxTime(iv.Start, sp.start), minTime(iv.End, sp.end)
		if !oe.After(os) {
			continue // this subagent does not overlap the interval
		}
		emit(cur, os, statusWorking) // parent working before the subagent
		emit(os, oe, statusDormant)  // parent waiting on the subagent
		cur = oe
	}
	emit(cur, iv.End, statusWorking) // parent working after the last subagent
	return out
}

// focusToSpans converts a lane's focus spans into the span algebra's type.
func focusToSpans(focus []FocusSpan) []span {
	var out []span
	for _, f := range focus {
		if f.End.After(f.Start) {
			out = append(out, span{f.Start, f.End})
		}
	}
	return out
}

// userActiveSpans derives the global stretches the user was at the keyboard
// (active) from the activity stream (EventActivity, To ∈ {"idle","active"}),
// bounded to [from, to]. The user is presumed active at the window start (an idle
// daemon emits "idle" on timeout and "active" on resume, so the first event is
// normally a move away from active). With no activity events there is no idle
// signal and this returns nil — callers then treat all agent work as unattended
// (graceful degradation).
func userActiveSpans(events []Event, from, to time.Time) []span {
	var acts []Event
	for _, ev := range events {
		if ev.Type == EventActivity {
			acts = append(acts, ev)
		}
	}
	if len(acts) == 0 {
		return nil
	}
	sort.SliceStable(acts, func(i, j int) bool { return acts[i].Ts.Before(acts[j].Ts) })
	var out []span
	state := activityActive
	start := from
	closeActive := func(t time.Time) {
		if state == activityActive && !start.IsZero() && t.After(start) {
			out = append(out, span{start, t})
		}
	}
	for _, ev := range acts {
		if ev.To == state {
			continue
		}
		closeActive(ev.Ts)
		state = ev.To
		start = ev.Ts
	}
	if !to.IsZero() {
		closeActive(to)
	}
	return mergeSpans(out)
}

// ActivitySpan is one stretch of the global user-activity timeline: the user was
// State ("active" or "idle") for [Start, End]. Successive spans alternate state
// and tile [from, to] whenever the activity stream has any event. It is the
// public, top-level view of the same idle/active signal the delegation metrics
// consume internally (userActiveSpans) — the dashboard dims the idle stretches
// and outlines focus∧active from it. (C2.)
type ActivitySpan struct {
	State string    `json:"state"` // "active" | "idle"
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// ActivityTimeline derives the alternating global activity spans — BOTH "active"
// and "idle", unlike userActiveSpans which keeps only the active ones — from the
// activity stream (EventActivity, To ∈ {"idle","active"}), tiling [from, to]. The
// user is presumed active at the window start (an idle daemon emits "idle" on
// timeout and "active" on resume, so the first event is normally a move away from
// active). With no activity events it returns nil — there is no idle signal to
// surface, and the dashboard overlay degrades to focus-only.
func ActivityTimeline(events []Event, from, to time.Time) []ActivitySpan {
	var acts []Event
	for _, ev := range events {
		if ev.Type != EventActivity {
			continue
		}
		if ev.To != activityIdle && ev.To != activityActive {
			continue // ignore malformed states
		}
		acts = append(acts, ev)
	}
	if len(acts) == 0 {
		return nil
	}
	sort.SliceStable(acts, func(i, j int) bool { return acts[i].Ts.Before(acts[j].Ts) })
	var out []ActivitySpan
	state := activityActive
	start := from
	emit := func(t time.Time) {
		if !start.IsZero() && t.After(start) {
			out = append(out, ActivitySpan{State: state, Start: start, End: t})
		}
	}
	for _, ev := range acts {
		if ev.To == state {
			continue // no real change in activity state
		}
		emit(ev.Ts)
		state = ev.To
		start = ev.Ts
	}
	if !to.IsZero() {
		emit(to)
	}
	return out
}

// mergeSpans returns the disjoint, sorted union of the input spans (overlapping
// or adjacent spans coalesced). Empty/backwards spans are dropped.
func mergeSpans(in []span) []span {
	if len(in) == 0 {
		return nil
	}
	s := make([]span, 0, len(in))
	for _, x := range in {
		if x.end.After(x.start) {
			s = append(s, x)
		}
	}
	if len(s) == 0 {
		return nil
	}
	sort.Slice(s, func(i, j int) bool { return s[i].start.Before(s[j].start) })
	out := []span{s[0]}
	for _, x := range s[1:] {
		last := &out[len(out)-1]
		if x.start.After(last.end) {
			out = append(out, x)
			continue
		}
		if x.end.After(last.end) {
			last.end = x.end
		}
	}
	return out
}

// intersectSpans returns the overlap of two span sets (a ∧ b). Inputs need not be
// normalized; both are merged first, then walked with two pointers.
func intersectSpans(a, b []span) []span {
	a = mergeSpans(a)
	b = mergeSpans(b)
	var out []span
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		start := maxTime(a[i].start, b[j].start)
		end := minTime(a[i].end, b[j].end)
		if end.After(start) {
			out = append(out, span{start, end})
		}
		if a[i].end.Before(b[j].end) {
			i++
		} else {
			j++
		}
	}
	return out
}

// subtractSpans returns a with the spans in b removed (a ∧ ¬b). Inputs need not
// be normalized; both are merged first.
func subtractSpans(a, b []span) []span {
	a = mergeSpans(a)
	b = mergeSpans(b)
	var out []span
	for _, x := range a {
		cur := x.start
		for _, y := range b {
			if !y.start.Before(x.end) {
				break // y and all later spans start at/after x's end
			}
			if !y.end.After(cur) {
				continue // y ends before the cursor — already past it
			}
			if y.start.After(cur) {
				out = append(out, span{cur, y.start})
			}
			cur = y.end
			if !cur.Before(x.end) {
				break // x fully consumed
			}
		}
		if cur.Before(x.end) {
			out = append(out, span{cur, x.end})
		}
	}
	return out
}

// totalDur sums span lengths. Callers pass disjoint spans (the output of
// merge/intersect/subtract), so overlaps are not double-counted.
func totalDur(spans []span) time.Duration {
	var d time.Duration
	for _, s := range spans {
		d += s.end.Sub(s.start)
	}
	return d
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
