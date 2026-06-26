package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/tjmisko/switchboard/internal/durfmt"
	"github.com/tjmisko/switchboard/internal/history"
)

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
	day := fs.String("day", "", "single UTC day (YYYY-MM-DD; default today)")
	since := fs.String("since", "", "range start UTC day (YYYY-MM-DD)")
	until := fs.String("until", "", "range end UTC day, inclusive (YYYY-MM-DD)")
	width := fs.Int("width", 48, "swimlane bar width in columns")
	asJSON := fs.Bool("json", false, "emit the swimlanes + summary as JSON")
	noColor := fs.Bool("no-color", false, "disable ANSI color")
	_ = fs.Parse(args)

	from, to, label := resolveWindow(*day, *since, *until)
	// Clamp the open-interval end to now, so a running session today extends to
	// the present rather than the (future) end-of-day bound.
	end := to
	if now := time.Now(); end.After(now) {
		end = now
	}

	events, err := history.ReadRange(*dir, from, to)
	if err != nil {
		fail("read %s: %v", *dir, err)
	}
	lanes := history.BuildSwimlanes(events, end)
	summary := history.Summarize(lanes)
	totals := history.AggregateTotals(events)

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(struct {
			Window  string             `json:"window"`
			Lanes   []history.Swimlane `json:"lanes"`
			Summary history.Summary    `json:"summary"`
			Totals  history.Totals     `json:"totals"`
		}{label, lanes, summary, totals})
		return
	}
	renderSwimlanes(os.Stdout, label, lanes, summary, totals, *width, !*noColor && isTTY(os.Stdout))
}

// resolveWindow turns the day/since/until flags into a [from, to) UTC window and
// a human label. Precedence: an explicit --since/--until range, else --day, else
// today. `to` is exclusive (start of the day after the last).
func resolveWindow(day, since, until string) (from, to time.Time, label string) {
	parse := func(s string) time.Time {
		t, err := time.ParseInLocation("2006-01-02", s, time.UTC)
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
		end := time.Now().UTC()
		if until != "" {
			end = parse(until).AddDate(0, 0, 1)
		}
		return from, end, fmt.Sprintf("%s … %s", dayOrStar(since), dayOrStar(until))
	case day != "":
		d := parse(day)
		return d, d.AddDate(0, 0, 1), day
	default:
		today := time.Now().UTC().Truncate(24 * time.Hour)
		return today, today.AddDate(0, 0, 1), today.Format("2006-01-02")
	}
}

func dayOrStar(s string) string {
	if s == "" {
		return "*"
	}
	return s
}

func renderSwimlanes(w *os.File, label string, lanes []history.Swimlane, s history.Summary, totals history.Totals, width int, color bool) {
	fmt.Fprintf(w, "timeline %s  (%d session%s)\n", label, len(lanes), plural(len(lanes)))
	if len(lanes) == 0 {
		fmt.Fprintln(w, "\nno events (history may be disabled — see `history path`)")
		return
	}
	from, to := s.From, s.To
	fmt.Fprintf(w, "%s … %s\n\n", from.Local().Format("15:04 Mon 02"), to.Local().Format("15:04 Mon 02"))

	for _, lane := range lanes {
		id := lane.SessionID
		if len(id) > 8 {
			id = id[:8]
		}
		bar := renderBar(lane, from, to, width, color)
		fmt.Fprintf(w, "%-10s %-8s %s %s–%s\n",
			truncate(lane.Project, 10), id, bar,
			lane.Start.Local().Format("15:04"), lane.End.Local().Format("15:04"))
	}

	fmt.Fprintf(w, "\nsummary\n")
	for _, st := range statusOrder(s.ByStatus) {
		fmt.Fprintf(w, "  %-12s %s\n", statusName(st), durfmt.Compact(s.ByStatus[st]))
	}
	fmt.Fprintf(w, "  %-26s %s\n", "attention · A (union)", durfmt.Compact(s.AttentionUnion))
	fmt.Fprintf(w, "  %-26s %s\n", "attention · B (per-session)", durfmt.Compact(s.AttentionPerSession))
	fmt.Fprintf(w, "  %-26s %s\n", "attention · C (fanout-weighted)", durfmt.Compact(s.AttentionFanout))
	if totals.Subagents > 0 {
		fmt.Fprintf(w, "  %-26s %d\n", "subagents launched", totals.Subagents)
	}
	if tok := totals.TotalTokens(); tok > 0 {
		fmt.Fprintf(w, "  %-26s %s  (in %s · out %s · cache %s)\n", "tokens used", humanCount(tok),
			humanCount(totals.TokIn), humanCount(totals.TokOut), humanCount(totals.TokCacheRead+totals.TokCacheCreate))
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
	order := []string{"working", "delegating", "idle", "permission", "suspended", ""}
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
