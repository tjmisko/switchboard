package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/tjmisko/switchboard/internal/state"
)

func TestRenderSlotStatusAndFlags(t *testing.T) {
	snap := state.Snapshot{
		Sessions: []state.Session{
			{PID: 4821, CWD: "/home/u/proj", Focused: true, Claude: &state.ClaudeInfo{Status: "working"}},
		},
	}
	out := renderSlot(snap, 0)
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
	out := renderSlot(snap, 0)
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
	out := renderSlot(state.Snapshot{}, 0)
	if !slices.Contains(out.Class, "empty") {
		t.Errorf("out-of-range slot should be 'empty': %v", out.Class)
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
	out := renderSlot(snap, 0)
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
