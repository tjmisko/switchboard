package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/tjmisko/switchboard/internal/history"
)

// cmdMemory renders the per-session memory series and the machine-wide pressure
// series the activity log recorded. Like `timeline` it reads the on-disk log
// directly — no daemon — and emits text by default or the structured document
// with --json.
//
// Memory is a SURFACE OF ITS OWN, not a section of the timeline envelope
// (docs/history-schema.md, "Why memory is not in the timeline envelope"): the
// dashboard decides whether to repaint by comparing raw response bytes, and a
// live sample series changes those bytes on every poll. Serving it separately,
// read lazily when a tooltip opens, keeps `timeline --json` byte-for-byte
// unaffected by this feature.
//
//	switchboard-ctl memory                        today
//	switchboard-ctl memory --day 2026-08-03
//	switchboard-ctl memory --since 2026-08-01 --until 2026-08-03
//	switchboard-ctl memory --json
func cmdMemory(args []string) {
	fs := flag.NewFlagSet("memory", flag.ExitOnError)
	dir := fs.String("dir", history.DefaultDir(), "activity-log directory")
	day := fs.String("day", "", "single local day (YYYY-MM-DD; default today)")
	since := fs.String("since", "", "range start local day (YYYY-MM-DD)")
	until := fs.String("until", "", "range end local day, inclusive (YYYY-MM-DD)")
	asJSON := fs.Bool("json", false, "emit the memory document as JSON")
	suspectCap := fs.Duration("suspect-cap", history.DefaultSuspectTrailingCap,
		"clip a session's series at the point its lane stopped being trusted (0 disables the post-check)")
	_ = fs.Parse(args)

	from, to, label := resolveWindow(*day, *since, *until)
	now := time.Now()
	end := to
	if end.After(now) {
		end = now
	}

	events, err := history.ReadRange(*dir, from, to)
	if err != nil {
		fail("read %s: %v", *dir, err)
	}

	// The lanes are built here for one purpose: to find out where each session
	// stopped being believed. A flagged lane has no evidence past its
	// SuspectSince, and this is exactly the shape that keeps producing memory
	// samples anyway — a hung or Ctrl-Z'd process still holds its pages — so a
	// hover drawing the series past that instant would contradict the bar it
	// annotates. Nothing else from the lanes reaches this document.
	lanes := history.BuildSwimlanes(events, end)
	bound := history.BoundWindow
	if to.After(now) || (*since != "" && *until == "") {
		bound = history.BoundNow
	}
	policy := history.DefaultSuspectPolicy(end, bound)
	policy.LaneCap = *suspectCap
	if *suspectCap <= 0 {
		policy.SubagentCap = 0
	}
	history.FlagSuspectLanes(lanes, policy)

	sessions := history.BuildMemorySessions(events, lanes)
	pressure := history.BuildPressure(events)

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(struct {
			Window   string                  `json:"window"`
			Sessions []history.MemorySession `json:"sessions"`
			Pressure []history.PressurePoint `json:"pressure"`
		}{label, sessions, pressure})
		return
	}
	renderMemory(os.Stdout, label, sessions, pressure)
}

// renderMemory prints the human-facing view: one row per session with both
// buckets and what spawned work cost, then the pressure line.
func renderMemory(w *os.File, label string, sessions []history.MemorySession, pressure []history.PressurePoint) {
	fmt.Fprintf(w, "memory %s  (%d session%s)\n", label, len(sessions), plural(len(sessions)))
	if len(sessions) == 0 {
		fmt.Fprintln(w, "\nno memory samples (history may be disabled, or the sampler off — see `history path`)")
		return
	}
	fmt.Fprintf(w, "\n%-20s %-8s %17s %17s %10s %8s\n",
		"project", "session", "agent peak/avg", "tree peak/avg", "spawned", "samples")
	for _, s := range sessions {
		name := s.Project
		if name == "" {
			name = fmt.Sprintf("pid %d", s.PID)
		}
		fmt.Fprintf(w, "%-20s %-8s %17s %17s %10s %8d\n",
			truncate(name, 20), shortID(s.SessionID),
			fmt.Sprintf("%s / %s", humanBytes(s.PeakAgentBytes), humanBytes(s.AvgAgentBytes)),
			fmt.Sprintf("%s / %s", humanBytes(s.PeakTreeBytes), humanBytes(s.AvgTreeBytes)),
			humanBytes(s.PeakTreeBytes-s.PeakAgentBytes), len(s.Mem))
	}
	renderPressure(w, pressure)
}

// renderPressure summarizes the machine-wide series: how low available memory
// went, and how long the machine actually spent stalled on memory. The stall
// total is the sum of the per-interval deltas, which is the figure that survives
// a sampler aliasing past a spike; avg10 is shown as its peak only, since it is a
// decaying average and a total of it would mean nothing.
func renderPressure(w *os.File, pressure []history.PressurePoint) {
	if len(pressure) == 0 {
		return
	}
	var lowAvail int64
	var haveAvail bool
	var stall int64
	var peakAvg10 float64
	var havePSI bool
	for _, p := range pressure {
		if p.AvailBytes != nil && (!haveAvail || *p.AvailBytes < lowAvail) {
			lowAvail, haveAvail = *p.AvailBytes, true
		}
		if p.PSIStallUs != nil {
			stall += *p.PSIStallUs
			havePSI = true
		}
		if p.PSIAvg10 != nil && *p.PSIAvg10 > peakAvg10 {
			peakAvg10 = *p.PSIAvg10
		}
	}
	fmt.Fprintf(w, "\npressure (%d point%s)\n", len(pressure), plural(len(pressure)))
	if haveAvail {
		fmt.Fprintf(w, "  %-26s %s\n", "available, low-water", humanBytes(lowAvail))
	}
	if !havePSI {
		// A kernel without CONFIG_PSI. Saying so is the point: absent is not zero,
		// and a blank line here would read as "the machine never stalled".
		fmt.Fprintf(w, "  %-26s %s\n", "psi", "not measured (no CONFIG_PSI)")
		return
	}
	fmt.Fprintf(w, "  %-26s %s\n", "stalled on memory", time.Duration(stall)*time.Microsecond)
	fmt.Fprintf(w, "  %-26s %.1f%%\n", "psi some avg10, peak", peakAvg10)
}

// humanBytes renders a byte count compactly in binary units: 1536 → "1.5K",
// 658374656 → "627.9M". Mirrors humanCount's shape for the token counts.
//
// Deliberately NOT history.HumanBytes, which renders a non-positive count as
// "unlimited" — right for the retention cap it was written for, wrong for a
// measurement, where 0 is an ordinary reading: a session that never spawned
// anything has exactly 0 bytes of spawned work, and this column says so.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 3; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%c", float64(n)/float64(div), "KMGT"[exp])
}
