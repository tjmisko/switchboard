package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/history"
)

// cmdHistory manages the on-disk activity log (the durable, opt-in stream of
// status transitions and lifecycle events the daemon records). It reads the
// files directly — no daemon connection — so it works whether or not the daemon
// is running, and even when history recording is disabled (the files persist).
//
//	switchboard-ctl history path                      print the log directory
//	switchboard-ctl history tail [--day D] [-n N]     show the most recent events
//	switchboard-ctl history stat                      summarize what is stored
//	switchboard-ctl history purge [--before D | --all]  delete day-files
//	switchboard-ctl history calibrate                 re-derive the suspect caps
func cmdHistory(args []string) {
	if len(args) == 0 {
		historyUsage()
		os.Exit(2)
	}
	switch args[0] {
	case "path":
		fmt.Println(historyDir(args[1:]))
	case "tail":
		cmdHistoryTail(args[1:])
	case "stat":
		cmdHistoryStat(args[1:])
	case "purge":
		cmdHistoryPurge(args[1:])
	case "calibrate":
		cmdHistoryCalibrate(args[1:])
	default:
		historyUsage()
		os.Exit(2)
	}
}

func historyUsage() {
	fmt.Fprintln(os.Stderr, strings.TrimSpace(`
usage: switchboard-ctl history <command> [flags]

  path                              print the activity-log directory
  tail [--day YYYY-MM-DD] [-n N]    show the N most recent events (default today, 20)
  stat                              summarize stored events (counts, size, range)
  purge [--before YYYY-MM-DD]       delete day-files older than a date
  purge --all                       delete the entire log
  calibrate                         re-derive the suspect caps from this corpus

All commands take --dir to override the directory (default $XDG_STATE_HOME/switchboard/history).
Recording is opt-in: enable it in $XDG_CONFIG_HOME/switchboard/history.json ({"enabled":true}).`))
}

// historyDir resolves the log directory honored by every subcommand: the --dir
// flag if given, else the XDG default.
func historyDir(args []string) string {
	fs := flag.NewFlagSet("history", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dir := fs.String("dir", history.DefaultDir(), "activity-log directory")
	// Parse leniently so subcommand-specific flags (already pulled by the caller)
	// do not trip this; we only want --dir.
	_ = fs.Parse(args)
	return *dir
}

func cmdHistoryTail(args []string) {
	fs := flag.NewFlagSet("history tail", flag.ExitOnError)
	dir := fs.String("dir", history.DefaultDir(), "activity-log directory")
	day := fs.String("day", "", "local day to read (YYYY-MM-DD; default today)")
	n := fs.Int("n", 20, "number of most-recent events to show")
	asJSON := fs.Bool("json", false, "emit raw JSON events")
	_ = fs.Parse(args)

	d := *day
	if d == "" {
		d = time.Now().Format("2006-01-02") // local day, matching how files partition
	}
	evs, err := history.ReadDay(*dir, d)
	if err != nil {
		fail("read %s: %v", d, err)
	}
	if len(evs) > *n {
		evs = evs[len(evs)-*n:]
	}
	if len(evs) == 0 {
		fmt.Printf("no events for %s (history may be disabled — see `history path`)\n", d)
		return
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		for _, ev := range evs {
			_ = enc.Encode(ev)
		}
		return
	}
	for _, line := range formatHistoryEvents(evs) {
		fmt.Println(line)
	}
}

// formatHistoryEvents renders the human history view. Raw --json deliberately
// bypasses it: compatibility projections remain durable evidence even when the
// human view suppresses a legacy subagent edge duplicated by agent_state.
func formatHistoryEvents(events []history.Event) []string {
	canonical := make(map[legacyAgentEdge]struct{})
	for _, ev := range events {
		if ev.Type != history.EventAgentState {
			continue
		}
		if edge, ok := canonicalLegacyEdge(ev); ok {
			canonical[edge] = struct{}{}
		}
	}
	lines := make([]string, 0, len(events))
	for _, ev := range events {
		if edge, ok := projectedLegacyEdge(ev); ok {
			if _, duplicate := canonical[edge]; duplicate {
				continue
			}
		}
		lines = append(lines, formatEvent(ev))
	}
	return lines
}

type legacyAgentEdge struct {
	root string
	node string
	kind string
	at   int64
}

func canonicalLegacyEdge(ev history.Event) (legacyAgentEdge, bool) {
	kind := ""
	switch {
	case !agentLifecycleLive(ev.FromLifecycle) && agentLifecycleLive(ev.ToLifecycle):
		kind = history.EventSubagentSpawn
	case !ev.FromLifecycle.Terminal() && ev.ToLifecycle.Terminal():
		kind = history.EventSubagentStop
	}
	if kind == "" || ev.SessionID == "" || ev.ThreadID == "" || ev.ParentThreadID == "" {
		return legacyAgentEdge{}, false
	}
	return legacyAgentEdge{root: ev.SessionID, node: ev.ThreadID, kind: kind, at: ev.Ts.UnixNano()}, true
}

func projectedLegacyEdge(ev history.Event) (legacyAgentEdge, bool) {
	if ev.Type != history.EventSubagentSpawn && ev.Type != history.EventSubagentStop {
		return legacyAgentEdge{}, false
	}
	node := ev.AgentID
	if node == "" {
		node = ev.ToolUseID
	}
	if ev.SessionID == "" || node == "" {
		return legacyAgentEdge{}, false
	}
	return legacyAgentEdge{root: ev.SessionID, node: node, kind: ev.Type, at: ev.Ts.UnixNano()}, true
}

func agentLifecycleLive(value agentgraph.LifecycleState) bool {
	return value == agentgraph.LifecyclePending || value == agentgraph.LifecycleRunning
}

// formatEvent renders one event as a compact human line:
//
//	14:32:07  transition  ce13c0f2  permission->working  sb   2s  (case9-approve-toolmatch)
func formatEvent(ev history.Event) string {
	id := ev.SessionID
	if len(id) > 8 {
		id = id[:8]
	}
	if id == "" {
		id = fmt.Sprintf("pid%d", ev.PID)
	}
	var detail string
	switch ev.Type {
	case history.EventTransition:
		detail = fmt.Sprintf("%s->%s", orDash(ev.From), orDash(ev.To))
		if ev.Subagents > 0 {
			detail += fmt.Sprintf(" S=%d", ev.Subagents)
		}
	case history.EventSubagentSpawn:
		detail = ev.AgentType
		if ev.Description != "" {
			detail += ": " + ev.Description
		}
	case history.EventSubagentStop:
		detail = ev.AgentType
	case history.EventAgentState:
		detail = formatAgentStateDetail(ev)
	case history.EventUsageSample:
		detail = fmt.Sprintf("in=%d out=%d cache=%d", ev.TokIn, ev.TokOut, ev.TokCacheRead+ev.TokCacheCreate)
	default:
		detail = ev.Agent
	}
	line := fmt.Sprintf("%s  %-11s  %-8s  %-22s  %s",
		ev.Ts.Local().Format("15:04:05"), ev.Type, id, detail, ev.Project)
	if ev.DurPrevMs > 0 {
		line += fmt.Sprintf("  %s", time.Duration(ev.DurPrevMs)*time.Millisecond)
	}
	if ev.Rule != "" {
		line += fmt.Sprintf("  (%s)", ev.Rule)
	}
	return strings.TrimRight(line, " ")
}

func formatAgentStateDetail(ev history.Event) string {
	label := ev.Nickname
	if label != "" && ev.Role != "" {
		label += " (" + ev.Role + ")"
	} else if label == "" {
		label = ev.Role
	}
	if label == "" {
		label = shortAgentID(ev.ThreadID)
	}
	if ev.ParentThreadID != "" {
		label = "└─ " + label
	}

	var axes []string
	if ev.FromRuntime != ev.ToRuntime {
		axes = append(axes, "runtime "+agentAxisValue(string(ev.FromRuntime))+"→"+agentAxisValue(string(ev.ToRuntime)))
	}
	if ev.FromAttention != ev.ToAttention {
		axes = append(axes, "attention "+agentAxisValue(string(ev.FromAttention))+"→"+agentAxisValue(string(ev.ToAttention)))
	}
	if ev.FromLifecycle != ev.ToLifecycle {
		axes = append(axes, "lifecycle "+agentAxisValue(string(ev.FromLifecycle))+"→"+agentAxisValue(string(ev.ToLifecycle)))
	}
	if len(axes) == 0 {
		axes = append(axes, "state unchanged")
	}
	return label + "  " + strings.Join(axes, " · ")
}

func shortAgentID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	if id == "" {
		return "unknown"
	}
	return id
}

func agentAxisValue(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}

func orDash(s string) string {
	if s == "" {
		return "·"
	}
	return s
}

func cmdHistoryStat(args []string) {
	fs := flag.NewFlagSet("history stat", flag.ExitOnError)
	dir := fs.String("dir", history.DefaultDir(), "activity-log directory")
	_ = fs.Parse(args)

	days, err := history.Days(*dir)
	if err != nil {
		fail("read %s: %v", *dir, err)
	}
	if len(days) == 0 {
		fmt.Printf("%s\nno events recorded (history may be disabled)\n", *dir)
		return
	}
	byType := map[string]int{}
	total := 0
	for _, day := range days {
		evs, err := history.ReadDay(*dir, day)
		if err != nil {
			continue
		}
		for _, ev := range evs {
			byType[ev.Type]++
			total++
		}
	}
	fmt.Printf("%s\n", *dir)
	fmt.Printf("%d events across %d day(s): %s … %s\n", total, len(days), days[0], days[len(days)-1])
	types := make([]string, 0, len(byType))
	for t := range byType {
		types = append(types, t)
	}
	sort.Strings(types)
	for _, t := range types {
		fmt.Printf("  %-14s %d\n", t, byType[t])
	}
}

func cmdHistoryPurge(args []string) {
	fs := flag.NewFlagSet("history purge", flag.ExitOnError)
	dir := fs.String("dir", history.DefaultDir(), "activity-log directory")
	before := fs.String("before", "", "delete day-files strictly older than this local day (YYYY-MM-DD)")
	all := fs.Bool("all", false, "delete the entire log")
	_ = fs.Parse(args)

	if *before == "" && !*all {
		fail("purge needs --before YYYY-MM-DD or --all")
	}
	var cutoff time.Time
	if *before != "" {
		t, err := time.ParseInLocation("2006-01-02", *before, time.Local)
		if err != nil {
			fail("--before %q: want YYYY-MM-DD", *before)
		}
		cutoff = t
	}
	removed, err := history.Purge(*dir, cutoff)
	if err != nil {
		fail("purge: %v", err)
	}
	fmt.Printf("removed %d day-file(s) from %s\n", removed, *dir)
}

// cmdHistoryCalibrate re-derives the two suspect caps from whatever corpus is on
// disk and reports whether the corpus still says what the frozen comment on
// history.DefaultSuspectTrailingCap claims it says.
//
// It is an operator/maintainer tool, not part of any pipeline: the analysis needs
// a month of real activity log, which the repo cannot carry, so nothing in CI runs
// it and TestSuspectCapDefaults still pins the constants outright. The output is
// the argument, not a decision — a drifted band is a reason to go look at the
// named lanes, not to move a constant.
func cmdHistoryCalibrate(args []string) {
	fs := flag.NewFlagSet("history calibrate", flag.ExitOnError)
	dir := fs.String("dir", history.DefaultDir(), "activity-log directory")
	_ = fs.Parse(args)

	cal, err := history.Calibrate(*dir, time.Now())
	if err != nil {
		fail("read %s: %v", *dir, err)
	}
	renderCalibration(os.Stdout, cal)
}

func renderCalibration(w io.Writer, cal history.Calibration) {
	fmt.Fprintln(w, cal.Dir)
	if len(cal.Days) == 0 {
		fmt.Fprintln(w, "no complete day-files to replay (history may be disabled — see `history path`)")
		return
	}
	fmt.Fprintf(w, "%d complete day(s): %s … %s; %d lane(s) replayed\n",
		len(cal.Days), cal.Days[0], cal.Days[len(cal.Days)-1], cal.Lanes)

	fmt.Fprintf(w, "\nlanes — silence since the last evidence, for lanes the reader closed at the bound\n")
	renderCalibrationPopulation(w, "legitimate (the corpus proves the session outlived the bound)", cal.LaneLegit)
	renderCalibrationPopulation(w, "ghost (nothing is ever heard from it again, in any day-file)", cal.LaneGhost)
	renderCalibrationVerdict(w, "DefaultSuspectTrailingCap",
		cal.LaneVerdict(history.DefaultSuspectTrailingCap), "legitimate", "ghost")

	fmt.Fprintf(w, "\nsubagent spans — span length\n")
	renderCalibrationPopulation(w, "paired (a subagent_stop closed it)", cal.SpanPaired)
	renderCalibrationPopulation(w, "reader-capped (no subagent_stop ever arrived)", cal.SpanCapped)
	renderCalibrationVerdict(w, "DefaultSuspectSubagentCap",
		cal.SpanVerdict(history.DefaultSuspectSubagentCap), "paired", "reader-capped")
}

// renderCalibrationPopulation prints one population's shape, then names the two
// samples at its extremes. The identity lines are the point: an extreme nobody can
// go look at is how the previous calibration became unarguable folklore.
func renderCalibrationPopulation(w io.Writer, label string, p history.Population) {
	fmt.Fprintf(w, "  %s\n", label)
	if p.Count() == 0 {
		fmt.Fprintf(w, "    count 0\n")
		return
	}
	fmt.Fprintf(w, "    count %-5d min %s   median %s   max %s\n", p.Count(),
		calibrationDur(p.Min().Dur), calibrationDur(p.Median()), calibrationDur(p.Max().Dur))
	fmt.Fprintf(w, "    min   %s\n", calibrationSample(p.Min()))
	fmt.Fprintf(w, "    max   %s\n", calibrationSample(p.Max()))
}

// renderCalibrationVerdict prints the band and where the frozen constant sits in
// it. The two error counts are the verdict proper: the band is the argument for a
// cap, but "how many real samples would this flag" is the thing that is either
// still zero or is not.
func renderCalibrationVerdict(w io.Writer, name string, v history.Verdict, lower, upper string) {
	b := v.Band
	if !v.Decidable() {
		// An absent population is not an overlap. Saying "no threshold separates
		// these" over a corpus holding zero ghosts would read as the cap being
		// refuted, when in fact nothing was weighed against it.
		fmt.Fprintf(w, "  no band: %d %s sample(s), %d %s sample(s) — one of each is needed\n",
			v.LowerCount, lower, v.UpperCount, upper)
		fmt.Fprintf(w, "  %s %s: this corpus does not score it\n",
			name, calibrationDur(v.Threshold))
	} else if b.Separated() {
		fmt.Fprintf(w, "  empty band %s … %s (%s wide)\n",
			calibrationDur(b.Lo), calibrationDur(b.Hi), calibrationDur(b.Width()))
		fmt.Fprintf(w, "  %s %s sits %.0f%% into it, %.2fx the %s maximum\n",
			name, calibrationDur(v.Threshold), b.Position(v.Threshold)*100,
			b.Headroom(v.Threshold), lower)
	} else {
		fmt.Fprintf(w, "  no empty band: the %s maximum (%s) is not below the %s minimum (%s)\n",
			lower, calibrationDur(b.Lo), upper, calibrationDur(b.Hi))
		fmt.Fprintf(w, "  %s %s: no threshold separates these two populations\n",
			name, calibrationDur(v.Threshold))
	}
	fmt.Fprintf(w, "    false positives  %d %s sample(s) at or above it\n", v.FalsePositives, lower)
	fmt.Fprintf(w, "    false negatives  %d %s sample(s) below it\n", v.FalseNegatives, upper)
}

// calibrationSample identifies one measured sample well enough to open the
// day-file and look at it.
func calibrationSample(s history.CalibrationSample) string {
	return strings.TrimRight(fmt.Sprintf("%s  %-8s  pid %-8d %s",
		s.Day, shortID(s.SessionID), s.PID, s.Name), " ")
}

// calibrationDur rounds to the second, the same resolution the suspect reasons
// print at — sub-second precision on a multi-hour silence is noise.
func calibrationDur(d time.Duration) string { return d.Round(time.Second).String() }
