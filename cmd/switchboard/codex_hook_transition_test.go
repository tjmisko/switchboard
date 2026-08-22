package main

import (
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/state"
)

func newCodexHookTestCoordinator(t *testing.T, sessionID string) (*agentCoordinator, *state.Store) {
	t.Helper()
	store := state.New("")
	seedCoordinatorSession(store, 8123, time.Now().Add(-time.Hour), state.AgentKindCodex, sessionID, "/codex")
	coordinator := newAgentCoordinator(store, nil, nil, nil)
	coordinator.codexStartSettle = 10 * time.Millisecond
	coordinator.refreshTrackedRoots()
	t.Cleanup(coordinator.Close)
	return coordinator, store
}

func sendCodexHook(coordinator *agentCoordinator, store *state.Store, req rpc.Request) {
	req.Agent = state.AgentKindCodex
	coordinator.HandleHook(req, store.Snapshot().Sessions[0])
}

func codexGraph(t *testing.T, store *state.Store) *state.AgentGraph {
	t.Helper()
	graph := store.Snapshot().Sessions[0].AgentGraph
	if graph == nil {
		t.Fatal("Codex graph is nil")
	}
	return graph
}

func waitForCodexGraph(t *testing.T, store *state.Store, rootID, status string) *state.AgentGraph {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		graph := store.Snapshot().Sessions[0].AgentGraph
		if graph != nil && graph.RootID == rootID && graph.Summary.Status == status {
			return graph
		}
		time.Sleep(time.Millisecond)
	}
	graph := store.Snapshot().Sessions[0].AgentGraph
	t.Fatalf("Codex graph did not become root=%q status=%q: %#v", rootID, status, graph)
	return nil
}

func TestCodexRequestUserInputStaysPendingUntilExactPostToolUse(t *testing.T) {
	coordinator, store := newCodexHookTestCoordinator(t, "thread-1")
	base := time.Now()
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "UserPromptSubmit", SessionID: "thread-1", TurnID: "turn-1", ObservedAt: base,
	})
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "PreToolUse", SessionID: "thread-1", TurnID: "turn-1", ToolUseID: "question-1",
		ToolName: "request_user_input", ObservedAt: base.Add(time.Millisecond),
	})
	graph := codexGraph(t, store)
	if graph.Summary.Status != state.StatusPermission || graph.Summary.Attention != agentgraph.AttentionUserInput {
		t.Fatalf("request_user_input did not become waiting-for-user: %#v", graph.Summary)
	}

	// A completion from another call (including another request_user_input call)
	// must not make the chip green while question-1 is still on screen.
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "PostToolUse", SessionID: "thread-1", TurnID: "turn-1", ToolUseID: "question-other",
		ToolName: "request_user_input", ObservedAt: base.Add(2 * time.Millisecond),
	})
	if graph = codexGraph(t, store); graph.Summary.Status != state.StatusPermission {
		t.Fatalf("unrelated PostToolUse cleared the question: %#v", graph.Summary)
	}
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "PostToolUse", SessionID: "thread-1", TurnID: "turn-1", ToolUseID: "exec-1",
		ToolName: "exec_command", ObservedAt: base.Add(3 * time.Millisecond),
	})
	if graph = codexGraph(t, store); graph.Summary.Status != state.StatusPermission {
		t.Fatalf("unrelated tool completion cleared the question: %#v", graph.Summary)
	}

	sendCodexHook(coordinator, store, rpc.Request{
		Event: "PostToolUse", SessionID: "thread-1", TurnID: "turn-1", ToolUseID: "question-1",
		ToolName: "request_user_input", ObservedAt: base.Add(4 * time.Millisecond),
	})
	if graph = codexGraph(t, store); graph.Summary.Status != state.StatusWorking || graph.Summary.Attention != agentgraph.AttentionNone {
		t.Fatalf("exact question response did not resume working: %#v", graph.Summary)
	}
}

func TestCodexRequestUserInputStopClearsInterruptedPrompt(t *testing.T) {
	coordinator, store := newCodexHookTestCoordinator(t, "thread-1")
	base := time.Now()
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "PreToolUse", SessionID: "thread-1", TurnID: "turn-1", ToolUseID: "question-1",
		ToolName: "functions.request_user_input", ObservedAt: base,
	})
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "Stop", SessionID: "thread-1", TurnID: "turn-1", ObservedAt: base.Add(time.Millisecond),
	})
	if graph := codexGraph(t, store); graph.Summary.Status != state.StatusIdle || graph.Summary.Attention != agentgraph.AttentionNone {
		t.Fatalf("Stop did not release the interrupted question: %#v", graph.Summary)
	}
}

func TestCodexAcceptedPlanImmediatelyResumesWorking(t *testing.T) {
	coordinator, store := newCodexHookTestCoordinator(t, "thread-1")
	base := time.Now()
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "Stop", SessionID: "thread-1", TurnID: "plan-turn", PermissionMode: "plan", ObservedAt: base,
	})
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "UserPromptSubmit", SessionID: "thread-1", TurnID: "implementation-turn", PermissionMode: "default", ObservedAt: base.Add(time.Millisecond),
	})
	if graph := codexGraph(t, store); graph.Summary.Status != state.StatusWorking {
		t.Fatalf("accepted plan did not resume working: %#v", graph.Summary)
	}
}

func TestCodexClearAndImmediatePlanAcceptanceCoalesceWithoutIdle(t *testing.T) {
	coordinator, store := newCodexHookTestCoordinator(t, "thread-old")
	base := time.Now()
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "UserPromptSubmit", SessionID: "thread-old", TurnID: "plan-turn", ObservedAt: base,
	})
	updates, cancelUpdates := store.Subscribe()
	defer cancelUpdates()
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "SessionStart", SessionID: "thread-new", HookSource: "clear", ObservedAt: base.Add(time.Millisecond),
	})
	graph := codexGraph(t, store)
	if graph.RootID != "thread-old" || graph.Summary.Status != state.StatusWorking {
		t.Fatalf("provisional clear emitted a visible transition: root=%q summary=%#v", graph.RootID, graph.Summary)
	}

	sendCodexHook(coordinator, store, rpc.Request{
		Event: "UserPromptSubmit", SessionID: "thread-new", TurnID: "implementation-turn", ObservedAt: base.Add(2 * time.Millisecond),
	})
	graph = codexGraph(t, store)
	if graph.RootID != "thread-new" || graph.Summary.Status != state.StatusWorking {
		t.Fatalf("clear-context plan acceptance did not rotate directly to working: root=%q summary=%#v", graph.RootID, graph.Summary)
	}
	for {
		select {
		case update := <-updates:
			if got := update.Snapshot.Sessions[0].Enrichment().Status; got != state.StatusWorking {
				t.Fatalf("clear-context plan acceptance published transient status %q", got)
			}
		default:
			goto updatesDrained
		}
	}

updatesDrained:
	time.Sleep(3 * coordinator.codexStartSettle)
	if graph = codexGraph(t, store); graph.RootID != "thread-new" || graph.Summary.Status != state.StatusWorking {
		t.Fatalf("canceled SessionStart timer repainted the accepted plan: root=%q summary=%#v", graph.RootID, graph.Summary)
	}

	// A late Stop from the pre-clear thread is newer in wall-clock time but stale
	// in conversation identity and must not rotate the PID backwards.
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "Stop", SessionID: "thread-old", TurnID: "plan-turn", ObservedAt: base.Add(time.Second),
	})
	if graph = codexGraph(t, store); graph.RootID != "thread-new" || graph.Summary.Status != state.StatusWorking {
		t.Fatalf("retired-thread Stop rolled clear backwards: root=%q summary=%#v", graph.RootID, graph.Summary)
	}
}

func TestCodexStandaloneClearSettlesToIdle(t *testing.T) {
	coordinator, store := newCodexHookTestCoordinator(t, "thread-old")
	base := time.Now()
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "UserPromptSubmit", SessionID: "thread-old", ObservedAt: base,
	})
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "SessionStart", SessionID: "thread-new", HookSource: "clear", ObservedAt: base.Add(time.Millisecond),
	})
	waitForCodexGraph(t, store, "thread-new", state.StatusIdle)
}

func TestCodexCompactSessionStartRemainsWorking(t *testing.T) {
	coordinator, store := newCodexHookTestCoordinator(t, "thread-1")
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "SessionStart", SessionID: "thread-1", HookSource: "compact", ObservedAt: time.Now(),
	})
	if graph := codexGraph(t, store); graph.Summary.Status != state.StatusWorking {
		t.Fatalf("compact SessionStart created an idle edge: %#v", graph.Summary)
	}
}
