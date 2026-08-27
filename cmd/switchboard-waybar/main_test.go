package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/barlayout"
	sblabel "github.com/tjmisko/switchboard/internal/label"
	"github.com/tjmisko/switchboard/internal/projectname"
	"github.com/tjmisko/switchboard/internal/state"
)

// Test chips render on a bar wide enough that no abbreviation kicks in.
var (
	testAvail   = 100000.0
	testMetrics = barlayout.DefaultMetrics()
)

func TestRenderSlotCodexNameDoesNotMoveWithTerminalSpinner(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rootID := "01890f00-0000-7000-8000-000000000001"
	base := state.Session{
		PID: 1, CWD: "/irrelevant/switchboard", Agent: state.AgentKindCodex,
		AgentGraph: &state.AgentGraph{
			RootID: rootID, Summary: state.AgentGraphSummary{Status: state.StatusWorking},
			Nodes: []state.AgentNode{{ID: rootID}},
		},
	}
	names := &nameConfig{}
	labels := &sblabel.NameCache{}
	var got []waybarOutput
	for _, title := range []string{"⠋ " + rootID, "⠙ " + rootID, "Working · " + rootID} {
		s := base
		s.Wezterm = &state.WeztermInfo{WindowTitle: title}
		got = append(got, renderSlot(state.Snapshot{Sessions: []state.Session{s}}, 0, testAvail, testMetrics, names, labels))
	}
	for i, out := range got {
		if out.Text != "sb-01" {
			t.Errorf("frame %d chip text = %q, want compact stable id", i, out.Text)
		}
		if !slices.Contains(out.Class, state.StatusWorking) {
			t.Errorf("frame %d chip class = %v, want working color", i, out.Class)
		}
		if i > 0 && (out.Text != got[0].Text || out.Tooltip != got[0].Tooltip || out.Alt != got[0].Alt || !slices.Equal(out.Class, got[0].Class)) {
			t.Errorf("frame %d changed visible output after a title-only spinner frame: %#v != %#v", i, out, got[0])
		}
	}
}

func TestRenderSlotCodexExplicitNameReplacesShortID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rootID := "01890f00-0000-7000-8000-000000000001"
	s := state.Session{
		PID: 1, CWD: "/irrelevant/switchboard", Agent: state.AgentKindCodex,
		AgentGraph: &state.AgentGraph{
			RootID: rootID, Summary: state.AgentGraphSummary{Status: state.StatusIdle},
			Nodes: []state.AgentNode{{ID: rootID, Nickname: "my-short-name"}},
		},
	}
	out := renderSlot(state.Snapshot{Sessions: []state.Session{s}}, 0, testAvail, testMetrics, &nameConfig{}, &sblabel.NameCache{})
	if out.Text != "sb-my-short-name" {
		t.Fatalf("renamed Codex chip = %q, want sb-my-short-name", out.Text)
	}
}

func TestRenderSlotMarksUnboundAggregateRowObserveOnly(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	session := state.Session{
		PID: 4, Hostname: "remote", Remote: true, CWD: "/work", Agent: state.AgentKindClaude,
		Claude: &state.AgentInfo{Status: state.StatusIdle},
	}
	out := renderSlot(state.Snapshot{Sessions: []state.Session{session}}, 0, testAvail, testMetrics, &nameConfig{}, &sblabel.NameCache{})
	if !slices.Contains(out.Class, "unnavigable") {
		t.Fatalf("classes = %v", out.Class)
	}
	if !strings.Contains(out.Tooltip, "observe only (pane not bound)") {
		t.Fatalf("tooltip = %q", out.Tooltip)
	}
}

func TestRenderSlotTagsRemoteSessionForNestedPill(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	session := state.Session{
		PID: 7, Hostname: "boxy", Remote: true, Navigable: true, CWD: "/work/proj",
		Agent: state.AgentKindClaude, Claude: &state.AgentInfo{Status: state.StatusWorking},
	}
	out := renderSlot(state.Snapshot{Sessions: []state.Session{session}}, 0, testAvail, testMetrics, &nameConfig{}, &sblabel.NameCache{})
	if !slices.Contains(out.Class, "remote") {
		t.Errorf("remote session missing 'remote' class: %v", out.Class)
	}
	// The nesting is additive: it must not displace the status fill or the
	// focus ring the CSS layers underneath it.
	if !slices.Contains(out.Class, state.StatusWorking) {
		t.Errorf("remote chip dropped its status class: %v", out.Class)
	}
}

func TestRenderSlotLeavesLocalSessionUnnested(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	session := state.Session{
		PID: 8, CWD: "/work/proj", Agent: state.AgentKindClaude,
		Claude: &state.AgentInfo{Status: state.StatusWorking},
	}
	out := renderSlot(state.Snapshot{Sessions: []state.Session{session}}, 0, testAvail, testMetrics, &nameConfig{}, &sblabel.NameCache{})
	if slices.Contains(out.Class, "remote") {
		t.Errorf("local session must not carry 'remote': %v", out.Class)
	}
}

// A remote chip is styled as a nested pill, but the CSS pays for the extra ring
// out of its own padding so its box matches a local chip's. The fit must
// therefore be blind to remoteness: if a remote session widened the row, the
// labels the user navigates by would re-abbreviate every time one appeared or
// dropped. This pins the property at the layer that would break — the chip text
// — rather than trusting the CSS comment alone.
func TestRenderSlotFitIgnoresWhetherSessionsAreRemote(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	build := func(remote bool) []state.Session {
		var out []state.Session
		for i := range 8 {
			out = append(out, state.Session{
				PID: i + 1, Remote: remote, Navigable: true,
				Hostname: map[bool]string{true: "boxy", false: ""}[remote],
				CWD:      fmt.Sprintf("/work/project-with-a-long-name-%d", i),
				Agent:    state.AgentKindClaude,
				Claude:   &state.AgentInfo{Status: state.StatusWorking},
			})
		}
		return out
	}
	// Narrow enough that the row is genuinely crowded and Fit has to abbreviate.
	const narrow = 520
	for slot := range 8 {
		local := renderSlot(state.Snapshot{Sessions: build(false)}, slot, narrow, testMetrics, &nameConfig{}, &sblabel.NameCache{})
		remote := renderSlot(state.Snapshot{Sessions: build(true)}, slot, narrow, testMetrics, &nameConfig{}, &sblabel.NameCache{})
		// Guard against the comparison going vacuous: if a metrics change ever
		// left this row roomy enough to render in full, both sides would match
		// trivially and stop testing the budget at all.
		if !strings.HasSuffix(local.Text, "…") {
			t.Fatalf("slot %d rendered %q unabbreviated; widen the row or lengthen the labels", slot, local.Text)
		}
		if len([]rune(local.Text)) != len([]rune(remote.Text)) {
			t.Fatalf("slot %d: local text %q (%d runes) vs remote %q (%d runes); remoteness changed the fit",
				slot, local.Text, len([]rune(local.Text)), remote.Text, len([]rune(remote.Text)))
		}
	}
}

// pangoPlain reverses pangoEscape so hover assertions read as the card renders,
// not as the markup that carries it.
func pangoPlain(s string) string {
	return strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&").Replace(s)
}

func TestRemoteTooltipDoesNotResolveCWDOnLocalFilesystem(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	remoteDir := filepath.Join(root, "nested", "switchboard")
	if err := os.MkdirAll(remoteDir, 0o755); err != nil {
		t.Fatal(err)
	}
	s := state.Session{
		PID: 4, Hostname: "buildbox", Remote: true, CWD: remoteDir,
		Agent: state.AgentKindClaude, Claude: &state.AgentInfo{Status: state.StatusIdle},
	}
	tip := sessionTooltip(projectname.DefaultConfig(), &sblabel.NameCache{}, s, time.Now())
	// The full display name resolves from the basename alone, case-folded for
	// the small-caps run; the host follows on line 2.
	if !strings.Contains(tip, "<span variant='smallcaps'>switchboard</span>") {
		t.Fatalf("remote tooltip did not use the lexical remote project name:\n%s", tip)
	}
	if !strings.Contains(tip, "<b>buildbox</b>") {
		t.Fatalf("remote tooltip did not name the remote host:\n%s", tip)
	}
	if !strings.Contains(tip, "pid 4") {
		t.Fatalf("remote tooltip lost the process identity:\n%s", tip)
	}
	if !strings.Contains(tip, remoteDir) || strings.Contains(tip, "~/nested") {
		t.Fatalf("remote tooltip contracted source-host cwd as local HOME:\n%s", tip)
	}
}

func TestAgentFanoutRollsUpGraphWithoutAddingSlots(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	nodes := []state.AgentNode{{ID: "root"}}
	for i := range 8 {
		nodes = append(nodes, state.AgentNode{
			ID: fmt.Sprintf("child-%d", i), ParentID: "root", Nickname: fmt.Sprintf("child-%d", i),
			Runtime: agentgraph.RuntimeIdle, Lifecycle: agentgraph.LifecycleRunning, UpdatedAt: now.Add(-time.Minute),
		})
	}
	// Two that finished, contributing 30m and 1h to the cumulative total.
	nodes = append(nodes,
		state.AgentNode{
			ID: "done-a", ParentID: "root", Nickname: "done-a", Lifecycle: agentgraph.LifecycleCompleted,
			StartedAt: now.Add(-2 * time.Hour), CompletedAt: now.Add(-90 * time.Minute),
		},
		state.AgentNode{
			ID: "done-b", ParentID: "root", Nickname: "done-b", Lifecycle: agentgraph.LifecycleCompleted,
			StartedAt: now.Add(-3 * time.Hour), CompletedAt: now.Add(-2 * time.Hour),
		})
	graph := &state.AgentGraph{
		RootID: "root", ObservedAt: now.Add(-time.Second), FreshUntil: now.Add(time.Minute), Complete: true,
		Summary: state.AgentGraphSummary{
			Status: state.StatusPermission, LiveChildren: 8, WaitingNodes: 2, ErrorNodes: 1,
		},
		Nodes: nodes,
	}
	snap := state.Snapshot{Sessions: []state.Session{{
		PID: 1, CWD: "/home/u/proj", Agent: state.AgentKindCodex, AgentGraph: graph,
	}}}
	out := renderSlot(snap, 0, testAvail, testMetrics, &nameConfig{}, &sblabel.NameCache{})
	if !slices.Contains(out.Class, state.StatusPermission) {
		t.Fatalf("root class did not use projected summary: %v", out.Class)
	}
	// 10 fan-outs (root excluded), and only the two finished ones counted.
	if got, want := agentFanout(graph), "10 agents · 8 live · 2 waiting · 1 error · 1h30m done"; got != want {
		t.Fatalf("agentFanout = %q, want %q", got, want)
	}
	if extra := renderSlot(snap, 1, testAvail, testMetrics, &nameConfig{}, &sblabel.NameCache{}); !slices.Contains(extra.Class, "empty") {
		t.Fatalf("children must not create slots: %#v", extra)
	}
}

// The cumulative figure counts finished agents only. Live agents would each add
// a minute per minute, so a large fan-out would rewrite the tooltip every few
// seconds and bring back the hover flicker the card was redesigned to remove.
func TestAgentFanoutExcludesRunningAgentsFromCumulativeTime(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	graph := &state.AgentGraph{
		RootID:  "root",
		Summary: state.AgentGraphSummary{LiveChildren: 1},
		Nodes: []state.AgentNode{
			{ID: "root"},
			{ID: "live", ParentID: "root", Lifecycle: agentgraph.LifecycleRunning, StartedAt: now.Add(-5 * time.Hour)},
		},
	}
	got := agentFanout(graph)
	if strings.Contains(got, "done") || strings.Contains(got, "5h") {
		t.Fatalf("running agent leaked into cumulative time: %q", got)
	}
	if want := "1 agent · 1 live"; got != want {
		t.Fatalf("agentFanout = %q, want %q", got, want)
	}
}

func TestAgentFanoutSilentWhenNothingFannedOut(t *testing.T) {
	if got := agentFanout(nil); got != "" {
		t.Errorf("nil graph = %q, want empty", got)
	}
	lone := &state.AgentGraph{RootID: "root", Nodes: []state.AgentNode{{ID: "root"}}}
	if got := agentFanout(lone); got != "" {
		t.Errorf("root-only graph = %q, want empty (the root is not a fan-out)", got)
	}
}

// The whole point of the redesign: nothing on the card may tick faster than once
// a minute, or hovering it dismisses the hover. This walks a session forward
// second by second and pins the number of distinct tooltips produced.
func TestSessionTooltipDoesNotChangeAtSecondResolution(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	start := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	since := start
	s := state.Session{
		PID: 1, CWD: "/home/u/proj", StartedAt: start, Agent: state.AgentKindClaude,
		Claude: &state.AgentInfo{Status: state.StatusWorking, StatusSinceWire: &since},
		AgentGraph: &state.AgentGraph{
			RootID:  "root",
			Summary: state.AgentGraphSummary{LiveChildren: 12},
			Nodes:   agentNodes(12, start.Add(-5*time.Hour)),
		},
	}
	seen := map[string]bool{}
	for sec := range 90 {
		seen[sessionTooltip(projectname.DefaultConfig(), &sblabel.NameCache{}, s, start.Add(time.Duration(sec)*time.Second))] = true
	}
	// 90 seconds spans one minute boundary, so at most two distinct cards.
	if len(seen) > 2 {
		t.Fatalf("tooltip produced %d distinct strings over 90s, want <= 2 "+
			"(a per-second field is back; it will flicker the hover)", len(seen))
	}
}

// agentNodes builds n running child nodes started at the same instant.
func agentNodes(n int, startedAt time.Time) []state.AgentNode {
	nodes := []state.AgentNode{{ID: "root"}}
	for i := range n {
		nodes = append(nodes, state.AgentNode{
			ID: fmt.Sprintf("child-%d", i), ParentID: "root", Nickname: fmt.Sprintf("child-%d", i),
			Lifecycle: agentgraph.LifecycleRunning, StartedAt: startedAt,
		})
	}
	return nodes
}

func TestSessionTooltipWithoutAgentGraphKeepsLegacyDetail(t *testing.T) {
	s := state.Session{PID: 1, CWD: "/tmp/x", Claude: &state.AgentInfo{Status: state.StatusDelegating, InFlightSubagents: 2}}
	tip := sessionTooltip(projectname.Config{}, &sblabel.NameCache{}, s, time.Now())
	if !strings.Contains(tip, "delegating · 2 agents") {
		t.Fatalf("legacy fallback detail missing:\n%s", tip)
	}
	if strings.Contains(tip, "\nagents\n") {
		t.Fatalf("graph tree appeared without agent_graph:\n%s", tip)
	}
}

func TestSessionTooltipWithAgentGraphDoesNotDuplicateLegacyDetail(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	s := state.Session{
		PID: 1, CWD: "/tmp/x",
		Claude: &state.AgentInfo{
			Status: state.StatusDelegating, InFlightSubagents: 3,
			Workflows: []state.WorkflowStatus{{RunID: "legacy-run", Name: "legacy-workflow", InFlight: 3}},
		},
		AgentGraph: &state.AgentGraph{
			RootID: "root", ObservedAt: now.Add(-time.Second), FreshUntil: now.Add(time.Minute),
			Nodes: []state.AgentNode{{ID: "root"}, {ID: "child", ParentID: "root", Nickname: "canonical", Runtime: agentgraph.RuntimeActive}},
		},
	}
	tip := sessionTooltip(projectname.Config{}, &sblabel.NameCache{}, s, now)
	if !strings.Contains(tip, "1 agent") {
		t.Fatalf("graph roll-up missing:\n%s", tip)
	}
	// The raw in-flight count is what the roll-up already says, so it goes...
	if strings.Contains(tip, "3 agents") {
		t.Fatalf("legacy in-flight count duplicated the graph roll-up:\n%s", tip)
	}
	// ...but the workflow's NAME is not in the roll-up, so it stays.
	if !strings.Contains(tip, "legacy-workflow") {
		t.Fatalf("workflow name is not derivable from the roll-up and must survive:\n%s", tip)
	}
}

// Pango shrinks only LOWERCASE letters in a small-caps run, so a mixed-case
// display name would render at two different heights. The card folds the case
// first so every title is one uniform height.
func TestSessionTooltipLowercasesTheSmallCapsTitle(t *testing.T) {
	cfg := projectname.Config{Rules: []projectname.ProjectRule{
		{Match: []string{"webapp"}, Canonical: "sspi", Full: "SSPI Data Webapp"},
	}}
	s := state.Session{PID: 1, CWD: "/home/u/webapp", Claude: &state.AgentInfo{Status: state.StatusWorking}}
	tip := sessionTooltip(cfg, &sblabel.NameCache{}, s, time.Now())
	if !strings.Contains(tip, "<span variant='smallcaps'>sspi data webapp</span>") {
		t.Errorf("title should be case-folded for the small-caps run:\n%s", tip)
	}
	if strings.Contains(tip, "SSPI Data Webapp") {
		t.Errorf("mixed-case title leaked through:\n%s", tip)
	}
}

// The name and the path answer one question together, so they share line 1 —
// ahead of the host, which takes line 2.
func TestSessionTooltipPairsProjectNameWithPathAheadOfHost(t *testing.T) {
	s := state.Session{PID: 1, CWD: "/home/u/webapp", Claude: &state.AgentInfo{Status: state.StatusWorking}}
	lines := strings.Split(sessionTooltip(projectname.Config{}, &sblabel.NameCache{}, s, time.Now()), "\n")
	if len(lines) < 2 {
		t.Fatalf("card too short: %q", lines)
	}
	if !strings.Contains(lines[0], "smallcaps") || !strings.Contains(lines[0], "/home/u/webapp") {
		t.Errorf("line 1 should pair the project name with its path, got %q", lines[0])
	}
	if !strings.Contains(lines[1], "<b>") {
		t.Errorf("line 2 should be the host, got %q", lines[1])
	}
}

func TestSessionTooltipShowsStatusDuration(t *testing.T) {
	now := time.Date(2026, 6, 26, 14, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		age  time.Duration
		want string
	}{
		// Under a minute the card floors rather than counting seconds: a
		// per-second field rewrites the tooltip and dismisses an open hover.
		{"should floor a sub-minute wait", 45 * time.Second, "permission · <1m"},
		{"should show minutes past the floor", 7 * time.Minute, "permission · 7m"},
		{"should show hours and minutes", 2*time.Hour + 5*time.Minute, "permission · 2h05m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			since := now.Add(-tc.age)
			s := state.Session{
				PID: 4821, CWD: "/home/u/proj",
				Claude: &state.ClaudeInfo{Status: "permission", StatusSinceWire: &since},
			}
			tip := pangoPlain(sessionTooltip(projectname.Config{}, nil, s, now))
			if !strings.Contains(tip, tc.want) {
				t.Errorf("tooltip should contain %q:\n%s", tc.want, tip)
			}
		})
	}
}

func TestSessionTooltipSuspendedShowsNoDuration(t *testing.T) {
	now := time.Date(2026, 6, 26, 14, 30, 0, 0, time.UTC)
	since := now.Add(-5 * time.Minute)
	s := state.Session{
		PID: 4821, CWD: "/home/u/proj", Suspended: true,
		Claude: &state.ClaudeInfo{Status: "working", StatusSinceWire: &since},
	}
	tip := sessionTooltip(projectname.Config{}, nil, s, now)
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
	out := renderSlot(snap, 0, testAvail, testMetrics, &nameConfig{}, &sblabel.NameCache{})
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
	out := renderSlot(snap, 0, testAvail, testMetrics, &nameConfig{}, &sblabel.NameCache{})
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
	out := renderSlot(state.Snapshot{}, 0, testAvail, testMetrics, &nameConfig{}, &sblabel.NameCache{})
	if !slices.Contains(out.Class, "empty") {
		t.Errorf("out-of-range slot should be 'empty': %v", out.Class)
	}
}

// When the bar is crowded the chip text is abbreviated with an ellipsis, but
// the tooltip still carries the full, untruncated name.
func TestRenderSlotAbbreviatesWhenCrowded(t *testing.T) {
	// Hermetic: no real projectname config. XDG_CONFIG_HOME has to move too —
	// ConfigPath prefers it over $HOME, so setting HOME alone would still read
	// the developer's own projects.json when XDG_CONFIG_HOME is set.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	snap := state.Snapshot{Sessions: []state.Session{
		{PID: 1, CWD: "/home/u/aaaaaaaaaaaaaaaaaaaa", Claude: &state.ClaudeInfo{Status: "working"}},
		{PID: 2, CWD: "/home/u/bbbbbbbbbbbbbbbbbbbb", Claude: &state.ClaudeInfo{Status: "working"}},
	}}
	unit := barlayout.Metrics{CharPx: 1, ChipFixedPx: 0}

	if full := renderSlot(snap, 0, 100000, unit, &nameConfig{}, &sblabel.NameCache{}); strings.HasSuffix(full.Text, "…") {
		t.Errorf("a wide bar should not abbreviate: %q", full.Text)
	}

	narrow := renderSlot(snap, 0, 10, unit, &nameConfig{}, &sblabel.NameCache{})
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
	out := renderSlot(snap, 0, testAvail, testMetrics, &nameConfig{}, &sblabel.NameCache{})
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

// A delegating chip whose session is running an ultracode Workflow names the
// workflow and its progress instead of the bare agent count — the numbers the
// CLI's own "N/M agents done" line shows, so the bar and the pane agree.
func TestRenderSlotDelegatingShouldNameWorkflowAndProgressWhenRunActive(t *testing.T) {
	snap := state.Snapshot{
		Sessions: []state.Session{
			{PID: 4821, CWD: "/home/u/proj", Claude: &state.ClaudeInfo{
				Status: state.StatusDelegating, InFlightSubagents: 10,
				Workflows: []state.WorkflowStatus{{
					RunID: "wf_5e3cb808-2ac", Name: "simplification-audit",
					AgentsStarted: 17, AgentsDone: 7, InFlight: 10,
				}},
			}},
		},
	}
	out := renderSlot(snap, 0, testAvail, testMetrics, &nameConfig{}, &sblabel.NameCache{})
	if !strings.Contains(out.Tooltip, "workflow simplification-audit · 7/17 agents") {
		t.Errorf("tooltip should name the workflow and its progress: %q", out.Tooltip)
	}
}

// A run whose persisted script (and so its name) is missing still annotates
// with its opaque run id rather than falling back to the bare count.
func TestRenderSlotDelegatingShouldFallBackToRunIDWhenWorkflowNameUnknown(t *testing.T) {
	snap := state.Snapshot{
		Sessions: []state.Session{
			{PID: 4821, CWD: "/home/u/proj", Claude: &state.ClaudeInfo{
				Status: state.StatusDelegating, InFlightSubagents: 3,
				Workflows: []state.WorkflowStatus{
					{RunID: "wf_9f-1", AgentsStarted: 4, AgentsDone: 1, InFlight: 3},
					{RunID: "wf_9f-2", Name: "second", AgentsStarted: 2, AgentsDone: 2},
				},
			}},
		},
	}
	out := renderSlot(snap, 0, testAvail, testMetrics, &nameConfig{}, &sblabel.NameCache{})
	if !strings.Contains(out.Tooltip, "workflow wf_9f-1 · 1/4 agents (+1 more)") {
		t.Errorf("tooltip should show the run id and fold extra runs: %q", out.Tooltip)
	}
}

// --- naming the writer behind a red chip ----------------------------------

// blockedNow anchors the blocked-writer tooltip tests, so the "· 45s" the hover
// prints is pinned rather than racing the wall clock.
var blockedNow = time.Date(2026, 6, 26, 14, 30, 0, 0, time.UTC)

// blockedSession builds a red session with `writers` blocked (wire spelling;
// "main" = the main thread) alongside a real subagents/ dir carrying a teammate
// meta per entry of `names` (bare agent id -> teammate name).
func blockedSession(t *testing.T, writers []string, inflight int, names map[string]string) state.Session {
	t.Helper()
	dir := t.TempDir()
	transcriptPath := filepath.Join(dir, "sess.jsonl")
	subagentsDir := filepath.Join(dir, "sess", "subagents")
	if err := os.MkdirAll(subagentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for id, name := range names {
		body := fmt.Sprintf(`{"agentType":"general-purpose","name":%q,"taskKind":"in_process_teammate"}`, name)
		if err := os.WriteFile(filepath.Join(subagentsDir, "agent-"+id+".meta.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	since := blockedNow.Add(-45 * time.Minute)
	return state.Session{
		PID: 4821, CWD: "/home/u/proj",
		Claude: &state.ClaudeInfo{
			Status:            state.StatusPermission,
			StatusSinceWire:   &since,
			Transcript:        transcriptPath,
			PendingWriters:    writers,
			InFlightSubagents: inflight,
		},
	}
}

// The incident, end to end: the chip read the SESSION's name while the actual
// state was "the escalate-cleanup teammate is waiting on approval". The hover now
// names the writer, so the red is a decision the user can route without switching
// to the pane first.
func TestSessionTooltipShouldNameTheBlockedTeammate(t *testing.T) {
	s := blockedSession(t, []string{"af5bd126402ac16c7"}, 4,
		map[string]string{"af5bd126402ac16c7": "escalate-cleanup"})
	tip := sessionTooltip(projectname.Config{}, &sblabel.NameCache{}, s, blockedNow)
	if !strings.Contains(tip, "permission · escalate-cleanup · 45m") {
		t.Errorf("tooltip should name the blocked teammate:\n%s", tip)
	}
}

func TestSessionTooltipShouldNameEveryWriterWhenTwoAreBlockedAtOnce(t *testing.T) {
	s := blockedSession(t, []string{"af5bd126402ac16c7", "main"}, 2,
		map[string]string{"af5bd126402ac16c7": "escalate-cleanup"})
	tip := sessionTooltip(projectname.Config{}, &sblabel.NameCache{}, s, blockedNow)
	if !strings.Contains(tip, "permission · escalate-cleanup, main · 45m") {
		t.Errorf("tooltip should name both blocked writers:\n%s", tip)
	}
}

// The solo case: one session, no teammates, main thread blocked. "main" restates
// what the red already means, so the hover stays exactly as it was.
func TestSessionTooltipShouldLeaveASoloPermissionUnannotated(t *testing.T) {
	s := blockedSession(t, []string{"main"}, 0, nil)
	tip := sessionTooltip(projectname.Config{}, &sblabel.NameCache{}, s, blockedNow)
	if !strings.Contains(tip, "permission · 45m") {
		t.Errorf("solo permission tooltip should be status + duration only:\n%s", tip)
	}
	if strings.Contains(tip, "main") {
		t.Errorf("solo permission tooltip should not name the main thread:\n%s", tip)
	}
}

// Bar real estate is fitted as a SET (barlayout.Fit): a chip label that grew when
// a prompt appeared would re-abbreviate every other chip on the row, twice per
// prompt, and would break the stable chip identity the user navigates by. The
// writer's name belongs in the hover only.
func TestRenderSlotShouldNotPutTheBlockedWriterInTheChipLabel(t *testing.T) {
	blocked := blockedSession(t, []string{"af5bd126402ac16c7"}, 4,
		map[string]string{"af5bd126402ac16c7": "escalate-cleanup"})
	quiet := state.Session{
		PID: 4821, CWD: "/home/u/proj",
		Claude: &state.ClaudeInfo{Status: state.StatusWorking},
	}

	red := renderSlot(state.Snapshot{Sessions: []state.Session{blocked}}, 0, testAvail, testMetrics, &nameConfig{}, &sblabel.NameCache{})
	green := renderSlot(state.Snapshot{Sessions: []state.Session{quiet}}, 0, testAvail, testMetrics, &nameConfig{}, &sblabel.NameCache{})
	if red.Text != green.Text {
		t.Errorf("chip text changed when a prompt appeared: %q -> %q", green.Text, red.Text)
	}
	if strings.Contains(red.Text, "escalate-cleanup") {
		t.Errorf("chip text must not carry the blocked writer: %q", red.Text)
	}
	if !strings.Contains(red.Tooltip, "escalate-cleanup") {
		t.Errorf("the writer's name should still reach the hover: %q", red.Tooltip)
	}
}

// --- project-name config cache -------------------------------------------

// newNameConfigFixture points ConfigPath at a temp dir and returns (configPath,
// projectRoot). The project root carries a .git so ProjectRoot resolves to it
// exactly, instead of walking up into whatever repo /tmp happens to sit under.
func newNameConfigFixture(t *testing.T) (string, string) {
	t.Helper()
	cfgHome := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", cfgHome)

	root := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(cfgHome, "switchboard", "projects.json")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatal(err)
	}
	return cfgPath, root
}

func writeAbbrev(t *testing.T, cfgPath, root, abbrev string) {
	t.Helper()
	body := fmt.Sprintf(`{"projects":{%q:{"canonical":%q,"aliases":[%q]}}}`, root, abbrev, abbrev)
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func snapshotIn(root string) state.Snapshot {
	return state.Snapshot{Sessions: []state.Session{
		{PID: 4821, CWD: root, Claude: &state.ClaudeInfo{Status: "working"}},
	}}
}

// The bar's middle-click rename (~/.config/scripts/claude-abbrev-edit) rewrites
// projects.json out from under a running chip and expects the next snapshot to
// show the new abbreviation. A load-once cache would silently break that flow;
// the mtime check is what keeps it working.
func TestNameConfigShouldReloadWhenConfigFileChanges(t *testing.T) {
	cfgPath, root := newNameConfigFixture(t)
	writeAbbrev(t, cfgPath, root, "aaa")

	names := &nameConfig{}
	labels := &sblabel.NameCache{}
	if got := renderSlot(snapshotIn(root), 0, testAvail, testMetrics, names, labels); got.Text != "aaa-proj" {
		t.Fatalf("chip text = %q, want aaa-proj", got.Text)
	}

	// Rewrite as the rename hook would, forcing a later mtime so the test does
	// not depend on the filesystem's timestamp granularity.
	writeAbbrev(t, cfgPath, root, "zzz")
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(cfgPath, future, future); err != nil {
		t.Fatal(err)
	}

	got := renderSlot(snapshotIn(root), 0, testAvail, testMetrics, names, labels)
	if got.Text != "zzz-proj" {
		t.Errorf("chip text = %q, want zzz-proj — the rewritten config was not picked up", got.Text)
	}
	// The hover names the project in full, so a changed ABBREVIATION must not
	// show up there — that is the chip's job, asserted above.
	if !strings.Contains(got.Tooltip, "<span variant='smallcaps'>proj</span>") ||
		strings.Contains(got.Tooltip, "zzz") {
		t.Errorf("hover should keep the full display name, not the abbreviation: %q", got.Tooltip)
	}
}

// The cache must actually cache: with the file's stamp pinned, a changed body is
// deliberately NOT observed. Rewriting the content while restoring the original
// mtime and keeping the size identical is the only way to prove from the outside
// that the second render did not re-read the file.
func TestNameConfigShouldServeCachedConfigWhenFileStampUnchanged(t *testing.T) {
	cfgPath, root := newNameConfigFixture(t)
	writeAbbrev(t, cfgPath, root, "aaa")
	fi, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	names := &nameConfig{}
	labels := &sblabel.NameCache{}
	if got := renderSlot(snapshotIn(root), 0, testAvail, testMetrics, names, labels); got.Text != "aaa-proj" {
		t.Fatalf("chip text = %q, want aaa-proj", got.Text)
	}

	writeAbbrev(t, cfgPath, root, "bbb") // same length, so size is unchanged too
	if err := os.Chtimes(cfgPath, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}

	if got := renderSlot(snapshotIn(root), 0, testAvail, testMetrics, names, labels); got.Text != "aaa-proj" {
		t.Errorf("chip text = %q, want the cached aaa-proj — the config was re-read despite an unchanged stamp", got.Text)
	}
}

// A config file that does not exist yet is the common case (the user has never
// renamed a project). Its absence caches as the defaults, and the first rename
// still has to land.
func TestNameConfigShouldPickUpAConfigFileCreatedAfterFirstRender(t *testing.T) {
	cfgPath, root := newNameConfigFixture(t)

	names := &nameConfig{}
	labels := &sblabel.NameCache{}
	if got := renderSlot(snapshotIn(root), 0, testAvail, testMetrics, names, labels); got.Text != "proj" {
		t.Fatalf("chip text = %q, want the unprefixed proj (no user config)", got.Text)
	}

	writeAbbrev(t, cfgPath, root, "zzz")
	if got := renderSlot(snapshotIn(root), 0, testAvail, testMetrics, names, labels); got.Text != "zzz-proj" {
		t.Errorf("chip text = %q, want zzz-proj — a newly created config was not picked up", got.Text)
	}
}

// --- session-name cache ----------------------------------------------------

// writeSessionName drops ~/.claude/sessions/<pid>.json carrying name under the
// test's HOME, which is what `/name` writes and what label.RawName prefers over
// the window title. Returns the path so a test can rewrite it.
func writeSessionName(t *testing.T, pid int, name string) string {
	t.Helper()
	dir := filepath.Join(os.Getenv("HOME"), ".claude", "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, fmt.Sprintf("%d.json", pid))
	body := fmt.Sprintf(`{"pid":%d,"name":%q,"status":"busy"}`, pid, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// `/name bbb` rewrites the session file out from under a running chip, and the
// bar is expected to show the new name on the next snapshot. Caching the lookup
// must not cost that.
func TestRenderSlotShouldShowARenamedSessionOnTheNextSnapshot(t *testing.T) {
	_, root := newNameConfigFixture(t)
	path := writeSessionName(t, 4821, "aaa")

	names := &nameConfig{}
	labels := &sblabel.NameCache{}
	if got := renderSlot(snapshotIn(root), 0, testAvail, testMetrics, names, labels); got.Text != "proj-aaa" {
		t.Fatalf("chip text = %q, want proj-aaa", got.Text)
	}

	// The rename, with a forced later mtime so the test does not depend on the
	// filesystem's timestamp granularity. The new name is the same length as the
	// old, so the size is unchanged and mtime alone carries the invalidation.
	writeSessionName(t, 4821, "bbb")
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	got := renderSlot(snapshotIn(root), 0, testAvail, testMetrics, names, labels)
	if got.Text != "proj-bbb" {
		t.Errorf("chip text = %q, want proj-bbb — the rename did not reach the chip", got.Text)
	}
	if !strings.Contains(got.Tooltip, "bbb") {
		t.Errorf("tooltip kept the stale name: %q", got.Tooltip)
	}
}

// A session that exits and a new one that starts must not share a name just
// because renderSlot names them through one cache.
func TestRenderSlotShouldNameEachSessionFromItsOwnFile(t *testing.T) {
	_, root := newNameConfigFixture(t)
	writeSessionName(t, 4821, "first")
	writeSessionName(t, 4822, "second")
	snap := state.Snapshot{Sessions: []state.Session{
		{PID: 4821, CWD: root, Claude: &state.ClaudeInfo{Status: "working"}},
		{PID: 4822, CWD: root, Claude: &state.ClaudeInfo{Status: "working"}},
	}}

	names := &nameConfig{}
	labels := &sblabel.NameCache{}
	for pass := range 2 {
		if got := renderSlot(snap, 0, testAvail, testMetrics, names, labels); got.Text != "proj-first" {
			t.Errorf("pass %d: slot 0 text = %q, want proj-first", pass, got.Text)
		}
		if got := renderSlot(snap, 1, testAvail, testMetrics, names, labels); got.Text != "proj-second" {
			t.Errorf("pass %d: slot 1 text = %q, want proj-second", pass, got.Text)
		}
	}
}

// BenchmarkRenderSlotUncachedNames is the pre-change cost of one emission: every
// slot names EVERY session in the snapshot so the abbreviation agrees across
// chips, and each of those names was a read plus an unmarshal. Passing a nil
// cache is exactly the old behavior. BenchmarkRenderSlotCachedNames is what
// replaces it. Multiply either by the bar's ten slot processes for the real
// per-snapshot cost.
func BenchmarkRenderSlotUncachedNames(b *testing.B) {
	snap := benchSnapshot(b)
	names := &nameConfig{}
	names.config()
	b.ResetTimer()
	for b.Loop() {
		_ = renderSlot(snap, 0, testAvail, testMetrics, names, nil)
	}
}

func BenchmarkRenderSlotCachedNames(b *testing.B) {
	snap := benchSnapshot(b)
	names := &nameConfig{}
	names.config()
	labels := &sblabel.NameCache{}
	renderSlot(snap, 0, testAvail, testMetrics, names, labels) // prime
	b.ResetTimer()
	for b.Loop() {
		_ = renderSlot(snap, 0, testAvail, testMetrics, names, labels)
	}
}

// benchSnapshot is a snapshot at this machine's usual live session count, each
// session named by a file on disk the way a real one is.
func benchSnapshot(b *testing.B) state.Snapshot {
	b.Helper()
	home := b.TempDir()
	b.Setenv("HOME", home)
	b.Setenv("XDG_CONFIG_HOME", b.TempDir())
	dir := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		b.Fatal(err)
	}
	sessions := make([]state.Session, 13)
	for i := range sessions {
		pid := 7000 + i
		body := fmt.Sprintf(`{"pid":%d,"sessionId":"b5c7fd65-5733-4ce2-a0fa-932b91d2c02%d","cwd":"/home/u/Projects/Arachne","startedAt":1785950796170,"kind":"interactive","name":"assess-npm-vulnerabilities","nameSource":"derived","status":"busy","updatedAt":1785957054072}`, pid, i%10)
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.json", pid)), []byte(body), 0o644); err != nil {
			b.Fatal(err)
		}
		sessions[i] = state.Session{PID: pid, CWD: "/home/u/Projects/Arachne", Claude: &state.ClaudeInfo{Status: "working"}}
	}
	return state.Snapshot{Sessions: sessions}
}

// --- emission dedupe -------------------------------------------------------

func TestEmitterShouldSuppressALineIdenticalToThePrevious(t *testing.T) {
	var buf bytes.Buffer
	e := &emitter{w: &buf}
	o := waybarOutput{Text: "sb-foo", Tooltip: "tip", Class: []string{"working"}}

	if !e.emit(o) {
		t.Fatal("the first emission must always be written")
	}
	if e.emit(o) {
		t.Error("an identical emission should be suppressed")
	}
	if got := strings.Count(buf.String(), "\n"); got != 1 {
		t.Errorf("wrote %d lines, want 1", got)
	}
}

func TestEmitterShouldWriteWhenAnyFieldChanges(t *testing.T) {
	var buf bytes.Buffer
	e := &emitter{w: &buf}
	e.emit(waybarOutput{Text: "sb-foo", Tooltip: "idle · 3m", Class: []string{"idle"}})

	// The tooltip's live duration counter alone is a real change: the bar shows
	// it, so it must reach the pipe.
	if !e.emit(waybarOutput{Text: "sb-foo", Tooltip: "idle · 4m", Class: []string{"idle"}}) {
		t.Error("a changed tooltip duration should be written")
	}
	if !e.emit(waybarOutput{Text: "sb-foo", Tooltip: "idle · 4m", Class: []string{"working"}}) {
		t.Error("a changed class should be written")
	}
	if got := strings.Count(buf.String(), "\n"); got != 3 {
		t.Errorf("wrote %d lines, want 3", got)
	}
}

// Entering the degraded state changes the bytes, so the dedupe lets it through
// on its own; staying down must not re-print it every retry.
func TestEmitterShouldWriteTheDegradedChipOnceWhenTheDaemonGoesDown(t *testing.T) {
	var buf bytes.Buffer
	e := &emitter{w: &buf}
	degraded := waybarOutput{Text: "✕", Tooltip: "switchboard not running", Class: []string{"tracker-down"}}

	e.emit(waybarOutput{Text: "sb-foo", Class: []string{"working"}})
	if !e.emit(degraded) {
		t.Error("the transition into degraded must be written")
	}
	if e.emit(degraded) || e.emit(degraded) {
		t.Error("a daemon that stays down should not re-print the degraded chip every retry")
	}
	if got := strings.Count(buf.String(), "\n"); got != 2 {
		t.Errorf("wrote %d lines, want 2", got)
	}
}

// After a reconnect our record of what the bar last read is untrustworthy —
// waybar may have reloaded the module. forget makes the next line unconditional
// so a chip cannot stick showing stale content across a daemon restart.
func TestEmitterShouldWriteAfterForgetEvenWhenTheLineRepeats(t *testing.T) {
	var buf bytes.Buffer
	e := &emitter{w: &buf}
	chip := waybarOutput{Text: "sb-foo", Class: []string{"working"}}

	e.emit(chip)
	if e.emit(chip) {
		t.Fatal("precondition: the repeat should be suppressed before forget")
	}
	e.forget()
	if !e.emit(chip) {
		t.Error("the first line after a reconnect must be written even if it repeats")
	}
	if got := strings.Count(buf.String(), "\n"); got != 2 {
		t.Errorf("wrote %d lines, want 2", got)
	}
}

// Aggregate mode shares the emitter, so it dedupes too.
func TestEmitterShouldSuppressRepeatsInAggregateMode(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var buf bytes.Buffer
	e := &emitter{w: &buf}
	names := &nameConfig{}
	labels := &sblabel.NameCache{}
	snap := state.Snapshot{Sessions: []state.Session{
		{PID: 4821, CWD: "/home/u/proj", Claude: &state.ClaudeInfo{Status: "working"}},
	}}

	if !e.emit(renderAggregate(snap, names, labels)) {
		t.Fatal("the first aggregate emission must be written")
	}
	if e.emit(renderAggregate(snap, names, labels)) {
		t.Error("an unchanged aggregate snapshot should be suppressed")
	}
}

// BenchmarkNameConfigUncached is the pre-change cost: one os.ReadFile plus a
// json.Unmarshal of projects.json per emission, paid by every slot process on
// every snapshot. BenchmarkNameConfigCached is what replaces it — one os.Stat.
func BenchmarkNameConfigUncached(b *testing.B) {
	cfgPath, root := benchFixture(b)
	writeAbbrevB(b, cfgPath, root)
	for b.Loop() {
		_ = projectname.Load()
	}
}

func BenchmarkNameConfigCached(b *testing.B) {
	cfgPath, root := benchFixture(b)
	writeAbbrevB(b, cfgPath, root)
	names := &nameConfig{}
	names.config() // prime
	for b.Loop() {
		_ = names.config()
	}
}

func benchFixture(b *testing.B) (string, string) {
	b.Helper()
	cfgHome := b.TempDir()
	b.Setenv("HOME", b.TempDir())
	b.Setenv("XDG_CONFIG_HOME", cfgHome)
	root := filepath.Join(b.TempDir(), "proj")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		b.Fatal(err)
	}
	cfgPath := filepath.Join(cfgHome, "switchboard", "projects.json")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		b.Fatal(err)
	}
	return cfgPath, root
}

// A realistic config: the user has renamed a handful of projects.
func writeAbbrevB(b *testing.B, cfgPath, root string) {
	b.Helper()
	entries := ""
	for i := range 8 {
		if i > 0 {
			entries += ","
		}
		entries += fmt.Sprintf(`%q:{"canonical":"p%d","aliases":["p%d"]}`, fmt.Sprintf("%s%d", root, i), i, i)
	}
	body := fmt.Sprintf(`{"projects":{%s}}`, entries)
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		b.Fatal(err)
	}
}

// --- width override --------------------------------------------------------

func TestResolveAvailPxShouldUseTheOverrideWhenTheBarSuppliesOne(t *testing.T) {
	if got := resolveAvailPx(1234.5); got != 1234.5 {
		t.Errorf("resolveAvailPx(1234.5) = %v, want 1234.5 (no hyprctl fork)", got)
	}
}

// Zero and negative both mean "auto-detect", so an unset flag keeps the bar
// working with no config change.
func TestResolveAvailPxShouldAutoDetectWhenTheOverrideIsUnset(t *testing.T) {
	want := barlayout.ScreenWidthPx()
	for _, in := range []float64{0, -1} {
		if got := resolveAvailPx(in); got != want {
			t.Errorf("resolveAvailPx(%v) = %v, want the auto-detected %v", in, got, want)
		}
	}
}

// The end-to-end rename path, exercised through the REAL writer rather than a
// hand-rolled os.WriteFile: ~/.config/scripts/claude-abbrev-edit shells out to
// `switchboard-ctl name set`, which is projectname.SetAbbrev -> upsertEntry ->
// temp file + os.Rename.
//
// Deliberately no os.Chtimes fudging here. SetAbbrev writes MarshalIndent output,
// so swapping one three-letter abbrev for another leaves the file size
// IDENTICAL — mtime is the only thing that can catch the change, and this test
// fails on any filesystem whose timestamp granularity is too coarse to separate
// the two writes. That is exactly the property worth knowing about.
func TestNameConfigShouldPickUpARenameWrittenBySetAbbrev(t *testing.T) {
	_, root := newNameConfigFixture(t)
	if err := projectname.SetAbbrev(root, "aaa"); err != nil {
		t.Fatal(err)
	}

	names := &nameConfig{}
	labels := &sblabel.NameCache{}
	if got := renderSlot(snapshotIn(root), 0, testAvail, testMetrics, names, labels); got.Text != "aaa-proj" {
		t.Fatalf("chip text = %q, want aaa-proj", got.Text)
	}

	// The middle-click rename, as the bar performs it.
	if err := projectname.SetAbbrev(root, "zzz"); err != nil {
		t.Fatal(err)
	}
	got := renderSlot(snapshotIn(root), 0, testAvail, testMetrics, names, labels)
	if got.Text != "zzz-proj" {
		t.Errorf("chip text = %q, want zzz-proj — the middle-click rename did not reach the chip", got.Text)
	}
	if !strings.Contains(got.Tooltip, "<span variant='smallcaps'>proj</span>") ||
		strings.Contains(got.Tooltip, "zzz") {
		t.Errorf("hover should keep the full display name, not the abbreviation: %q", got.Tooltip)
	}
}
