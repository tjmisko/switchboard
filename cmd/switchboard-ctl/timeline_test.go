package main

import (
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
	if !from.Equal(time.Date(2026, 6, 26, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("from = %v", from)
	}
	if !to.Equal(time.Date(2026, 6, 27, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("to (exclusive next day) = %v", to)
	}
	if label != "2026-06-26" {
		t.Errorf("label = %q", label)
	}
}

func TestResolveWindowRangeUntilInclusive(t *testing.T) {
	_, to, _ := resolveWindow("", "2026-06-20", "2026-06-26")
	// until is inclusive → exclusive bound is the next day.
	if !to.Equal(time.Date(2026, 6, 27, 0, 0, 0, 0, time.UTC)) {
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
		{999, "999"},        // boundary: just under 1k stays a plain count
		{1000, "1.0k"},      // boundary: 1k switches to k
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
