package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/durfmt"
	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/pricing"
	"github.com/tjmisko/switchboard/internal/projectname"
)

// planWindowHours is the width of the rolling plan-usage window (Anthropic's
// ~5-hour limit window) the --plan-window flag totals cost/tokens over.
const planWindowHours = 5

// cmdTimeline renders the activity log as a per-session swimlane view plus the
// summary stats (per-status totals and the three "hours of agent attention"
// figures). It reads the on-disk log directly — no daemon — and emits text by
// default or the full structured data with --json (the stable contract a GUI
// dashboard would consume).
//
//	switchboard-ctl timeline                       today
//	switchboard-ctl timeline --day 2026-06-20
//	switchboard-ctl timeline --since 2026-06-20 --until 2026-06-26
//	switchboard-ctl timeline --json
func cmdTimeline(args []string) {
	fs := flag.NewFlagSet("timeline", flag.ExitOnError)
	dir := fs.String("dir", history.DefaultDir(), "activity-log directory")
	day := fs.String("day", "", "single local day (YYYY-MM-DD; default today)")
	since := fs.String("since", "", "range start local day (YYYY-MM-DD)")
	until := fs.String("until", "", "range end local day, inclusive (YYYY-MM-DD)")
	width := fs.Int("width", 48, "swimlane bar width in columns")
	asJSON := fs.Bool("json", false, "emit the swimlanes + summary as JSON")
	noColor := fs.Bool("no-color", false, "disable ANSI color")
	planWindow := fs.Bool("plan-window", false, "include the rolling 5h plan_window cost/token total")
	suspectCap := fs.Duration("suspect-cap", history.DefaultSuspectTrailingCap,
		"flag a lane with no session_end silent this long; the unpaired-subagent cap scales with it (0 disables both)")
	_ = fs.Parse(args)

	from, to, label := resolveWindow(*day, *since, *until)
	// Clamp the open-interval end to now, so a running session today extends to
	// the present rather than the (future) end-of-day bound.
	now := time.Now()
	end := to
	if end.After(now) {
		end = now
	}

	events, err := history.ReadRange(*dir, from, to)
	if err != nil {
		fail("read %s: %v", *dir, err)
	}
	// The activity stream carries across midnight exactly like names do: an edge
	// is written only on change, so an operator idle since last night has no edge
	// in this window until morning. Seed the window with the carried state as a
	// synthetic edge at `from`, so ActivityTimeline and the delegation metrics
	// tile the pre-first-edge stretch with the truth instead of leaving it
	// unknown (they no longer presume "active" there — that fabricated presence).
	// A failed lookback logs and degrades to unknown rather than failing the run.
	if seed, err := history.CarriedActivityState(*dir, from); err != nil {
		log.Printf("carried activity: %v — the stretch before the window's first activity edge reads as unknown", err)
	} else if seed != "" {
		events = append(events, history.Event{Ts: from, Type: history.EventActivity, To: seed})
	}
	// Use one immutable price snapshot for every aggregate in this response so a
	// concurrent atomic cache refresh cannot mix catalog versions.
	catalogs := pricing.CachedOrBootstrapCatalogs("", now)
	lanes := history.BuildSwimlanesWithCatalogs(events, end, catalogs, now)
	// A `/name` is recorded once, when it is set, so a session named before this
	// window opened (yesterday evening, for one still running this morning) has no
	// label event inside it and would render as never-named. Back-fill each such
	// lane from the earlier day-files before anything renders or derives a name.
	// Cosmetic, so a failed lookback logs and leaves the lanes as they were built.
	if err := history.BackfillCarriedNames(*dir, lanes, from); err != nil {
		log.Printf("carried names: %v — lanes named before this window may render unnamed", err)
	}
	// Post-check the lanes for implausibly long trailing intervals BEFORE anything
	// derives a number from them, so the text renderer, --json, and every dashboard
	// provider read the same flag and the same aggregates. It annotates and never
	// deletes: a suspect lane is the live symptom of a session_end the daemon
	// missed, and hiding it would hide the hole. (docs/session-lifecycle-hazards.md §5)
	// `end` is a wall clock for any window that reaches the present — today's live
	// day, where `to` is a future midnight the clamp above pulls back to `now`, and
	// an open-ended --since range, where resolveWindow already returned `now`.
	// Every other window ends on a calendar boundary, which is a materially
	// different reason for a lane to look long. (See flagSuspectLanes.)
	liveBound := to.After(now) || (*since != "" && *until == "")
	suspect := flagSuspectLanes(lanes, end, liveBound, *suspectCap)
	agentCap := history.DefaultSuspectPolicy(end, history.BoundWindow).WithLaneCap(*suspectCap).SubagentCap
	agents := buildAgentTimeline(events, from, end, agentCap)
	// Reattribute parent "working" time that overlaps a launched subagent to
	// "dormant" (the subagent carries the compute) before summarizing or encoding,
	// so the swimlanes, by_status, and attention metrics all agree.
	history.MarkDelegationDormant(lanes)
	summary := history.Summarize(lanes, events)
	totals := history.AggregateTotalsWithCatalogs(events, catalogs, now)
	// The global user-activity timeline (idle/active), bounded to the lanes' span,
	// is surfaced top-level for the dashboard's idle-dim + focus∧active overlay.
	// nil (no activity events) marshals away under omitempty.
	activity := history.ActivityTimeline(events, summary.From, summary.To)

	// The plan window is a separate rolling [now-5h, now] read (independent of the
	// display window), priced by the producer (A4).
	var planWin *history.PlanWindow
	if *planWindow {
		pwFrom, pwTo := now.Add(-planWindowHours*time.Hour), now
		pwEvents, err := history.ReadRange(*dir, pwFrom, pwTo)
		if err != nil {
			fail("read plan window %s: %v", *dir, err)
		}
		pw := history.AggregatePlanWindowWithCatalogs(pwEvents, pwFrom, pwTo, catalogs, now)
		planWin = &pw
	}

	if *asJSON {
		// Enrich each lane with the project's pretty display name for the dashboard.
		// The history package is a dependency-light leaf and must not import
		// projectname, so the reverse abbrev->full lookup happens here, off the
		// stored abbreviation, before encoding. (#33)
		pcfg := projectname.Load()
		for i := range lanes {
			lanes[i].ProjectFull = projectname.FullForAbbrev(pcfg, lanes[i].Project)
		}
		var agentOutput *agentTimeline
		if len(agents.Roots) > 0 {
			agentOutput = &agents
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(struct {
			Window     string                 `json:"window"`
			Lanes      []history.Swimlane     `json:"lanes"`
			Summary    history.Summary        `json:"summary"`
			Totals     history.Totals         `json:"totals"`
			Activity   []history.ActivitySpan `json:"activity,omitempty"`
			PlanWindow *history.PlanWindow    `json:"plan_window,omitempty"`
			Agents     *agentTimeline         `json:"agent_timeline,omitempty"`
		}{label, lanes, summary, totals, activity, planWin, agentOutput})
		return
	}
	renderSwimlanes(os.Stdout, label, lanes, summary, totals, suspect, *width, !*noColor && isTTY(os.Stdout))
	renderAgentTimeline(os.Stdout, agents)
	if planWin != nil {
		fmt.Fprintf(os.Stdout, "\nplan window (last %gh)\n", planWin.Hours)
		renderCostLine(os.Stdout, "cost (API-equivalent)", planWin.Cost)
		fmt.Fprintf(os.Stdout, "  %-12s %s\n", "tokens", humanCount(planWin.TokIn+planWin.TokOut+planWin.TokCacheRead+planWin.TokCacheCreate))
	}
}

// agentTimeline is an additive projection of provider-neutral agent_state
// history. It intentionally does not mutate Summary: richer graph evidence must
// not silently rewrite the long-standing cost or attention formulas.
type agentTimeline struct {
	Roots   []agentRootTimeline  `json:"roots"`
	Summary agentTimelineSummary `json:"summary"`
}

type agentRootTimeline struct {
	SessionID     string              `json:"session_id,omitempty"`
	PID           int                 `json:"pid,omitempty"`
	Provider      string              `json:"provider,omitempty"`
	Nodes         []agentTimelineNode `json:"nodes"`
	AgentActivity time.Duration       `json:"agent_activity"`
	UserAttention time.Duration       `json:"user_attention"`
}

type agentTimelineNode struct {
	ThreadID       string                    `json:"thread_id"`
	ParentThreadID string                    `json:"parent_thread_id,omitempty"`
	Nickname       string                    `json:"nickname,omitempty"`
	Role           string                    `json:"role,omitempty"`
	Depth          int                       `json:"depth"`
	Runtime        agentgraph.RuntimeState   `json:"runtime"`
	AttentionState agentgraph.AttentionState `json:"attention_state"`
	Lifecycle      agentgraph.LifecycleState `json:"lifecycle"`
	Activity       []agentActivitySpan       `json:"activity,omitempty"`
	Attention      []agentAttentionSpan      `json:"attention,omitempty"`
}

type agentActivitySpan struct {
	Start         time.Time `json:"start"`
	End           time.Time `json:"end"`
	Suspect       bool      `json:"suspect,omitempty"`
	SuspectReason string    `json:"suspect_reason,omitempty"`
}

type agentAttentionSpan struct {
	Reason        agentgraph.AttentionState `json:"reason"`
	Start         time.Time                 `json:"start"`
	End           time.Time                 `json:"end"`
	Suspect       bool                      `json:"suspect,omitempty"`
	SuspectReason string                    `json:"suspect_reason,omitempty"`
}

type agentTimelineSummary struct {
	AgentActivity      time.Duration `json:"agent_activity"`
	ActivityUnion      time.Duration `json:"activity_union"`
	UserAttention      time.Duration `json:"user_attention"`
	UserAttentionUnion time.Duration `json:"user_attention_union"`
	ApprovalAttention  time.Duration `json:"approval_attention"`
	UserInputAttention time.Duration `json:"user_input_attention"`
	SuspectSpans       int           `json:"suspect_spans"`
	SuspectDuration    time.Duration `json:"suspect_duration"`
}

type agentRootKey struct {
	session string
	pid     int
}

type canonicalTimelineEdge struct {
	session, thread            string
	pid                        int
	at                         int64
	fromRuntime, toRuntime     agentgraph.RuntimeState
	fromAttention, toAttention agentgraph.AttentionState
	fromLifecycle, toLifecycle agentgraph.LifecycleState
}

type openAgentSpan struct {
	start time.Time
	open  bool
}

type agentNodeBuilder struct {
	node            agentTimelineNode
	order           int
	activity        openAgentSpan
	attention       openAgentSpan
	attentionReason agentgraph.AttentionState
}

type agentRootBuilder struct {
	root  agentRootTimeline
	nodes map[string]*agentNodeBuilder
}

// buildAgentTimeline folds only canonical child-node events. Root-thread work
// remains represented by the existing session swimlane, avoiding double
// accounting. pending/running form one continuous activity span; terminal or
// session_end evidence closes it. An open span is bounded at the query end and
// goes through the same calibrated cap used for legacy subagent spans.
func buildAgentTimeline(events []history.Event, from, end time.Time, suspectCap time.Duration) agentTimeline {
	evs := append([]history.Event(nil), events...)
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].Ts.Before(evs[j].Ts) })
	roots := make(map[agentRootKey]*agentRootBuilder)
	seen := make(map[canonicalTimelineEdge]struct{})
	rootOrder := make([]agentRootKey, 0)
	nextOrder := 0

	rootFor := func(ev history.Event) *agentRootBuilder {
		key := agentRootKey{session: ev.SessionID, pid: ev.PID}
		if ev.SessionID != "" {
			key.pid = 0
		}
		root := roots[key]
		if root == nil {
			root = &agentRootBuilder{
				root:  agentRootTimeline{SessionID: ev.SessionID, PID: ev.PID, Provider: ev.Agent},
				nodes: make(map[string]*agentNodeBuilder),
			}
			roots[key] = root
			rootOrder = append(rootOrder, key)
		}
		if root.root.PID == 0 {
			root.root.PID = ev.PID
		}
		if root.root.Provider == "" {
			root.root.Provider = ev.Agent
		}
		return root
	}

	closeRoot := func(root *agentRootBuilder, at time.Time, suspect bool) {
		for _, node := range root.nodes {
			closeAgentActivity(node, at, suspect, suspectCap)
			closeAgentAttention(node, at, suspect, suspectCap)
		}
	}

	for _, ev := range evs {
		if ev.Type == history.EventSessionEnd {
			key := agentRootKey{session: ev.SessionID, pid: ev.PID}
			if ev.SessionID != "" {
				key.pid = 0
			}
			if root := roots[key]; root != nil {
				closeRoot(root, ev.Ts, false)
			} else if ev.SessionID == "" && ev.PID != 0 {
				// Pre-identification lifecycle events carry only a pid. Close any
				// canonical root still associated with that process rather than
				// stretching its child spans to the query bound.
				for _, candidate := range roots {
					if candidate.root.PID == ev.PID {
						closeRoot(candidate, ev.Ts, false)
					}
				}
			}
			continue
		}
		if ev.Type != history.EventAgentState || ev.ThreadID == "" || ev.ParentThreadID == "" {
			continue
		}
		edge := canonicalTimelineEdge{
			session: ev.SessionID, thread: ev.ThreadID, pid: ev.PID, at: ev.Ts.UnixNano(),
			fromRuntime: ev.FromRuntime, toRuntime: ev.ToRuntime,
			fromAttention: ev.FromAttention, toAttention: ev.ToAttention,
			fromLifecycle: ev.FromLifecycle, toLifecycle: ev.ToLifecycle,
		}
		if _, duplicate := seen[edge]; duplicate {
			continue
		}
		seen[edge] = struct{}{}
		root := rootFor(ev)
		node := root.nodes[ev.ThreadID]
		if node == nil {
			node = &agentNodeBuilder{node: agentTimelineNode{
				ThreadID: ev.ThreadID, Runtime: agentgraph.RuntimeUnknown,
				AttentionState: agentgraph.AttentionNone, Lifecycle: agentgraph.LifecycleUnknown,
			}, order: nextOrder}
			nextOrder++
			root.nodes[ev.ThreadID] = node
		}
		if ev.ParentThreadID != "" {
			node.node.ParentThreadID = ev.ParentThreadID
		}
		if ev.Nickname != "" {
			node.node.Nickname = ev.Nickname
		}
		if ev.Role != "" {
			node.node.Role = ev.Role
		}

		if ev.ToRuntime != "" {
			node.node.Runtime = ev.ToRuntime
		}
		if ev.ToLifecycle != "" {
			wasLive, isLive := agentLifecycleLive(ev.FromLifecycle), agentLifecycleLive(ev.ToLifecycle)
			switch {
			case isLive && !node.activity.open:
				if wasLive {
					seedAgentSpanFromDuration(&node.activity, ev, from)
				}
				if !node.activity.open {
					node.activity = openAgentSpan{start: ev.Ts, open: true}
				}
			case wasLive && !isLive:
				seedAgentSpanFromDuration(&node.activity, ev, from)
				closeAgentActivity(node, ev.Ts, false, suspectCap)
			}
			node.node.Lifecycle = ev.ToLifecycle
		}
		if ev.ToAttention != "" {
			if ev.FromAttention != ev.ToAttention {
				if !node.attention.open && agentAttentionWaiting(ev.FromAttention) {
					seedAgentSpanFromDuration(&node.attention, ev, from)
					node.attentionReason = ev.FromAttention
				}
				if node.attention.open {
					closeAgentAttention(node, ev.Ts, false, suspectCap)
				}
				if agentAttentionWaiting(ev.ToAttention) {
					node.attention = openAgentSpan{start: ev.Ts, open: true}
					node.attentionReason = ev.ToAttention
				}
			} else if agentAttentionWaiting(ev.ToAttention) && !node.attention.open {
				seedAgentSpanFromDuration(&node.attention, ev, from)
				if !node.attention.open {
					node.attention = openAgentSpan{start: ev.Ts, open: true}
				}
				node.attentionReason = ev.ToAttention
			}
			node.node.AttentionState = ev.ToAttention
		}
	}

	for _, key := range rootOrder {
		closeRoot(roots[key], end, true)
	}

	out := agentTimeline{}
	var allActivity, allAttention []timelineSpan
	for _, key := range rootOrder {
		builder := roots[key]
		nodes := make([]*agentNodeBuilder, 0, len(builder.nodes))
		for _, node := range builder.nodes {
			node.node.Depth = agentNodeDepth(node.node.ThreadID, builder.root.SessionID, builder.nodes)
			nodes = append(nodes, node)
		}
		sort.SliceStable(nodes, func(i, j int) bool {
			if nodes[i].order != nodes[j].order {
				return nodes[i].order < nodes[j].order
			}
			return nodes[i].node.ThreadID < nodes[j].node.ThreadID
		})
		var activity, attention, approvals, userInputs []timelineSpan
		for _, builderNode := range nodes {
			node := builderNode.node
			builder.root.Nodes = append(builder.root.Nodes, node)
			for _, span := range node.Activity {
				if span.Suspect {
					out.Summary.SuspectSpans++
					out.Summary.SuspectDuration += span.End.Sub(span.Start)
					continue
				}
				d := span.End.Sub(span.Start)
				builder.root.AgentActivity += d
				out.Summary.AgentActivity += d
				activity = append(activity, timelineSpan{start: span.Start, end: span.End})
			}
			for _, span := range node.Attention {
				if span.Suspect {
					out.Summary.SuspectSpans++
					out.Summary.SuspectDuration += span.End.Sub(span.Start)
					continue
				}
				attention = append(attention, timelineSpan{start: span.Start, end: span.End})
				switch span.Reason {
				case agentgraph.AttentionApproval:
					approvals = append(approvals, timelineSpan{start: span.Start, end: span.End})
				case agentgraph.AttentionUserInput:
					userInputs = append(userInputs, timelineSpan{start: span.Start, end: span.End})
				}
			}
		}
		builder.root.UserAttention = unionTimelineDuration(attention)
		out.Summary.UserAttention += builder.root.UserAttention
		out.Summary.ApprovalAttention += unionTimelineDuration(approvals)
		out.Summary.UserInputAttention += unionTimelineDuration(userInputs)
		allActivity = append(allActivity, activity...)
		allAttention = append(allAttention, attention...)
		out.Roots = append(out.Roots, builder.root)
	}
	out.Summary.ActivityUnion = unionTimelineDuration(allActivity)
	out.Summary.UserAttentionUnion = unionTimelineDuration(allAttention)
	return out
}

func seedAgentSpanFromDuration(span *openAgentSpan, ev history.Event, windowStart time.Time) {
	if span.open || ev.DurPrevMs <= 0 {
		return
	}
	start := ev.Ts.Add(-time.Duration(ev.DurPrevMs) * time.Millisecond)
	if !windowStart.IsZero() && start.Before(windowStart) {
		start = windowStart
	}
	if start.Before(ev.Ts) {
		*span = openAgentSpan{start: start, open: true}
	}
}

func closeAgentActivity(node *agentNodeBuilder, at time.Time, inferred bool, cap time.Duration) {
	if !node.activity.open || !at.After(node.activity.start) {
		node.activity.open = false
		return
	}
	span := agentActivitySpan{Start: node.activity.start, End: at}
	if inferred && cap > 0 && at.Sub(node.activity.start) >= cap {
		span.Suspect = true
		span.SuspectReason = fmt.Sprintf("unclosed agent activity stretched to query bound: %s >= %s cap", at.Sub(node.activity.start), cap)
	}
	node.node.Activity = append(node.node.Activity, span)
	node.activity.open = false
}

func closeAgentAttention(node *agentNodeBuilder, at time.Time, inferred bool, cap time.Duration) {
	if !node.attention.open || !at.After(node.attention.start) {
		node.attention.open = false
		return
	}
	span := agentAttentionSpan{Reason: node.attentionReason, Start: node.attention.start, End: at}
	if inferred && cap > 0 && at.Sub(node.attention.start) >= cap {
		span.Suspect = true
		span.SuspectReason = fmt.Sprintf("unclosed agent attention stretched to query bound: %s >= %s cap", at.Sub(node.attention.start), cap)
	}
	node.node.Attention = append(node.node.Attention, span)
	node.attention.open = false
}

func agentAttentionWaiting(value agentgraph.AttentionState) bool {
	return value == agentgraph.AttentionApproval || value == agentgraph.AttentionUserInput
}

func agentNodeDepth(id, rootID string, nodes map[string]*agentNodeBuilder) int {
	depth := 1
	seen := map[string]struct{}{id: {}}
	parent := nodes[id].node.ParentThreadID
	for parent != "" && parent != rootID {
		if _, cycle := seen[parent]; cycle {
			break
		}
		seen[parent] = struct{}{}
		ancestor := nodes[parent]
		if ancestor == nil {
			break
		}
		depth++
		parent = ancestor.node.ParentThreadID
	}
	return depth
}

type timelineSpan struct{ start, end time.Time }

func unionTimelineDuration(spans []timelineSpan) time.Duration {
	if len(spans) == 0 {
		return 0
	}
	sorted := append([]timelineSpan(nil), spans...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].start.Equal(sorted[j].start) {
			return sorted[i].end.Before(sorted[j].end)
		}
		return sorted[i].start.Before(sorted[j].start)
	})
	start, end := sorted[0].start, sorted[0].end
	var total time.Duration
	for _, span := range sorted[1:] {
		if span.start.After(end) {
			total += end.Sub(start)
			start, end = span.start, span.end
		} else if span.end.After(end) {
			end = span.end
		}
	}
	return total + end.Sub(start)
}

func renderAgentTimeline(w io.Writer, timeline agentTimeline) {
	if len(timeline.Roots) == 0 {
		return
	}
	fmt.Fprintln(w, "\nagent timeline (canonical; separate from legacy totals)")
	for _, root := range timeline.Roots {
		fmt.Fprintf(w, "  root %-8s  activity %s · user attention %s\n", shortID(root.SessionID), durfmt.Compact(root.AgentActivity), durfmt.Compact(root.UserAttention))
		for _, node := range root.Nodes {
			label := node.Nickname
			if label == "" {
				label = node.Role
			}
			if label == "" {
				label = shortAgentID(node.ThreadID)
			}
			fmt.Fprintf(w, "    %s└─ %-16s %s · %s · %s\n", strings.Repeat("  ", max(0, node.Depth-1)), label,
				agentAxisValue(string(node.Runtime)), agentAxisValue(string(node.AttentionState)), agentAxisValue(string(node.Lifecycle)))
		}
	}
	if timeline.Summary.SuspectSpans > 0 {
		fmt.Fprintf(w, "  ! %d unclosed agent span%s excluded as suspect (%s)\n", timeline.Summary.SuspectSpans,
			plural(timeline.Summary.SuspectSpans), durfmt.Compact(timeline.Summary.SuspectDuration))
	}
}

// flagSuspectLanes runs the trailing-interval plausibility post-check and reports
// each flagged lane on stderr, so `switchboard-ctl timeline` surfaces a ghost
// without a GUI and a scripted caller sees it even when it only reads the JSON on
// stdout. A cap of 0 disables the check entirely (the escape hatch for a working
// pattern the default is miscalibrated for); any other cap tunes BOTH halves, via
// WithLaneCap, since --suspect-cap is the only knob an operator has and a pattern
// that trips the lane half trips the subagent half with it.
//
// liveBound says whether `end` is a wall clock (a window reaching the present) or
// a calendar boundary (a closed day, or a bounded --since/--until range). The
// distinction goes into every reason string: a lane stretched to a calendar
// boundary may simply have run across midnight, with its session_end sitting in
// the next day's file, which is an accepted false positive of a day-partitioned
// query rather than a symptom of a lost death.
func flagSuspectLanes(lanes []history.Swimlane, end time.Time, liveBound bool, laneCap time.Duration) history.SuspectReport {
	bound := history.BoundWindow
	if liveBound {
		bound = history.BoundNow
	}
	policy := history.DefaultSuspectPolicy(end, bound).WithLaneCap(laneCap)
	report := history.FlagSuspectLanes(lanes, policy)
	if !report.Any() {
		return report
	}
	for _, lane := range lanes {
		if lane.Suspect {
			log.Printf("suspect lane %s pid=%d (%s): %s — excluded from the attention totals",
				shortID(lane.SessionID), lane.PID, laneDisplayName(lane), lane.SuspectReason)
		}
		for _, sp := range lane.Subagents {
			if sp.Suspect {
				log.Printf("suspect subagent %s on lane %s: %s — not credited as compute",
					sp.AgentID, shortID(lane.SessionID), sp.SuspectReason)
			}
		}
	}
	return report
}

// resolveWindow turns the day/since/until flags into a [from, to) local window
// and a human label. Days are local calendar days (matching how history
// partitions its files), so a date here means the day you lived, not a UTC day
// that rolls mid-evening. Precedence: an explicit --since/--until range, else
// --day, else today. `to` is exclusive (start of the day after the last).
func resolveWindow(day, since, until string) (from, to time.Time, label string) {
	parse := func(s string) time.Time {
		t, err := time.ParseInLocation("2006-01-02", s, time.Local)
		if err != nil {
			fail("bad date %q: want YYYY-MM-DD", s)
		}
		return t
	}
	switch {
	case since != "" || until != "":
		from = parse(since)
		if since == "" {
			from = time.Time{}
		}
		end := time.Now()
		if until != "" {
			end = parse(until).AddDate(0, 0, 1)
		}
		return from, end, fmt.Sprintf("%s … %s", dayOrStar(since), dayOrStar(until))
	case day != "":
		d := parse(day)
		return d, d.AddDate(0, 0, 1), day
	default:
		now := time.Now()
		today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		return today, today.AddDate(0, 0, 1), today.Format("2006-01-02")
	}
}

func dayOrStar(s string) string {
	if s == "" {
		return "*"
	}
	return s
}

// shortID trims a session id to the 8-char prefix the swimlane rows show.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// laneDisplayName is the lane's name, falling back to its project (and then to
// its pid, so a bare-lead-in ghost is still identifiable in a log line).
func laneDisplayName(lane history.Swimlane) string {
	if lane.Name != "" {
		return lane.Name
	}
	if lane.Project != "" {
		return lane.Project
	}
	return fmt.Sprintf("pid %d", lane.PID)
}

func renderSwimlanes(w *os.File, label string, lanes []history.Swimlane, s history.Summary, totals history.Totals, suspect history.SuspectReport, width int, color bool) {
	fmt.Fprintf(w, "timeline %s  (%d session%s)\n", label, len(lanes), plural(len(lanes)))
	if len(lanes) == 0 {
		fmt.Fprintln(w, "\nno events (history may be disabled — see `history path`)")
		return
	}
	from, to := s.From, s.To
	fmt.Fprintf(w, "%s … %s\n\n", from.Local().Format("15:04 Mon 02"), to.Local().Format("15:04 Mon 02"))

	for _, lane := range lanes {
		name := lane.Name
		if name == "" {
			name = lane.Project
		}
		bar := renderBar(lane, from, to, width, color)
		// A "!" between the bar and the times marks a lane the post-check flagged:
		// its end is the window bound, not an observed session_end, so read its
		// length as an upper bound. The reasons are listed under the summary.
		mark := " "
		if lane.Suspect {
			mark = "!"
		}
		fmt.Fprintf(w, "%-20s %-8s %s%s %s–%s\n",
			truncate(name, 20), shortID(lane.SessionID), bar, mark,
			lane.Start.Local().Format("15:04"), lane.End.Local().Format("15:04"))
	}

	fmt.Fprintf(w, "\nsummary\n")
	for _, st := range statusOrder(s.ByStatus) {
		fmt.Fprintf(w, "  %-12s %s\n", statusName(st), durfmt.Compact(s.ByStatus[st]))
	}
	fmt.Fprintf(w, "  %-26s %s\n", "attention · A (union)", durfmt.Compact(s.AttentionUnion))
	fmt.Fprintf(w, "  %-26s %s\n", "attention · B (per-session)", durfmt.Compact(s.AttentionPerSession))
	fmt.Fprintf(w, "  %-26s %s\n", "attention · C (fanout-weighted)", durfmt.Compact(s.AttentionFanout))
	// Delegation split — only meaningful when there is focus/activity signal.
	if s.AttendedActive > 0 || s.PromptActive > 0 {
		fmt.Fprintf(w, "  %-26s %s\n", "delegated (you away)", durfmt.Compact(s.DelegatedActive))
		fmt.Fprintf(w, "  %-26s %s\n", "attended (you watching)", durfmt.Compact(s.AttendedActive))
		fmt.Fprintf(w, "  %-26s %.0f%%\n", "delegation effectiveness", s.DelegationEffectiveness*100)
	}
	if totals.Subagents > 0 {
		fmt.Fprintf(w, "  %-26s %d\n", "subagents launched", totals.Subagents)
	}
	if tok := totals.TotalTokens(); tok > 0 {
		fmt.Fprintf(w, "  %-26s %s  (in %s · out %s · cache %s)\n", "tokens used", humanCount(tok),
			humanCount(totals.TokIn), humanCount(totals.TokOut), humanCount(totals.TokCacheRead+totals.TokCacheCreate))
	}
	if totals.Cost != nil {
		renderCostLine(w, "cost (API-equivalent)", totals.Cost)
	}
	renderSuspect(w, lanes, s, suspect)
}

func renderCostLine(w io.Writer, label string, cost *history.CostEstimate) {
	if cost == nil || cost.APIEquivalentUSD == nil {
		fmt.Fprintf(w, "  %-26s unavailable", label)
	} else {
		fmt.Fprintf(w, "  %-26s $%.2f", label, cost.APIEquivalentUSD.Float64())
	}
	if cost != nil && cost.Status != "" {
		fmt.Fprintf(w, " (%s)", cost.Status)
	}
	if cost != nil && cost.UnpricedEvents > 0 {
		fmt.Fprintf(w, "; %d event%s unpriced", cost.UnpricedEvents, plural(int(cost.UnpricedEvents)))
	}
	_, _ = fmt.Fprintln(w)
}

// renderSuspect prints the post-check's findings under the summary: what was left
// out of the totals above, and why, one line per flagged lane. Silent on a clean
// day. The lanes themselves are always drawn in full — this section is the only
// place the operator learns that part of a bar is inference.
func renderSuspect(w *os.File, lanes []history.Swimlane, s history.Summary, suspect history.SuspectReport) {
	if !suspect.Any() {
		return
	}
	fmt.Fprintf(w, "\nsuspect (excluded from the totals above)\n")
	// A lane under the cap can still carry a phantom subagent span, so the lane
	// line is conditional the same way the span line is — "0 lanes, 0s" under a
	// heading that says something was excluded reads as a bug in the check.
	if suspect.Lanes > 0 {
		fmt.Fprintf(w, "  %-26s %d lane%s, %s\n", "unclosed past the cap",
			s.SuspectLanes, plural(s.SuspectLanes), durfmt.Compact(s.SuspectDuration))
	}
	if suspect.Subagents > 0 {
		fmt.Fprintf(w, "  %-26s %d\n", "phantom subagent spans", suspect.Subagents)
	}
	for _, lane := range lanes {
		if !lane.Suspect {
			continue
		}
		fmt.Fprintf(w, "  ! %-18s %-8s %s\n",
			truncate(laneDisplayName(lane), 18), shortID(lane.SessionID), lane.SuspectReason)
	}
}

// humanCount renders a token count compactly: 1234 → "1.2k", 4500000 → "4.5M".
func humanCount(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// renderBar paints one lane as width columns spanning [from, to], each column
// colored by the status active at its midpoint (a space where the lane is not
// live). All lanes share the same [from, to], so columns align across rows.
func renderBar(lane history.Swimlane, from, to time.Time, width int, color bool) string {
	if width <= 0 || !to.After(from) {
		return ""
	}
	span := to.Sub(from)
	var b strings.Builder
	for col := 0; col < width; col++ {
		frac := (float64(col) + 0.5) / float64(width)
		at := from.Add(time.Duration(float64(span) * frac))
		status, live := statusAt(lane, at)
		b.WriteString(block(status, live, color))
	}
	return b.String()
}

// statusAt returns the status of the interval covering t, and false when t is
// outside the lane (before it started, after it ended, or in a gap).
func statusAt(lane history.Swimlane, t time.Time) (string, bool) {
	for _, iv := range lane.Intervals {
		if !t.Before(iv.Start) && t.Before(iv.End) {
			return iv.Status, true
		}
	}
	return "", false
}

const (
	colReset  = "\033[0m"
	colGreen  = "\033[32m"
	colYellow = "\033[33m"
	colRed    = "\033[31m"
	colGrey   = "\033[90m"
)

// block renders one bar cell: a colored ▰ for a live status, a space off-lane.
func block(status string, live, colorOn bool) string {
	if !live {
		return " "
	}
	ch := "▰"
	if !colorOn {
		// Plain mode: a status initial keeps the bar legible without color.
		switch status {
		case "working", "delegating":
			return "w"
		case "dormant":
			return "d"
		case "idle":
			return "i"
		case "permission":
			return "p"
		case "suspended":
			return "z"
		default:
			return "·"
		}
	}
	var c string
	switch status {
	case "working", "delegating":
		c = colGreen
	case "idle":
		c = colYellow
	case "permission":
		c = colRed
	default:
		c = colGrey
	}
	return c + ch + colReset
}

func statusOrder(m map[string]time.Duration) []string {
	order := []string{"working", "delegating", "dormant", "idle", "permission", "suspended", ""}
	var out []string
	seen := map[string]bool{}
	for _, st := range order {
		if _, ok := m[st]; ok {
			out = append(out, st)
			seen[st] = true
		}
	}
	// Any unexpected status, appended in sorted order.
	var extra []string
	for st := range m {
		if !seen[st] {
			extra = append(extra, st)
		}
	}
	sort.Strings(extra)
	return append(out, extra...)
}

func statusName(st string) string {
	if st == "" {
		return "unknown"
	}
	return st
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
