package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/durfmt"
	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/state"
)

var graphRenderNow = time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)

type fakeTUITimer struct {
	c      chan time.Time
	resets chan time.Duration
}

func newFakeTUITimer() *fakeTUITimer {
	return &fakeTUITimer{c: make(chan time.Time, 1), resets: make(chan time.Duration, 8)}
}
func (t *fakeTUITimer) C() <-chan time.Time { return t.c }
func (t *fakeTUITimer) Reset(d time.Duration) {
	t.resets <- d
}
func (t *fakeTUITimer) Stop() {}

type fakeTUIClock struct {
	mu    sync.RWMutex
	now   time.Time
	timer *fakeTUITimer
}

func (c *fakeTUIClock) Now() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.now
}
func (c *fakeTUIClock) NewTimer(d time.Duration) durfmt.Timer {
	c.timer.resets <- d
	return c.timer
}
func (c *fakeTUIClock) advance(now time.Time) {
	c.mu.Lock()
	c.now = now
	c.mu.Unlock()
	c.timer.c <- now
}

type tuiRecvStep struct {
	response *rpc.Response
	err      error
}

type scriptedTUIReceiver struct {
	calls atomic.Int32
	steps chan tuiRecvStep
	enter chan int
}

func (r *scriptedTUIReceiver) Recv(response *rpc.Response) error {
	call := int(r.calls.Add(1))
	r.enter <- call
	step := <-r.steps
	if step.response != nil {
		*response = *step.response
	}
	return step.err
}

func waitTUIRecvCall(t *testing.T, receiver *scriptedTUIReceiver, want int) {
	t.Helper()
	select {
	case got := <-receiver.enter:
		if got != want {
			t.Fatalf("Recv call = %d, want %d", got, want)
		}
	case <-time.After(time.Second):
		t.Fatalf("Recv call %d never started", want)
	}
}

func waitTUIRefreshDelay(t *testing.T, timer *fakeTUITimer) time.Duration {
	t.Helper()
	select {
	case d := <-timer.resets:
		return d
	case <-time.After(time.Second):
		t.Fatal("TUI did not schedule its next refresh")
		return 0
	}
}

func TestQuietTUISnapshotAdvancesAtCompactBoundaryWithoutAnotherSocketRead(t *testing.T) {
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	since := base.Add(-59 * time.Second)
	snapshot := state.Snapshot{Sessions: []state.Session{{
		PID: 7, CWD: "/work", Claude: &state.AgentInfo{Status: state.StatusIdle, StatusSinceWire: &since},
	}}}
	receiver := &scriptedTUIReceiver{steps: make(chan tuiRecvStep), enter: make(chan int, 2)}
	snapshots := make(chan state.Snapshot, 1)
	errs := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go receiveSnapshots(ctx, receiver, snapshots, errs)
	waitTUIRecvCall(t, receiver, 1)
	receiver.steps <- tuiRecvStep{response: &rpc.Response{Snapshot: &snapshot}}

	timer := newFakeTUITimer()
	clock := &fakeTUIClock{now: base, timer: timer}
	frames := make(chan string, 2)
	done := make(chan error, 1)
	go func() {
		done <- runSnapshotRenderer(ctx, snapshots, errs, "/home/u", nil,
			func(frame string) { frames <- frame }, clock)
	}()
	if got := waitTUIRefreshDelay(t, timer); got != time.Second {
		t.Fatalf("first visible boundary delay = %v, want 1s", got)
	}
	waitTUIRecvCall(t, receiver, 2)
	initial := <-frames

	clock.advance(base.Add(time.Second))
	if got := waitTUIRefreshDelay(t, timer); got != time.Minute {
		t.Fatalf("post-boundary refresh delay = %v, want 1m", got)
	}
	refreshed := <-frames
	if got := receiver.calls.Load(); got != 2 {
		t.Fatalf("timer refresh started %d Recv calls, want the same one blocked read", got)
	}
	if !strings.Contains(initial, "59s") || !strings.Contains(refreshed, "1m") {
		t.Fatalf("quiet TUI age did not advance\ninitial:\n%s\nrefreshed:\n%s", initial, refreshed)
	}

	receiver.steps <- tuiRecvStep{err: io.EOF}
	select {
	case err := <-done:
		if err != io.EOF {
			t.Fatalf("renderer ended with %v, want EOF", err)
		}
	case <-time.After(time.Second):
		t.Fatal("TUI renderer did not stop after socket reader ended")
	}
}

func TestRenderSnapshotCodexRootPlusTreeGolden(t *testing.T) {
	since := graphRenderNow.Add(-5 * time.Minute)
	snap := state.Snapshot{
		Capabilities: &state.Capabilities{Observe: true, Navigate: true, WM: "hyprland", Terminal: "wezterm"},
		Sessions: []state.Session{{
			PID: 4821, CWD: "/home/u/Projects/switchboard", Focused: true, Agent: state.AgentKindCodex,
			Hyprland: &state.HyprlandInfo{Workspace: "4"},
			Codex:    &state.AgentInfo{Status: state.StatusPermission, StatusSinceWire: &since},
			AgentGraph: graphFixture("codex-root", []state.AgentNode{
				{ID: "codex-root", Runtime: agentgraph.RuntimeIdle, Lifecycle: agentgraph.LifecycleRunning},
				{ID: "documents-id", ParentID: "codex-root", Nickname: "documents", Attention: agentgraph.AttentionUserInput, Lifecycle: agentgraph.LifecycleRunning, UpdatedAt: graphRenderNow.Add(-22 * time.Minute)},
				{ID: "nested-123456789", ParentID: "documents-id", Role: "tagging", Runtime: agentgraph.RuntimeActive, Lifecycle: agentgraph.LifecycleRunning, UpdatedAt: graphRenderNow.Add(-21 * time.Minute), Usage: state.AgentUsage{TotalTokens: 321}},
				{ID: "keyboard-id", ParentID: "codex-root", Nickname: "keyboard", Runtime: agentgraph.RuntimeNotLoaded, Lifecycle: agentgraph.LifecycleCompleted, CompletedAt: graphRenderNow.Add(-20 * time.Minute)},
				{ID: "metadata-id", ParentID: "codex-root", Nickname: "metadata", Attention: agentgraph.AttentionApproval, Lifecycle: agentgraph.LifecycleRunning, UpdatedAt: graphRenderNow.Add(-3 * time.Minute)},
			}),
		}},
	}
	assertFrameGolden(t, "codex-tree.golden.txt", renderSnapshot(snap, "/home/u", false, graphRenderNow, nil))
}

func TestRenderSnapshotClaudeRootPlusTreeGolden(t *testing.T) {
	snap := state.Snapshot{Sessions: []state.Session{{
		PID: 5102, CWD: "/home/u/proj", Agent: state.AgentKindClaude,
		Claude: &state.AgentInfo{Status: state.StatusDelegating, InFlightSubagents: 2},
		AgentGraph: graphFixture("claude-root", []state.AgentNode{
			{ID: "claude-root", Runtime: agentgraph.RuntimeIdle, Lifecycle: agentgraph.LifecycleRunning},
			{ID: "research-id", ParentID: "claude-root", Nickname: "research", Runtime: agentgraph.RuntimeActive, Lifecycle: agentgraph.LifecycleRunning, UpdatedAt: graphRenderNow.Add(-45 * time.Second)},
			{ID: "fedcba9876543210", ParentID: "claude-root", Runtime: agentgraph.RuntimeUnknown, Lifecycle: agentgraph.LifecycleUnknown},
		}),
	}}}
	assertFrameGolden(t, "claude-tree.golden.txt", renderSnapshot(snap, "/home/u", false, graphRenderNow, nil))
}

func TestRenderSnapshotMarksExpiredAndSuspendedChildrenStale(t *testing.T) {
	graph := graphFixture("root", []state.AgentNode{
		{ID: "root"},
		{ID: "error", ParentID: "root", Nickname: "failure", Runtime: agentgraph.RuntimeSystemError, Lifecycle: agentgraph.LifecycleRunning},
	})
	graph.FreshUntil = graphRenderNow
	snap := state.Snapshot{Sessions: []state.Session{{
		PID: 1, CWD: "/tmp/x", Suspended: true, AgentGraph: graph,
	}}}
	got := renderSnapshot(snap, "/home/u", false, graphRenderNow, nil)
	if !strings.Contains(got, "system error · stale (root suspended)") {
		t.Fatalf("suspended child did not retain an explicitly stale error state:\n%s", got)
	}
}

func TestShouldNotMarkTheTUIStaleWhenTheDaemonSuppressedAWithinBucketRepublish(t *testing.T) {
	base := graphRenderNow
	graph := &state.AgentGraph{
		RootID: "root", ObservedAt: base,
		FreshUntil: state.CeilFreshUntil(base.Add(time.Second)), Complete: true,
		Nodes: []state.AgentNode{
			{ID: "root"},
			{ID: "child", ParentID: "root", Runtime: agentgraph.RuntimeActive,
				Lifecycle: agentgraph.LifecycleRunning, UpdatedAt: base},
		},
	}
	snap := state.Snapshot{Sessions: []state.Session{{PID: 1, CWD: "/tmp/x", AgentGraph: graph}}}

	covered := renderSnapshot(snap, "/home/u", false, base.Add(state.FreshnessBucket-100*time.Millisecond), nil)
	if strings.Contains(covered, " · stale") || !strings.Contains(covered, "4s") {
		t.Fatalf("ceiling-covered consumer frame became stale or lost its age counter:\n%s", covered)
	}

	expired := renderSnapshot(snap, "/home/u", false, base.Add(state.FreshnessBucket+100*time.Millisecond), nil)
	if !strings.Contains(expired, " · stale") {
		t.Fatalf("frame remained fresh after the published ceiling:\n%s", expired)
	}
}

func TestRenderSnapshotBoundsAgentRows(t *testing.T) {
	nodes := []state.AgentNode{{ID: "root"}}
	for i := 0; i < maxAgentRows+3; i++ {
		nodes = append(nodes, state.AgentNode{ID: fmt.Sprintf("child-%02d", i), ParentID: "root", Runtime: agentgraph.RuntimeIdle})
	}
	snap := state.Snapshot{Sessions: []state.Session{{PID: 1, CWD: "/tmp/x", AgentGraph: graphFixture("root", nodes)}}}
	got := renderSnapshot(snap, "/home/u", false, graphRenderNow, nil)
	if !strings.Contains(got, "+3 more agents") {
		t.Fatalf("bounded frame missing fold marker:\n%s", got)
	}
	if strings.Contains(got, "child-32") {
		t.Fatalf("folded child leaked into frame:\n%s", got)
	}
}

func graphFixture(rootID string, nodes []state.AgentNode) *state.AgentGraph {
	return &state.AgentGraph{
		RootID: rootID, Source: agentgraph.SourceCodexAppServer,
		ObservedAt: graphRenderNow.Add(-time.Second), FreshUntil: graphRenderNow.Add(time.Minute), Complete: true,
		Summary: state.AgentGraphSummary{Status: state.StatusDelegating, Runtime: agentgraph.RuntimeIdle}, Nodes: nodes,
	}
}

func assertFrameGolden(t *testing.T, name, got string) {
	t.Helper()
	want, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	normalized := strings.ReplaceAll(got, "\r\n", "\n")
	if normalized != string(want) {
		t.Fatalf("frame differs from %s\n--- got ---\n%s--- want ---\n%s", name, normalized, want)
	}
}

func TestRenderSnapshotShowsStatusDuration(t *testing.T) {
	now := time.Date(2026, 6, 26, 14, 30, 0, 0, time.UTC)
	since := now.Add(-3 * time.Minute)
	snap := state.Snapshot{
		Sessions: []state.Session{
			{PID: 4821, CWD: "/home/u/proj", Claude: &state.ClaudeInfo{
				Status: "idle", StatusSinceWire: &since,
			}},
		},
	}
	got := renderSnapshot(snap, "/home/u", false, now, nil)
	if !strings.Contains(got, "3m") {
		t.Errorf("session line should carry the idle duration '3m':\n%s", got)
	}
}

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

	got := renderSnapshot(snap, "/home/u", false, time.Now(), nil)

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

// A delegating session renders its "delegating" label in green (work is
// happening by proxy), distinct from idle's yellow.
func TestRenderSnapshotDelegatingIsGreen(t *testing.T) {
	snap := state.Snapshot{
		Sessions: []state.Session{
			{PID: 4821, CWD: "/home/u/proj", Claude: &state.ClaudeInfo{
				Status: state.StatusDelegating, InFlightSubagents: 2,
			}},
		},
	}
	plain := renderSnapshot(snap, "/home/u", false, time.Now(), nil)
	if !strings.Contains(plain, "delegating") {
		t.Errorf("plain frame missing 'delegating' label:\n%s", plain)
	}
	colored := renderSnapshot(snap, "/home/u", true, time.Now(), nil)
	if !strings.Contains(colored, colGreen) {
		t.Errorf("delegating session should be painted green:\n%q", colored)
	}
}

func TestRenderSnapshotGreysSuspended(t *testing.T) {
	snap := state.Snapshot{
		Sessions: []state.Session{
			{
				PID: 4821, CWD: "/home/u/proj", Suspended: true,
				Claude: &state.ClaudeInfo{Status: "working"},
			},
		},
	}

	// Plain (no color): suspended sessions read "suspended", not their stale
	// underlying status.
	plain := renderSnapshot(snap, "/home/u", false, time.Now(), nil)
	if !strings.Contains(plain, "suspended") {
		t.Errorf("plain frame missing 'suspended' label:\n%s", plain)
	}
	if strings.Contains(plain, "working") {
		t.Errorf("suspended session should not show its stale 'working' status:\n%s", plain)
	}

	// Colored: the line is painted grey (the suspended treatment).
	colored := renderSnapshot(snap, "/home/u", true, time.Now(), nil)
	if !strings.Contains(colored, colGrey) {
		t.Errorf("colored suspended frame missing grey escape:\n%q", colored)
	}
}

func TestRenderSnapshotEmptyAndNoCaps(t *testing.T) {
	got := renderSnapshot(state.Snapshot{UpdatedAt: time.Now()}, "/home/u", false, time.Now(), nil)
	if !strings.Contains(got, "0 sessions") {
		t.Errorf("want '0 sessions', got:\n%s", got)
	}
	if !strings.Contains(got, "no agent sessions") {
		t.Errorf("want empty-state line, got:\n%s", got)
	}
	// nil capabilities → bare "observe" tier, no panic.
	if !strings.Contains(got, "observe") {
		t.Errorf("want 'observe' tier with nil caps, got:\n%s", got)
	}
}

// --- naming the writer behind a red row -----------------------------------

// blockedSnapshot builds a one-session red snapshot with `writers` blocked (wire
// spelling; "main" = the main thread) and a real subagents/ dir carrying a
// teammate meta per entry of `names` (bare agent id -> teammate name).
func blockedSnapshot(t *testing.T, writers []string, inflight int, names map[string]string) state.Snapshot {
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
	return state.Snapshot{Sessions: []state.Session{{
		PID: 4821, CWD: "/home/u/proj",
		Claude: &state.ClaudeInfo{
			Status:            state.StatusPermission,
			Transcript:        transcriptPath,
			PendingWriters:    writers,
			InFlightSubagents: inflight,
		},
	}}}
}

// The reference renderer is the only surface a headless/SSH user has, and a red
// row that says "permission" and nothing else has exactly the incident's defect:
// it does not say which of the session's writers is stuck.
func TestRenderSnapshotShouldNameTheBlockedTeammateOnAPermissionRow(t *testing.T) {
	snap := blockedSnapshot(t, []string{"af5bd126402ac16c7"}, 4,
		map[string]string{"af5bd126402ac16c7": "escalate-cleanup"})
	got := renderSnapshot(snap, "/home/u", false, time.Now(), nil)
	if !strings.Contains(got, "blocked: escalate-cleanup") {
		t.Errorf("permission row should name the blocked teammate:\n%s", got)
	}
}

func TestRenderSnapshotShouldNameEveryWriterWhenTwoAreBlockedAtOnce(t *testing.T) {
	snap := blockedSnapshot(t, []string{"af5bd126402ac16c7", "main"}, 2,
		map[string]string{"af5bd126402ac16c7": "escalate-cleanup"})
	got := renderSnapshot(snap, "/home/u", false, time.Now(), nil)
	if !strings.Contains(got, "blocked: escalate-cleanup, main") {
		t.Errorf("permission row should name both blocked writers:\n%s", got)
	}
}

// Solo session, main thread blocked: the red row already says this, so the
// annotation stays off rather than spending a word on "main".
func TestRenderSnapshotShouldLeaveASoloPermissionRowUnannotated(t *testing.T) {
	snap := blockedSnapshot(t, []string{"main"}, 0, nil)
	got := renderSnapshot(snap, "/home/u", false, time.Now(), nil)
	if strings.Contains(got, "blocked:") {
		t.Errorf("solo permission row should carry no blocked-writer annotation:\n%s", got)
	}
}

// The annotation is last on the line, after every fixed-width column, so a
// name of any length cannot push the cwd/pid columns out of alignment.
func TestRenderSnapshotShouldKeepColumnsAlignedWhenAWriterIsNamed(t *testing.T) {
	snap := blockedSnapshot(t, []string{"af5bd126402ac16c7"}, 4,
		map[string]string{"af5bd126402ac16c7": "a-very-long-teammate-name-indeed"})
	got := renderSnapshot(snap, "/home/u", false, time.Now(), nil)
	line := ""
	for _, l := range strings.Split(got, "\r\n") {
		if strings.Contains(l, "pid 4821") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("no session row in frame:\n%s", got)
	}
	if strings.Index(line, "blocked:") < strings.Index(line, "pid 4821") {
		t.Errorf("the blocked-writer annotation must follow the fixed-width columns: %q", line)
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
