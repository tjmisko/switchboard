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
