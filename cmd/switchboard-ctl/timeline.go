package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/tjmisko/switchboard/internal/durfmt"
	"github.com/tjmisko/switchboard/internal/history"
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
	lanes := history.BuildSwimlanes(events, end)
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
	// Reattribute parent "working" time that overlaps a launched subagent to
	// "dormant" (the subagent carries the compute) before summarizing or encoding,
	// so the swimlanes, by_status, and attention metrics all agree.
	history.MarkDelegationDormant(lanes)
	summary := history.Summarize(lanes, events)
	totals := history.AggregateTotals(events)
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
		pw := history.AggregatePlanWindow(pwEvents, pwFrom, pwTo)
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
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(struct {
			Window     string                 `json:"window"`
			Lanes      []history.Swimlane     `json:"lanes"`
			Summary    history.Summary        `json:"summary"`
			Totals     history.Totals         `json:"totals"`
			Activity   []history.ActivitySpan `json:"activity,omitempty"`
			PlanWindow *history.PlanWindow    `json:"plan_window,omitempty"`
		}{label, lanes, summary, totals, activity, planWin})
		return
	}
	renderSwimlanes(os.Stdout, label, lanes, summary, totals, suspect, *width, !*noColor && isTTY(os.Stdout))
	if planWin != nil {
		fmt.Fprintf(os.Stdout, "\nplan window (last %gh)\n", planWin.Hours)
		fmt.Fprintf(os.Stdout, "  %-12s $%.2f\n", "cost", planWin.CostUSD)
		fmt.Fprintf(os.Stdout, "  %-12s %s\n", "tokens", humanCount(planWin.TokIn+planWin.TokOut+planWin.TokCacheRead+planWin.TokCacheCreate))
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
	if totals.CostUSD > 0 {
		fmt.Fprintf(w, "  %-26s $%.2f\n", "cost (recomputed)", totals.CostUSD)
	}
	renderSuspect(w, lanes, s, suspect)
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
