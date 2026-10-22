package main

import (
	"context"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/state"
)

func newCodexTurnEndTestCoordinator(t *testing.T) (*agentCoordinator, *state.Store, *fakeCodexCoordinatorObserver) {
	t.Helper()
	store := state.New("")
	ref := seedCoordinatorSession(store, 8123, time.Now().Add(-time.Hour), state.AgentKindCodex, "thread-1", "/codex")
	fake := newFakeCodexCoordinatorObserver()
	fake.observations[ref.Key()] = testCodexObservation(ref, "thread-1", time.Now(), agentgraph.RuntimeIdle, agentgraph.AttentionNone)
	c := newAgentCoordinator(store, nil, nil, fake)
	c.codexStartSettle = 10 * time.Millisecond
	c.refreshTrackedRoots()
	t.Cleanup(c.Close)
	return c, store, fake
}

func TestCodexTurnEndShouldRetireTheTurnsApprovalLatch(t *testing.T) {
	c, store, fake := newCodexTurnEndTestCoordinator(t)
	base := time.Now()
	sendCodexHook(c, store, rpc.Request{
		Event: "PermissionRequest", SessionID: "thread-1", TurnID: "turn-1",
		ToolUseID: "exec-1", ToolName: "exec_command", ObservedAt: base,
	})
	if got := codexApprovalWriters(t, c, store); len(got) != 1 {
		t.Fatalf("approval gate did not open: %v", got)
	}
	fake.turnEnds = map[string]fakeTurnEnd{"thread-1": {turnID: "turn-1", at: base}}
	c.observeAt(context.Background(), codexRootRef(t, store), base.Add(time.Second))
	if got := codexApprovalWriters(t, c, store); len(got) != 0 {
		t.Fatalf("turn-end did not retire the gate: %v", got)
	}
}

func TestCodexTurnEndShouldRetireTheTurnsQuestionLatch(t *testing.T) {
	c, store, fake := newCodexTurnEndTestCoordinator(t)
	base := time.Now()
	sendCodexHook(c, store, rpc.Request{
		Event: "PreToolUse", SessionID: "thread-1", TurnID: "turn-1",
		ToolName: "AskUserQuestion", ToolInputHash: "ask-hash", ObservedAt: base,
	})
	if got := codexPendingWriters(t, c, store); len(got) != 1 {
		t.Fatalf("question did not open: %v", got)
	}
	fake.turnEnds = map[string]fakeTurnEnd{"thread-1": {turnID: "turn-1", at: base}}
	c.observeAt(context.Background(), codexRootRef(t, store), base.Add(time.Second))
	if got := codexPendingWriters(t, c, store); len(got) != 0 {
		t.Fatalf("turn-end did not retire the question: %v", got)
	}
}

// A turn-end releases only the gates that turn opened: a sibling turn's latch
// must survive, or a concurrent request would lose its red.
func TestCodexTurnEndShouldNotRetireAnotherTurnsLatch(t *testing.T) {
	c, store, fake := newCodexTurnEndTestCoordinator(t)
	base := time.Now()
	sendCodexHook(c, store, rpc.Request{
		Event: "PermissionRequest", SessionID: "thread-1", TurnID: "turn-1",
		ToolUseID: "exec-1", ToolName: "exec_command", ObservedAt: base,
	})
	sendCodexHook(c, store, rpc.Request{
		Event: "PermissionRequest", SessionID: "thread-1", TurnID: "turn-2",
		ToolUseID: "exec-2", ToolName: "exec_command", ObservedAt: base.Add(time.Millisecond),
	})
	fake.turnEnds = map[string]fakeTurnEnd{"thread-1": {turnID: "turn-1", at: base}}
	c.observeAt(context.Background(), codexRootRef(t, store), base.Add(time.Second))

	ref, _ := providerRootRef(store.Snapshot().Sessions[0])
	c.codexHookMu.Lock()
	var turns []string
	for _, pending := range c.codexHookRoots[ref.Key()].approvals {
		turns = append(turns, pending.turnID)
	}
	c.codexHookMu.Unlock()
	if len(turns) != 1 || turns[0] != "turn-2" {
		t.Fatalf("surviving approvals = %v, want only turn-2", turns)
	}
}

// A cap proves the root's turn ended, so the latch it opened must be gone, not
// merely hidden behind the limited overlay.
func TestCodexUsageLimitShouldRetireTheRootsApprovalLatch(t *testing.T) {
	c, store := newStandardCodexHookTestCoordinator(t, "root")
	now := time.Now()
	resetsAt := now.Add(3 * time.Hour).Truncate(time.Second)
	dir := t.TempDir()
	rootPath := writeRollout(t, dir, "root.jsonl", rolloutActive(now), rolloutCapped(now.Add(2*time.Second), resetsAt))
	sendCodexHook(c, store, rpc.Request{
		Event: "PermissionRequest", SessionID: "root", TurnID: "turn-1",
		ToolUseID: "exec-1", ToolName: "exec_command", Transcript: rootPath, ObservedAt: now,
	})
	if got := codexApprovalWriters(t, c, store); len(got) != 1 {
		t.Fatalf("approval gate did not open: %v", got)
	}

	c.observeAt(context.Background(), codexRootRef(t, store), now.Add(95*time.Second))
	if got := codexApprovalWriters(t, c, store); len(got) != 0 {
		t.Fatalf("cap did not retire the latch: %v", got)
	}
}

// A closed session's stale red is a restored last-known permission. The cap is
// the level that says the turn ended, so it must retire that red, not just the
// hook latches, or it flashes again the moment the cap lapses.
func TestCodexUsageLimitShouldRetireARestoredPermissionWhenTheRootIsCapped(t *testing.T) {
	now := time.Now()
	resetsAt := now.Add(2 * time.Hour).Truncate(time.Second)
	store := state.New("")
	ref := seedCoordinatorSession(store, 4084, now.Add(-2*time.Hour), state.AgentKindCodex, "root", "/codex")
	restored := &state.AgentGraph{
		RootID: "root", Source: agentgraph.SourceRestoredLastKnown,
		ObservedAt: now.Add(-time.Hour), FreshUntil: now.Add(23 * time.Hour), Complete: true,
		Summary: state.AgentGraphSummary{Runtime: agentgraph.RuntimeIdle, Attention: agentgraph.AttentionApproval, Status: state.StatusPermission},
		Nodes:   []state.AgentNode{{ID: "root", Runtime: agentgraph.RuntimeIdle, Attention: agentgraph.AttentionApproval}},
	}
	store.Apply(func(m map[int]*state.Session) {
		m[4084].LandAgentGraph(restored, state.GraphLanding{Kind: state.GraphRestored}, now)
	})
	fake := newFakeCodexCoordinatorObserver()
	dir := t.TempDir()
	fake.threadPaths = map[string]string{"root": writeRollout(t, dir, "root.jsonl", rolloutActive(now.Add(-time.Minute)), rolloutCapped(now, resetsAt))}
	fake.observations[ref.Key()] = testCodexObservation(ref, "root", now, agentgraph.RuntimeNotLoaded, agentgraph.AttentionNone)
	c := newAgentCoordinator(store, nil, nil, fake)
	c.refreshTrackedRoots()
	defer c.Close()

	c.observeAt(context.Background(), codexRootRef(t, store), now)
	c.observeAt(context.Background(), codexRootRef(t, store), now.Add(codexUsageLimitScanInterval))

	snap := store.Snapshot().Sessions[0]
	if snap.UsageLimit == nil {
		t.Fatal("cap was not recorded")
	}
	if snap.AgentGraph == nil || snap.AgentGraph.Summary.Attention != agentgraph.AttentionNone {
		t.Fatalf("restored permission survived the cap: %#v", snap.AgentGraph)
	}
	published := state.ProjectPublished(store.Snapshot(), resetsAt.Add(time.Minute)).Sessions[0]
	if published.Codex == nil || published.Codex.Status == state.StatusPermission {
		t.Fatalf("post-reset status = %#v, want idle", published.Codex)
	}
}

// A stale turn-end reports the same terminal turn on every tick. It must not
// retire a latch opened after that turn ended.
func TestCodexTurnEndShouldNotRetireALatchOpenedAfterTheTurn(t *testing.T) {
	c, store, fake := newCodexTurnEndTestCoordinator(t)
	base := time.Now()
	fake.turnEnds = map[string]fakeTurnEnd{"thread-1": {turnID: "turn-1", at: base}}
	sendCodexHook(c, store, rpc.Request{
		Event: "PermissionRequest", SessionID: "thread-1", TurnID: "turn-2",
		ToolUseID: "exec-2", ToolName: "exec_command", ObservedAt: base.Add(time.Second),
	})
	c.observeAt(context.Background(), codexRootRef(t, store), base.Add(2*time.Second))
	if got := codexApprovalWriters(t, c, store); len(got) != 1 {
		t.Fatalf("a stale turn-end retired a newer latch: %v", got)
	}
}
