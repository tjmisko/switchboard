package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

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
	for _, ev := range evs {
		fmt.Println(formatEvent(ev))
	}
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
