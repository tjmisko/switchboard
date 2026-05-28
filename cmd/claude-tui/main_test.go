package main

import (
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/state"
)

func TestRenderSnapshotListsSessions(t *testing.T) {
	snap := state.Snapshot{
		Sessions: []state.Session{
			{
				PID: 4821, CWD: "/home/u/Projects/switchboard", Focused: true,
				Hyprland: &state.HyprlandInfo{Workspace: "4"},
				Claude:   &state.ClaudeInfo{Status: "working"},
			},
			{PID: 5102, CWD: "/home/u/other"}, // no claude block → unknown
		},
		Capabilities: &state.Capabilities{Observe: true, Navigate: true, WM: "hyprland", Terminal: "wezterm"},
	}

	got := renderSnapshot(snap, "/home/u", false)

	for _, want := range []string{
		"2 sessions",
		"navigate · wm=hyprland term=wezterm",
		"working",
		"~/Projects/switchboard", // home abbreviated
		"ws 4",
		"pid 4821",
		"unknown", // the session with no claude block
		"~/other",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("frame missing %q\n--- frame ---\n%s", want, got)
		}
	}
	// The focused session is marked.
	if !strings.Contains(got, "*") {
		t.Error("focused session not marked with *")
	}
	// color=false → no ANSI escapes leak in.
	if strings.Contains(got, "\033[") {
		t.Error("plain render leaked ANSI escapes")
	}
}

func TestRenderSnapshotEmptyAndNoCaps(t *testing.T) {
	got := renderSnapshot(state.Snapshot{UpdatedAt: time.Now()}, "/home/u", false)
	if !strings.Contains(got, "0 sessions") {
		t.Errorf("want '0 sessions', got:\n%s", got)
	}
	if !strings.Contains(got, "no claude sessions") {
		t.Errorf("want empty-state line, got:\n%s", got)
	}
	// nil capabilities → bare "observe" tier, no panic.
	if !strings.Contains(got, "observe") {
		t.Errorf("want 'observe' tier with nil caps, got:\n%s", got)
	}
}

func TestAbbrevHome(t *testing.T) {
	if got := abbrevHome("/home/u/proj", "/home/u"); got != "~/proj" {
		t.Errorf("abbrevHome = %q, want ~/proj", got)
	}
	if got := abbrevHome("/etc/x", "/home/u"); got != "/etc/x" {
		t.Errorf("abbrevHome(non-home) = %q, want unchanged", got)
	}
	if got := abbrevHome("", "/home/u"); got != "(unknown cwd)" {
		t.Errorf("abbrevHome(empty) = %q", got)
	}
}
