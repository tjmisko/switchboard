package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/tjmisko/switchboard/internal/statustune"
)

// cmdDiagnose answers "the chip was the wrong color around <time>" without making
// the user hand-craft a journalctl + grep pipeline. It pulls the daemon's
// status-decision log lines for a time window, keeps the ones a plain-English
// symptom makes relevant, and prints each with the rule that fired and the
// statustune.Tuning knob that governs it — turning a vague complaint into a
// specific, actionable line + the field to change.
//
// It needs no daemon connection: the journal (or a saved dump via --file) is the
// source of truth.
func cmdDiagnose(args []string) {
	fs := flag.NewFlagSet("diagnose", flag.ExitOnError)
	var (
		around  = fs.String("around", "", "center the window on this time (e.g. 14:30, \"2026-06-23 14:30:00\")")
		window  = fs.Duration("window", 2*time.Minute, "half-width of the --around window")
		since   = fs.String("since", "", "journalctl --since (default: 1 hour ago, or --around-window)")
		until   = fs.String("until", "", "journalctl --until")
		session = fs.String("session", "", "only this session id (matches the short id in the logs)")
		pid     = fs.Int("pid", 0, "only this pid")
		unit    = fs.String("unit", "switchboard.service", "systemd unit to read")
		system  = fs.Bool("system", false, "read the system journal (default: --user)")
		file    = fs.String("file", "", "read log lines from this file (or - for stdin) instead of journalctl")
		symFlag = fs.String("symptom", "", "force the symptom: red|green|orange|all (default: infer from the description)")
		asJSON  = fs.Bool("json", false, "emit the parsed/filtered records as JSON")
		limit   = fs.Int("limit", 200, "max decision lines to display")
	)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, strings.TrimSpace(`
usage: switchboard-ctl diagnose [flags] [words describing the problem]

Find the status-decision log lines behind a wrong-color complaint and name the
tuning knob to change. Examples:

  switchboard-ctl diagnose --around 14:32 red was stuck for ages
  switchboard-ctl diagnose --since "20 min ago" should have been green not orange
  switchboard-ctl diagnose --session ce13c0f2 --symptom green went green too early
  journalctl --user -u switchboard.service -o short-iso | switchboard-ctl diagnose --file - red

flags:`))
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	// Resolve the time window. --around computes since/until around a point;
	// otherwise pass --since/--until straight through, defaulting to the last hour.
	s, u := *since, *until
	if *around != "" {
		center, err := parseAround(*around)
		if err != nil {
			fail("--around %q: %v", *around, err)
		}
		s = center.Add(-*window).Format("2006-01-02 15:04:05")
		u = center.Add(*window).Format("2006-01-02 15:04:05")
	} else if s == "" && *file == "" {
		s = "1 hour ago"
	}

	lines, err := gatherLines(*file, *unit, *system, s, u)
	if err != nil {
		fail("%v", err)
	}

	sym := resolveSymptom(*symFlag, fs.Args())
	runDiagnose(os.Stdout, lines, sym, *session, *pid, *limit, *asJSON)
}

// gatherLines reads candidate log lines from a file/stdin or by invoking
// journalctl. Returning raw lines (with whatever timestamp prefix the source
// carries) keeps ParseDecision/extractTime as the single parsing authority.
func gatherLines(file, unit string, system bool, since, until string) ([]string, error) {
	if file != "" {
		var r io.Reader = os.Stdin
		if file != "-" {
			f, err := os.Open(file)
			if err != nil {
				return nil, fmt.Errorf("open %s: %w", file, err)
			}
			defer f.Close()
			r = f
		}
		return readLines(r)
	}
	jargs := []string{}
	if !system {
		jargs = append(jargs, "--user")
	}
	jargs = append(jargs, "-u", unit, "-o", "short-iso", "--no-pager")
	if since != "" {
		jargs = append(jargs, "--since", since)
	}
	if until != "" {
		jargs = append(jargs, "--until", until)
	}
	out, err := exec.Command("journalctl", jargs...).Output()
	if err != nil {
		return nil, fmt.Errorf("journalctl failed: %w (try --file to read a saved dump, or --system for the system journal)", err)
	}
	return readLines(strings.NewReader(string(out)))
}

func readLines(r io.Reader) ([]string, error) {
	var lines []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024) // tolerate long lines
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines, sc.Err()
}

// runDiagnose is the pure core: parse, filter, classify, render. Split out from
// cmdDiagnose (which does the impure flag/journal I/O) so it is testable by
// feeding synthetic lines directly.
func runDiagnose(w io.Writer, lines []string, sym symptom, sessionFilter string, pidFilter, limit int, asJSON bool) {
	var recs []statustune.Record
	for _, line := range lines {
		rec, ok := statustune.ParseDecision(line)
		if !ok {
			continue
		}
		if sessionFilter != "" && rec.Session != sessionFilter {
			continue
		}
		if pidFilter != 0 && rec.PID != pidFilter {
			continue
		}
		rec.Time, _ = extractTime(line)
		recs = append(recs, rec)
	}
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].Time.Before(recs[j].Time) })
	if len(recs) > limit {
		recs = recs[len(recs)-limit:] // keep the most recent
	}

	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(recs)
		return
	}

	fmt.Fprintf(w, "switchboard diagnose — %s\n", sym.headline)
	if len(recs) == 0 {
		fmt.Fprintln(w, "\nno status-decision lines matched. widen --window/--since, drop --session/--pid, or check the unit with --unit.")
		return
	}
	renderTimeline(w, recs, sym)
	renderSummary(w, recs, sym)
}

// renderTimeline prints the decisions grouped by session in time order, flagging
// the ones the symptom deems relevant (←) and annotating those that have a knob.
func renderTimeline(w io.Writer, recs []statustune.Record, sym symptom) {
	var order []string
	bySession := map[string][]statustune.Record{}
	for _, r := range recs {
		if _, seen := bySession[r.Session]; !seen {
			order = append(order, r.Session)
		}
		bySession[r.Session] = append(bySession[r.Session], r)
	}
	for _, sid := range order {
		group := bySession[sid]
		fmt.Fprintf(w, "\nsession %s (pid %d)\n", sidOrUnknown(sid), group[0].PID)
		for _, r := range group {
			marker := "  "
			if sym.relevant(r) {
				marker = "→ "
			}
			fmt.Fprintf(w, "  %s%s  %-26s %s%s\n", marker, clockOf(r.Time), transitionField(r), detailOf(r), tupleOf(r))
			if sym.relevant(r) {
				if h := statustune.RuleKnob(r.Rule); h.Field != "" {
					fmt.Fprintf(w, "        ↳ knob: Tuning.%s — %s\n", h.Field, h.What)
				} else if r.Kind == statustune.KindReconciler && h.What != "" {
					fmt.Fprintf(w, "        ↳ %s\n", h.What)
				}
			}
		}
	}
}

// renderSummary lists the rules behind the highlighted decisions (with the knob
// to change), the RED-episode durations recovered from the age= field, and the
// next step.
func renderSummary(w io.Writer, recs []statustune.Record, sym symptom) {
	type tally struct {
		count int
		knob  statustune.KnobHint
	}
	ruleCount := map[string]*tally{}
	var ruleOrder []string
	var redDurations []time.Duration
	highlighted := 0
	for _, r := range recs {
		if !sym.relevant(r) {
			continue
		}
		highlighted++
		if r.Rule != "" {
			if _, ok := ruleCount[r.Rule]; !ok {
				ruleCount[r.Rule] = &tally{knob: statustune.RuleKnob(r.Rule)}
				ruleOrder = append(ruleOrder, r.Rule)
			}
			ruleCount[r.Rule].count++
		}
		// A permission exit's age IS the red duration (how long it held red).
		if r.From == "permission" && r.To != "permission" && !r.Hold && r.HasTuple {
			redDurations = append(redDurations, r.Age)
		}
	}

	fmt.Fprintf(w, "\nsummary: %d of %d decision(s) relevant to this symptom\n", highlighted, len(recs))
	if len(ruleOrder) > 0 {
		fmt.Fprintln(w, "  rules that fired (and the knob to change):")
		for _, rule := range ruleOrder {
			t := ruleCount[rule]
			if t.knob.Field != "" {
				fmt.Fprintf(w, "    %s ×%d  → Tuning.%s\n", rule, t.count, t.knob.Field)
			} else {
				fmt.Fprintf(w, "    %s ×%d  → (no knob: %s)\n", rule, t.count, t.knob.What)
			}
		}
	}
	if len(redDurations) > 0 {
		parts := make([]string, len(redDurations))
		for i, d := range redDurations {
			parts[i] = d.String()
		}
		fmt.Fprintf(w, "  RED held for: %s  (from the age= on each permission exit)\n", strings.Join(parts, ", "))
	}
	fmt.Fprintln(w, "  next: change the Tuning field above in cmd/switchboard/main.go, rebuild, and pin it with a test row.")
}

// --- symptom classification ---

type symptom struct {
	name     string
	headline string
	relevant func(statustune.Record) bool
}

func touchesPermission(r statustune.Record) bool {
	return r.From == "permission" || r.To == "permission"
}
func turnedGreen(r statustune.Record) bool {
	return !r.Hold && (r.To == "working" || r.To == "delegating") && r.From != r.To
}
func touchesIdleOrDelegating(r statustune.Record) bool {
	in := func(s string) bool { return s == "idle" || s == "delegating" }
	return in(r.From) || in(r.To)
}

var (
	symRed    = symptom{"stale-red", "stale/stuck RED — when and why did the red chip release?", touchesPermission}
	symGreen  = symptom{"false-green", "premature GREEN — what turned the chip green?", turnedGreen}
	symOrange = symptom{"false-orange", "ORANGE while work continued — was delegation detected?", touchesIdleOrDelegating}
	symAll    = symptom{"all", "all status decisions in the window", func(statustune.Record) bool { return true }}
)

// symptomKeywords maps free-text words to a symptom by keyword hit-count.
var symptomKeywords = []struct {
	sym  symptom
	keys []string
}{
	{symRed, []string{"red", "stuck", "stale", "nag", "linger", "late", "slow", "blocked", "permission", "long", "ages"}},
	{symGreen, []string{"green", "early", "premature", "soon", "cleared", "clear"}},
	{symOrange, []string{"orange", "idle", "yellow", "teammate", "subagent", "delegat", "working", "spinner"}},
}

// resolveSymptom honors an explicit --symptom, else infers from the description
// words (the highest keyword hit-count wins; ties and no-match fall back to all).
func resolveSymptom(flagVal string, words []string) symptom {
	switch strings.ToLower(flagVal) {
	case "red", "stale", "permission":
		return symRed
	case "green":
		return symGreen
	case "orange", "idle", "delegating":
		return symOrange
	case "all":
		return symAll
	}
	desc := strings.ToLower(strings.Join(words, " "))
	best, bestScore, tie := symAll, 0, false
	for _, sk := range symptomKeywords {
		score := 0
		for _, k := range sk.keys {
			if strings.Contains(desc, k) {
				score++
			}
		}
		switch {
		case score > bestScore:
			best, bestScore, tie = sk.sym, score, false
		case score == bestScore && score > 0:
			tie = true
		}
	}
	if bestScore == 0 || tie {
		return symAll
	}
	return best
}

// --- formatting helpers ---

func transitionField(r statustune.Record) string {
	verb := "->"
	if r.Hold {
		verb = "=="
	}
	from := r.From
	if from == "" {
		from = "·"
	}
	return from + verb + r.To
}

func detailOf(r statustune.Record) string {
	if r.Kind == statustune.KindHook {
		return fmt.Sprintf("event=%s", r.Event)
	}
	if r.Reason != "" {
		return fmt.Sprintf("rule=%s reason=%q", r.Rule, r.Reason)
	}
	return "rule=" + r.Rule
}

func tupleOf(r statustune.Record) string {
	if !r.HasTuple {
		return ""
	}
	return fmt.Sprintf("  [S=%d pending=%q age=%s]", r.Subagents, r.Pending, r.Age)
}

func clockOf(t time.Time) string {
	if t.IsZero() {
		return "??:??:??"
	}
	return t.Format("15:04:05")
}

func sidOrUnknown(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

// extractTime recovers the timestamp from a log line's leading prefix, supporting
// journalctl short-iso (2026-06-23T14:30:01+0000 / RFC3339) and the Go `log`
// default (2026/06/23 14:30:01). Returns false when no prefix parses.
func extractTime(line string) (time.Time, bool) {
	f := strings.Fields(line)
	if len(f) == 0 {
		return time.Time{}, false
	}
	for _, layout := range []string{"2006-01-02T15:04:05-0700", time.RFC3339} {
		if t, err := time.Parse(layout, f[0]); err == nil {
			return t, true
		}
	}
	if len(f) >= 2 {
		if t, err := time.ParseInLocation("2006/01/02 15:04:05", f[0]+" "+f[1], time.Local); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// parseAround parses the --around value: a full date-time, or a clock time which
// is taken as today (local). It is intentionally strict (a handful of layouts) —
// for anything journalctl-specific, use --since/--until directly.
func parseAround(s string) (time.Time, error) {
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	now := time.Now()
	for _, layout := range []string{"15:04:05", "15:04"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.Local), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized time (try 15:04, 2026-06-23 14:30:00, or use --since/--until)")
}
