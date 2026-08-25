package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tjmisko/switchboard/internal/state"
)

var testOptions = renderOptions{
	maxSessions: 10,
	ctlPath:     "/opt/switchboard/bin/switchboard-ctl",
	colors: palette{
		working:    "#8ABEB7",
		idle:       "#F0C674",
		permission: "#A54242",
		unknown:    "#707880",
	},
}

func TestRenderSnapshotRendersProviderNeutralClickableChips(t *testing.T) {
	snap := state.Snapshot{Sessions: []state.Session{
		{PID: 41, Agent: state.AgentKindClaude, Claude: &state.AgentInfo{Status: state.StatusWorking}},
		{PID: 42, Agent: state.AgentKindCodex, Codex: &state.AgentInfo{Status: state.StatusIdle}},
		{PID: 43, Agent: state.AgentKindCodex, Codex: &state.AgentInfo{Status: state.StatusPermission}},
		{PID: 44, AgentGraph: &state.AgentGraph{Summary: state.AgentGraphSummary{Status: state.StatusDelegating}}},
	}}

	got := renderSnapshot(snap, []string{"sb-build", "sb-review", "sb-prompt", "sb-agents"}, testOptions)
	want := strings.Join([]string{
		"%{A1:'/opt/switchboard/bin/switchboard-ctl' focus pid\\:41:}%{F#8ABEB7}● sb-build%{F-}%{A}",
		"%{A1:'/opt/switchboard/bin/switchboard-ctl' focus pid\\:42:}%{F#F0C674}● sb-review%{F-}%{A}",
		"%{A1:'/opt/switchboard/bin/switchboard-ctl' focus pid\\:43:}%{F#A54242}● sb-prompt%{F-}%{A}",
		"%{A1:'/opt/switchboard/bin/switchboard-ctl' focus pid\\:44:}%{F#8ABEB7}● sb-agents%{F-}%{A}",
	}, "  ")
	if got != want {
		t.Fatalf("renderSnapshot() =\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderSnapshotBoundsSessionsAndShowsOverflow(t *testing.T) {
	options := testOptions
	options.maxSessions = 2
	snap := state.Snapshot{Sessions: []state.Session{{PID: 1}, {PID: 2}, {PID: 3}, {PID: 4}}}
	got := renderSnapshot(snap, []string{"one", "two"}, options)
	if strings.Contains(got, `pid\:3`) || !strings.HasSuffix(got, "%{F#707880}+2%{F-}") {
		t.Fatalf("bounded render = %q", got)
	}
}

func TestRenderSnapshotUnlimitedWhenMaxIsZero(t *testing.T) {
	options := testOptions
	options.maxSessions = 0
	snap := state.Snapshot{Sessions: []state.Session{{PID: 1}, {PID: 2}, {PID: 3}}}
	got := renderSnapshot(snap, []string{"one", "two", "three"}, options)
	if !strings.Contains(got, `pid\:3`) || strings.Contains(got, "+1") {
		t.Fatalf("unlimited render = %q", got)
	}
}

func TestRenderSnapshotHeadlessAndSuspendedAreMuted(t *testing.T) {
	snap := state.Snapshot{Sessions: []state.Session{
		{PID: 7, Headless: true, Claude: &state.AgentInfo{Status: state.StatusWorking}},
		{PID: 8, Suspended: true, Codex: &state.AgentInfo{Status: state.StatusPermission}},
	}}
	got := renderSnapshot(snap, []string{"headless", "stopped"}, testOptions)
	if strings.Contains(got, "pid:7") {
		t.Fatalf("headless chip must not be clickable: %q", got)
	}
	if !strings.Contains(got, "%{F#707880}● headless%{F-}") ||
		!strings.Contains(got, `pid\:8:}%{F#707880}● stopped`) {
		t.Fatalf("muted render = %q", got)
	}
}

func TestEscapeActionCommandEscapesEveryColon(t *testing.T) {
	got := escapeActionCommand("'/opt/with:colon/ctl' focus pid:42")
	want := `'/opt/with\:colon/ctl' focus pid\:42`
	if got != want {
		t.Fatalf("escapeActionCommand() = %q, want %q", got, want)
	}
}

func TestRenderSnapshotEscapesFormattingAndLineBreaks(t *testing.T) {
	snap := state.Snapshot{Sessions: []state.Session{{PID: 9}}}
	got := renderSnapshot(snap, []string{"bad%{A}  name\nnext"}, testOptions)
	if !strings.Contains(got, "bad％{A} name next") || strings.Contains(got, "\nnext") {
		t.Fatalf("escaped render = %q", got)
	}
}

func TestRenderSnapshotEmptyAndUnavailable(t *testing.T) {
	if got := renderSnapshot(state.Snapshot{}, nil, testOptions); got != "" {
		t.Fatalf("empty snapshot = %q", got)
	}
	if got := renderUnavailable(testOptions.colors); got != "%{F#707880}✕%{F-}" {
		t.Fatalf("unavailable = %q", got)
	}
}

func TestEmitterDeduplicatesButForgetForcesFirstLine(t *testing.T) {
	var buf bytes.Buffer
	e := &emitter{w: &buf}
	if !e.emit("") || e.emit("") {
		t.Fatal("first empty line should emit and its duplicate should not")
	}
	e.forget()
	if !e.emit("") {
		t.Fatal("forget should force the next line")
	}
	if got := buf.String(); got != "\n\n" {
		t.Fatalf("output = %q", got)
	}
}

func TestValidColor(t *testing.T) {
	for _, color := range []string{"#fff", "#F0C674", "#80282A2E"} {
		if !validColor(color) {
			t.Errorf("validColor(%q) = false", color)
		}
	}
	for _, color := range []string{"F0C674", "#12", "#xyzxyz", "#12345"} {
		if validColor(color) {
			t.Errorf("validColor(%q) = true", color)
		}
	}
}
