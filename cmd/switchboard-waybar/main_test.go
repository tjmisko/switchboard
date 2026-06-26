package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/barlayout"
	"github.com/tjmisko/switchboard/internal/projectname"
	"github.com/tjmisko/switchboard/internal/state"
)

// Test chips render on a bar wide enough that no abbreviation kicks in.
var (
	testAvail   = 100000.0
	testMetrics = barlayout.DefaultMetrics()
)

func TestSessionTooltipShowsStatusDuration(t *testing.T) {
	now := time.Date(2026, 6, 26, 14, 30, 0, 0, time.UTC)
	since := now.Add(-45 * time.Second)
	s := state.Session{
		PID: 4821, CWD: "/home/u/proj",
		Claude: &state.ClaudeInfo{Status: "permission", StatusSinceWire: &since},
	}
	tip := sessionTooltip(projectname.Config{}, s, now)
	if !strings.Contains(tip, "permission · 45s") {
		t.Errorf("tooltip should show the permission-wait duration:\n%s", tip)
	}
}

func TestSessionTooltipSuspendedShowsNoDuration(t *testing.T) {
	now := time.Date(2026, 6, 26, 14, 30, 0, 0, time.UTC)
	since := now.Add(-5 * time.Minute)
	s := state.Session{
		PID: 4821, CWD: "/home/u/proj", Suspended: true,
		Claude: &state.ClaudeInfo{Status: "working", StatusSinceWire: &since},
	}
	tip := sessionTooltip(projectname.Config{}, s, now)
	// Suspended status (and its clock) is stale; show "suspended", not a counter.
	if strings.Contains(tip, "5m") {
		t.Errorf("suspended session should not show a stale duration:\n%s", tip)
	}
	if !strings.Contains(tip, "suspended") {
		t.Errorf("suspended session should be labeled suspended:\n%s", tip)
	}
}

func TestRenderSlotStatusAndFlags(t *testing.T) {
	snap := state.Snapshot{
		Sessions: []state.Session{
			{PID: 4821, CWD: "/home/u/proj", Focused: true, Claude: &state.ClaudeInfo{Status: "working"}},
		},
	}
	out := renderSlot(snap, 0, testAvail, testMetrics)
	if !slices.Contains(out.Class, "working") {
		t.Errorf("class missing status 'working': %v", out.Class)
	}
	if !slices.Contains(out.Class, "focused") {
		t.Errorf("class missing 'focused': %v", out.Class)
	}
	if slices.Contains(out.Class, "suspended") {
		t.Errorf("non-suspended session should not carry 'suspended': %v", out.Class)
	}
}

func TestRenderSlotSuspended(t *testing.T) {
	snap := state.Snapshot{
		Sessions: []state.Session{
			{PID: 4821, CWD: "/home/u/proj", Suspended: true, Claude: &state.ClaudeInfo{Status: "working"}},
		},
	}
	out := renderSlot(snap, 0, testAvail, testMetrics)
	if !slices.Contains(out.Class, "suspended") {
		t.Errorf("suspended session missing 'suspended' class: %v", out.Class)
	}
	// The underlying status class is still present so CSS can layer the two.
	if !slices.Contains(out.Class, "working") {
		t.Errorf("suspended chip dropped its status class: %v", out.Class)
	}
	if !strings.Contains(out.Tooltip, "suspended") {
		t.Errorf("tooltip should note suspension: %q", out.Tooltip)
	}
}

func TestRenderSlotEmpty(t *testing.T) {
	out := renderSlot(state.Snapshot{}, 0, testAvail, testMetrics)
	if !slices.Contains(out.Class, "empty") {
		t.Errorf("out-of-range slot should be 'empty': %v", out.Class)
	}
}

// When the bar is crowded the chip text is abbreviated with an ellipsis, but
// the tooltip still carries the full, untruncated name.
func TestRenderSlotAbbreviatesWhenCrowded(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // hermetic: no real projectname config
	snap := state.Snapshot{Sessions: []state.Session{
		{PID: 1, CWD: "/home/u/aaaaaaaaaaaaaaaaaaaa", Claude: &state.ClaudeInfo{Status: "working"}},
		{PID: 2, CWD: "/home/u/bbbbbbbbbbbbbbbbbbbb", Claude: &state.ClaudeInfo{Status: "working"}},
	}}
	unit := barlayout.Metrics{CharPx: 1, ChipFixedPx: 0}

	if full := renderSlot(snap, 0, 100000, unit); strings.HasSuffix(full.Text, "…") {
		t.Errorf("a wide bar should not abbreviate: %q", full.Text)
	}

	narrow := renderSlot(snap, 0, 10, unit)
	if !strings.HasSuffix(narrow.Text, "…") {
		t.Errorf("a crowded bar should abbreviate with an ellipsis: %q", narrow.Text)
	}
	if !strings.Contains(narrow.Tooltip, "aaaaaaaa") {
		t.Errorf("tooltip should keep the full name, got %q", narrow.Tooltip)
	}
}

// A delegating chip (idle main thread, subagents in flight) renders GREEN: its
// primary class is "working" so existing CSS paints it green with no change, and
// a secondary "delegating" class rides along for an optional badge. The tooltip
// explains the green with the agent count.
func TestRenderSlotDelegating(t *testing.T) {
	snap := state.Snapshot{
		Sessions: []state.Session{
			{PID: 4821, CWD: "/home/u/proj", Claude: &state.ClaudeInfo{
				Status: state.StatusDelegating, InFlightSubagents: 2,
			}},
		},
	}
	out := renderSlot(snap, 0, testAvail, testMetrics)
	if !slices.Contains(out.Class, "working") {
		t.Errorf("delegating chip must carry the green 'working' class: %v", out.Class)
	}
	if !slices.Contains(out.Class, "delegating") {
		t.Errorf("delegating chip missing its 'delegating' marker class: %v", out.Class)
	}
	if out.Alt != "working" {
		t.Errorf("Alt = %q, want working (green)", out.Alt)
	}
	if !strings.Contains(out.Tooltip, "delegating · 2 agents") {
		t.Errorf("tooltip should explain the green with the agent count: %q", out.Tooltip)
	}
}
