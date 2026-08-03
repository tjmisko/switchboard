package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/history"
)

func atSec(sec int) time.Time {
	return time.Date(2026, 6, 26, 14, 0, sec, 0, time.UTC)
}

func TestResolveWindowDay(t *testing.T) {
	from, to, label := resolveWindow("2026-06-26", "", "")
	// Days are local calendar days, so the window bounds are local midnights.
	if !from.Equal(time.Date(2026, 6, 26, 0, 0, 0, 0, time.Local)) {
		t.Errorf("from = %v", from)
	}
	if !to.Equal(time.Date(2026, 6, 27, 0, 0, 0, 0, time.Local)) {
		t.Errorf("to (exclusive next day) = %v", to)
	}
	if label != "2026-06-26" {
		t.Errorf("label = %q", label)
	}
}

func TestResolveWindowRangeUntilInclusive(t *testing.T) {
	_, to, _ := resolveWindow("", "2026-06-20", "2026-06-26")
	// until is inclusive → exclusive bound is the next local day.
	if !to.Equal(time.Date(2026, 6, 27, 0, 0, 0, 0, time.Local)) {
		t.Errorf("until should be inclusive; to = %v, want 2026-06-27", to)
	}
}

func TestStatusAtCoversIntervalHalfOpen(t *testing.T) {
	lane := history.Swimlane{Intervals: []history.Interval{
		{Status: "working", Start: atSec(0), End: atSec(10)},
		{Status: "idle", Start: atSec(10), End: atSec(20)},
	}}
	if s, ok := statusAt(lane, atSec(5)); !ok || s != "working" {
		t.Errorf("at 5s = (%q,%v), want working", s, ok)
	}
	if s, ok := statusAt(lane, atSec(10)); !ok || s != "idle" {
		t.Errorf("boundary at 10s should belong to the later interval (half-open), got (%q,%v)", s, ok)
	}
	if _, ok := statusAt(lane, atSec(25)); ok {
		t.Errorf("after the lane should be off-lane")
	}
}

func TestHumanCount(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0"},
		{999, "999"},   // boundary: just under 1k stays a plain count
		{1000, "1.0k"}, // boundary: 1k switches to k
		{1500, "1.5k"},
		{999999, "1000.0k"}, // just under 1M still renders in k
		{1000000, "1.0M"},   // boundary: 1M switches to M
		{2500000, "2.5M"},
	}
	for _, c := range cases {
		if got := humanCount(c.n); got != c.want {
			t.Errorf("humanCount(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("within width should be unchanged, got %q", got)
	}
	if got := truncate("hello", 5); got != "hello" {
		t.Errorf("at exactly width should be unchanged, got %q", got)
	}
	if got := truncate("hello world", 5); got != "hell…" {
		t.Errorf("over width = %q, want %q", got, "hell…")
	}
}

func TestStatusName(t *testing.T) {
	if got := statusName(""); got != "unknown" {
		t.Errorf(`statusName("") = %q, want "unknown"`, got)
	}
	if got := statusName("working"); got != "working" {
		t.Errorf("statusName passthrough = %q, want working", got)
	}
}

func TestStatusOrder(t *testing.T) {
	// Known statuses come back in the fixed display order; unexpected ones are
	// appended in sorted order.
	m := map[string]time.Duration{
		"working":    time.Second,
		"idle":       time.Second,
		"permission": time.Second,
		"zzz":        time.Second, // unexpected
		"aaa":        time.Second, // unexpected
	}
	got := statusOrder(m)
	want := []string{"working", "idle", "permission", "aaa", "zzz"}
	if len(got) != len(want) {
		t.Fatalf("statusOrder = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("statusOrder[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

func TestRenderBarPlainUsesStatusInitials(t *testing.T) {
	lane := history.Swimlane{Intervals: []history.Interval{
		{Status: "working", Start: atSec(0), End: atSec(10)},
		{Status: "permission", Start: atSec(10), End: atSec(20)},
	}}
	bar := renderBar(lane, atSec(0), atSec(20), 4, false)
	// First half working (w), second half permission (p).
	if !strings.HasPrefix(bar, "ww") || !strings.HasSuffix(bar, "pp") {
		t.Errorf("plain bar = %q, want ww..pp", bar)
	}
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what it
// wrote (cmdTimeline writes the JSON envelope straight to os.Stdout).
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	_ = w.Close()
	os.Stdout = orig
	return <-done
}

// writeDay writes events as one-JSON-per-line into the day-file dir/day.jsonl.
func writeDay(t *testing.T, dir, day string, evs ...history.Event) {
	t.Helper()
	var b bytes.Buffer
	for _, ev := range evs {
		line, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(history.DayPath(dir, day), b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTimelineJSONPlanWindowFlag(t *testing.T) {
	dir := t.TempDir()
	// A recent usage_sample lands in both today's display window and the rolling
	// 5h plan window. opus input 1M tokens → $5.00.
	t0 := time.Now().Add(-time.Minute)
	day := t0.Format("2006-01-02")
	writeDay(t, dir, day, history.Event{
		Ts: t0, Type: history.EventUsageSample, PID: 1, SessionID: "s1",
		Model: "claude-opus-4-8", TokIn: 1_000_000,
	})

	out := captureStdout(t, func() {
		cmdTimeline([]string{"--dir", dir, "--day", day, "--json", "--plan-window"})
	})

	var env struct {
		Lanes      []history.Swimlane  `json:"lanes"`
		Totals     history.Totals      `json:"totals"`
		PlanWindow *history.PlanWindow `json:"plan_window"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v\n%s", err, out)
	}
	if env.PlanWindow == nil {
		t.Fatalf("plan_window absent with --plan-window:\n%s", out)
	}
	if env.PlanWindow.Hours != planWindowHours {
		t.Errorf("plan_window.hours = %v, want %d", env.PlanWindow.Hours, planWindowHours)
	}
	if env.PlanWindow.CostUSD < 4.99 || env.PlanWindow.CostUSD > 5.01 {
		t.Errorf("plan_window.cost_usd = %v, want ~5.00", env.PlanWindow.CostUSD)
	}
	if env.Totals.CostUSD < 4.99 || env.Totals.CostUSD > 5.01 {
		t.Errorf("totals.cost_usd = %v, want ~5.00", env.Totals.CostUSD)
	}
	if len(env.Lanes) != 1 || env.Lanes[0].CostUSD < 4.99 {
		t.Errorf("lane cost_usd not carried: %+v", env.Lanes)
	}
}

// The ctl is the seam where the post-check runs, between BuildSwimlanes and
// everything that derives a number from it — so the flag has to reach --json, the
// text renderer, and every dashboard provider identically. These pin that seam.
//
// A --day query for a PAST day bounds the lanes at the next local midnight, which
// is never clamped by `now`; that is the "window bound" half of the reason string.
func TestTimelineSuspectPostCheck(t *testing.T) {
	// A ghost on a closed day: discovered, went to work, never seen again, no
	// session_end. Its lane is stretched from 08:00 to the next midnight (16h).
	setup := func(t *testing.T) (dir, day string, startedAt time.Time) {
		t.Helper()
		dir = t.TempDir()
		d := time.Now().AddDate(0, 0, -7)
		day = d.Format("2006-01-02")
		startedAt = time.Date(d.Year(), d.Month(), d.Day(), 8, 0, 0, 0, time.Local)
		writeDay(t, dir, day,
			history.Event{Ts: startedAt, Type: history.EventSessionStart, PID: 4242, Agent: "claude"},
			history.Event{Ts: startedAt, Type: history.EventTransition, PID: 4242, SessionID: "ghost-1", To: "working"},
		)
		return dir, day, startedAt
	}

	type envelope struct {
		Lanes   []history.Swimlane `json:"lanes"`
		Summary history.Summary    `json:"summary"`
	}

	t.Run("should carry the suspect flag and its reason into the JSON envelope when no session_end closed a lane", func(t *testing.T) {
		dir, day, startedAt := setup(t)
		out := captureStdout(t, func() {
			cmdTimeline([]string{"--dir", dir, "--day", day, "--json"})
		})
		var env envelope
		if err := json.Unmarshal([]byte(out), &env); err != nil {
			t.Fatalf("unmarshal envelope: %v\n%s", err, out)
		}
		if len(env.Lanes) != 1 {
			t.Fatalf("got %d lanes, want 1\n%s", len(env.Lanes), out)
		}
		lane := env.Lanes[0]
		if !lane.Suspect {
			t.Fatalf("lane not flagged:\n%s", out)
		}
		if !strings.Contains(lane.SuspectReason, "window bound") {
			t.Errorf("reason = %q, want it to name the window bound for a closed-day query", lane.SuspectReason)
		}
		if !lane.SuspectSince.Equal(startedAt) {
			t.Errorf("suspect_since = %v, want the last observed instant %v", lane.SuspectSince, startedAt)
		}
		// Annotated, not truncated: the lane still runs to the bound so the operator
		// can see exactly what the daemon believed.
		if want := startedAt.Add(16 * time.Hour); !lane.End.Equal(want) {
			t.Errorf("lane end = %v, want the untouched bound %v", lane.End, want)
		}
		if env.Summary.SuspectLanes != 1 || env.Summary.SuspectDuration != 16*time.Hour {
			t.Errorf("summary suspect = %d lanes / %v, want 1 / 16h", env.Summary.SuspectLanes, env.Summary.SuspectDuration)
		}
		// …and the 16 stretched hours are out of the headline numbers.
		if env.Summary.AttentionUnion != 0 || env.Summary.ByStatus["working"] != 0 {
			t.Errorf("attention_union = %v, working = %v, want the suspect tail excluded from both",
				env.Summary.AttentionUnion, env.Summary.ByStatus["working"])
		}
	})

	t.Run("should attribute the stretch to now when an open-ended range reaches the present", func(t *testing.T) {
		dir, day, _ := setup(t)
		out := captureStdout(t, func() {
			cmdTimeline([]string{"--dir", dir, "--since", day, "--json"})
		})
		var env envelope
		if err := json.Unmarshal([]byte(out), &env); err != nil {
			t.Fatalf("unmarshal envelope: %v\n%s", err, out)
		}
		if len(env.Lanes) != 1 || !env.Lanes[0].Suspect {
			t.Fatalf("lane not flagged:\n%s", out)
		}
		// `--since X` with no `--until` ends at wall-clock now, not a midnight, so
		// this is a live-day ghost and must not be excused as a midnight crossing.
		if !strings.Contains(env.Lanes[0].SuspectReason, "stretched to now") {
			t.Errorf("reason = %q, want it to name `now` for an open-ended range", env.Lanes[0].SuspectReason)
		}
	})

	// --suspect-cap is the operator's whole interface to this check, so it has to
	// govern both halves. A pattern quiet enough to need a looser lane cap has
	// subagents quiet with it, and a flag that moved only the lane half would leave
	// the operator flagged by the half they had no way to reach.
	t.Run("should loosen the lane half when given a cap above the stretch", func(t *testing.T) {
		dir, day, _ := setup(t)
		out := captureStdout(t, func() {
			cmdTimeline([]string{"--dir", dir, "--day", day, "--json", "--suspect-cap", "20h"})
		})
		var env envelope
		if err := json.Unmarshal([]byte(out), &env); err != nil {
			t.Fatalf("unmarshal envelope: %v\n%s", err, out)
		}
		if env.Lanes[0].Suspect || env.Summary.SuspectLanes != 0 {
			t.Errorf("16h lane still flagged under a 20h cap:\n%s", out)
		}
	})

	t.Run("should loosen the subagent half in proportion when given a non-default cap", func(t *testing.T) {
		// The same ghost, but with an unpaired subagent spawned three hours before the
		// bound: over the 2h default, under the 5h that a 10h lane cap scales to.
		spawnGhost := func(t *testing.T) (dir, day string) {
			t.Helper()
			dir, day, startedAt := setup(t)
			writeDay(t, dir, day,
				history.Event{Ts: startedAt, Type: history.EventSessionStart, PID: 4242, Agent: "claude"},
				history.Event{Ts: startedAt, Type: history.EventTransition, PID: 4242, SessionID: "ghost-1", To: "working"},
				history.Event{Ts: startedAt.Add(13 * time.Hour), Type: history.EventSubagentSpawn,
					PID: 4242, SessionID: "ghost-1", AgentID: "aphantom-0001"},
			)
			return dir, day
		}
		spans := func(t *testing.T, args ...string) []history.SubagentSpan {
			t.Helper()
			out := captureStdout(t, func() { cmdTimeline(args) })
			var env envelope
			if err := json.Unmarshal([]byte(out), &env); err != nil {
				t.Fatalf("unmarshal envelope: %v\n%s", err, out)
			}
			if len(env.Lanes) != 1 || len(env.Lanes[0].Subagents) != 1 {
				t.Fatalf("want one lane carrying one span:\n%s", out)
			}
			return env.Lanes[0].Subagents
		}

		dir, day := spawnGhost(t)
		if got := spans(t, "--dir", dir, "--day", day, "--json"); !got[0].Suspect {
			t.Fatalf("3h unpaired span not flagged at the 2h default: %+v", got[0])
		}
		dir, day = spawnGhost(t)
		if got := spans(t, "--dir", dir, "--day", day, "--json", "--suspect-cap", "10h"); got[0].Suspect {
			t.Errorf("span still flagged under a 10h cap (5h scaled): %s", got[0].SuspectReason)
		}
	})

	t.Run("should flag nothing when the suspect cap is disabled", func(t *testing.T) {
		dir, day, _ := setup(t)
		out := captureStdout(t, func() {
			cmdTimeline([]string{"--dir", dir, "--day", day, "--json", "--suspect-cap", "0"})
		})
		var env envelope
		if err := json.Unmarshal([]byte(out), &env); err != nil {
			t.Fatalf("unmarshal envelope: %v\n%s", err, out)
		}
		if env.Lanes[0].Suspect || env.Summary.SuspectLanes != 0 {
			t.Errorf("check ran with --suspect-cap 0:\n%s", out)
		}
		if env.Summary.ByStatus["working"] != 16*time.Hour {
			t.Errorf("working = %v, want the full unchecked 16h back", env.Summary.ByStatus["working"])
		}
	})
}

func TestRenderSwimlanesMarksSuspectLanes(t *testing.T) {
	t.Run("should mark the bar and print the reason when a lane is suspect", func(t *testing.T) {
		lanes := []history.Swimlane{{
			SessionID: "ghost-123456", PID: 4242, Name: "debug-paused-agent-pump",
			Start: atSec(0), End: atSec(20),
			Intervals: []history.Interval{{Status: "working", Start: atSec(0), End: atSec(20)}},
			Suspect:   true, SuspectSince: atSec(0),
			SuspectReason: `unclosed lane stretched to now: final "working" interval 4h53m52s >= 4h0m0s cap`,
		}}
		summary := history.Summary{From: atSec(0), To: atSec(20), Sessions: 1,
			ByStatus: map[string]time.Duration{}, SuspectLanes: 1, SuspectDuration: 4*time.Hour + 53*time.Minute}
		report := history.SuspectReport{Lanes: 1, Duration: summary.SuspectDuration}

		out := captureStdout(t, func() {
			renderSwimlanes(os.Stdout, "2026-07-22", lanes, summary, history.Totals{}, report, 8, false)
		})

		if !strings.Contains(out, "wwwwwwww!") {
			t.Errorf("suspect bar is not marked with '!':\n%s", out)
		}
		if !strings.Contains(out, "suspect (excluded from the totals above)") {
			t.Errorf("no suspect section:\n%s", out)
		}
		if !strings.Contains(out, `final "working" interval 4h53m52s`) {
			t.Errorf("the reason is not surfaced:\n%s", out)
		}
	})

	t.Run("should print no suspect section when nothing was flagged", func(t *testing.T) {
		lanes := []history.Swimlane{{
			SessionID: "clean-1", PID: 7, Start: atSec(0), End: atSec(20),
			Intervals: []history.Interval{{Status: "working", Start: atSec(0), End: atSec(20)}},
		}}
		summary := history.Summary{From: atSec(0), To: atSec(20), Sessions: 1, ByStatus: map[string]time.Duration{}}
		out := captureStdout(t, func() {
			renderSwimlanes(os.Stdout, "2026-07-22", lanes, summary, history.Totals{}, history.SuspectReport{}, 8, false)
		})
		if strings.Contains(out, "suspect") || strings.Contains(out, "!") {
			t.Errorf("clean day should carry no suspect noise:\n%s", out)
		}
	})
}

func TestTimelineJSONPlanWindowOmittedByDefault(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Now().Add(-time.Minute)
	day := t0.Format("2006-01-02")
	writeDay(t, dir, day, history.Event{
		Ts: t0, Type: history.EventSessionStart, PID: 1, SessionID: "s1",
	})

	out := captureStdout(t, func() {
		cmdTimeline([]string{"--dir", dir, "--day", day, "--json"})
	})
	if strings.Contains(out, "plan_window") {
		t.Errorf("plan_window should be omitted without the flag:\n%s", out)
	}
}
