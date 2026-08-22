package main

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/provider"
	claudeprovider "github.com/tjmisko/switchboard/internal/provider/claude"
	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/terminal"
	"github.com/tjmisko/switchboard/internal/wm"
)

func newStandardCodexHookTestCoordinator(t *testing.T, sessionID string) (*agentCoordinator, *state.Store) {
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

func TestStandardCodexHookRPCOwnsRequestUserInputLifecycle(t *testing.T) {
	coordinator, store := newStandardCodexHookTestCoordinator(t, "thread-1")
	server := rpc.New(store, "", terminal.NewNone(), wm.NewNone())
	server.SetAgentHookHandler(coordinator.HandleHook)
	serverSide, clientSide := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer clientSide.Close()
	go server.ServeConnection(ctx, serverSide)
	encoder, decoder := json.NewEncoder(clientSide), json.NewDecoder(clientSide)
	base := time.Now()

	requests := []struct {
		req        rpc.Request
		wantStatus string
	}{
		{rpc.Request{
			Cmd: "hook", PID: 8123, Agent: state.AgentKindCodex, Event: "PreToolUse", SessionID: "thread-1",
			TurnID: "turn-1", ToolUseID: "question-1", ToolName: "request_user_input", ObservedAt: base,
		}, state.StatusPermission},
		{rpc.Request{
			Cmd: "hook", PID: 8123, Agent: state.AgentKindCodex, Event: "PostToolUse", SessionID: "thread-1",
			TurnID: "turn-1", ToolUseID: "exec-1", ToolName: "exec_command", ObservedAt: base.Add(time.Millisecond),
		}, state.StatusPermission},
		{rpc.Request{
			Cmd: "hook", PID: 8123, Agent: state.AgentKindCodex, Event: "PostToolUse", SessionID: "thread-1",
			TurnID: "turn-1", ToolUseID: "question-1", ToolName: "request_user_input", ObservedAt: base.Add(2 * time.Millisecond),
		}, state.StatusWorking},
	}
	for _, step := range requests {
		if err := encoder.Encode(step.req); err != nil {
			t.Fatal(err)
		}
		var response rpc.Response
		if err := decoder.Decode(&response); err != nil {
			t.Fatal(err)
		}
		if !response.OK {
			t.Fatalf("standard Codex hook response = %+v", response)
		}
		if got := codexGraph(t, store).Summary.Status; got != step.wantStatus {
			t.Fatalf("standard Codex hook status = %q, want %q", got, step.wantStatus)
		}
	}
}

func TestStandardCodexQuestionsAreIsolatedByProcessLifetimeNotCWD(t *testing.T) {
	store := state.New("")
	started := time.Now().Add(-time.Hour)
	seedCoordinatorSession(store, 8201, started, state.AgentKindCodex, "thread-1", "/same")
	seedCoordinatorSession(store, 8202, started.Add(time.Second), state.AgentKindCodex, "thread-2", "/same")
	first := provider.RootRef{PID: 8201, StartedAt: started, Provider: agentgraph.ProviderCodex}
	second := provider.RootRef{PID: 8202, StartedAt: started.Add(time.Second), Provider: agentgraph.ProviderCodex}
	coordinator := newAgentCoordinator(store, nil, nil, nil)
	coordinator.refreshTrackedRoots()
	defer coordinator.Close()
	base := time.Now()
	firstSession, _ := sessionForKey(store.Snapshot(), first.Key())
	coordinator.HandleHook(rpc.Request{
		Agent: state.AgentKindCodex, Event: "PreToolUse", SessionID: "thread-1",
		TurnID: "turn-1", ToolUseID: "question-1", ToolName: "request_user_input", ObservedAt: base,
	}, firstSession)
	secondSession, _ := sessionForKey(store.Snapshot(), second.Key())
	coordinator.HandleHook(rpc.Request{
		Agent: state.AgentKindCodex, Event: "UserPromptSubmit", SessionID: "thread-2", ObservedAt: base.Add(time.Millisecond),
	}, secondSession)

	firstSession, _ = sessionForKey(store.Snapshot(), first.Key())
	secondSession, _ = sessionForKey(store.Snapshot(), second.Key())
	if firstSession.AgentGraph.Summary.Status != state.StatusPermission || secondSession.AgentGraph.Summary.Status != state.StatusWorking {
		t.Fatalf("same-cwd Codex waits crossed process lifetimes: first=%#v second=%#v",
			firstSession.AgentGraph.Summary, secondSession.AgentGraph.Summary)
	}
}

func TestCodexRequestUserInputStaysPendingUntilExactPostToolUse(t *testing.T) {
	coordinator, store := newStandardCodexHookTestCoordinator(t, "thread-1")
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

func TestCodexRequestUserInputSurvivesGenericAppServerSnapshot(t *testing.T) {
	coordinator, store := newStandardCodexHookTestCoordinator(t, "thread-1")
	base := time.Now()
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "PreToolUse", SessionID: "thread-1", TurnID: "turn-1", ToolUseID: "question-1",
		ToolName: "request_user_input", ObservedAt: base,
	})
	ref, _ := providerRootRef(store.Snapshot().Sessions[0])
	observation := testCodexObservation(ref, "thread-1", base.Add(time.Millisecond), agentgraph.RuntimeActive, agentgraph.AttentionNone)
	if !coordinator.applyObservation(ref, coordinator.begin(ref.Key()), observation, claudeprovider.Compatibility{}, base.Add(time.Millisecond)) {
		t.Fatal("generic app-server observation was not applied")
	}
	if graph := codexGraph(t, store); graph.Summary.Status != state.StatusPermission || graph.Summary.Attention != agentgraph.AttentionUserInput {
		t.Fatalf("generic app-server snapshot cleared the standard hook wait: %#v", graph.Summary)
	}
}

func TestCodexRequestUserInputOnsetOutranksConcurrentGenericSnapshot(t *testing.T) {
	coordinator, store := newStandardCodexHookTestCoordinator(t, "thread-1")
	base := time.Now()
	ref, _ := providerRootRef(store.Snapshot().Sessions[0])
	observation := testCodexObservation(ref, "thread-1", base.Add(time.Millisecond), agentgraph.RuntimeActive, agentgraph.AttentionNone)
	if !coordinator.applyObservation(ref, coordinator.begin(ref.Key()), observation, claudeprovider.Compatibility{}, base.Add(time.Millisecond)) {
		t.Fatal("generic app-server observation was not applied")
	}
	// The hook occurred first but reached the daemon after the concurrent poll.
	// Poll time cannot clear a wait the standard app-server path cannot model.
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "PreToolUse", SessionID: "thread-1", TurnID: "turn-1", ToolUseID: "question-1",
		ToolName: "request_user_input", ObservedAt: base,
	})
	if graph := codexGraph(t, store); graph.Summary.Status != state.StatusPermission || graph.Summary.Attention != agentgraph.AttentionUserInput {
		t.Fatalf("concurrent generic snapshot fenced the exact question onset: %#v", graph.Summary)
	}
}

func TestCodexRequestUserInputStopClearsInterruptedPrompt(t *testing.T) {
	coordinator, store := newStandardCodexHookTestCoordinator(t, "thread-1")
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
	coordinator, store := newStandardCodexHookTestCoordinator(t, "thread-1")
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
	coordinator, store := newStandardCodexHookTestCoordinator(t, "thread-old")
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
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "Stop", SessionID: "thread-old", TurnID: "plan-turn", ObservedAt: base.Add(time.Second),
	})
	if graph = codexGraph(t, store); graph.RootID != "thread-new" || graph.Summary.Status != state.StatusWorking {
		t.Fatalf("retired-thread Stop rolled clear backwards: root=%q summary=%#v", graph.RootID, graph.Summary)
	}
}

func TestCodexStandaloneClearSettlesToIdle(t *testing.T) {
	coordinator, store := newStandardCodexHookTestCoordinator(t, "thread-old")
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
	coordinator, store := newStandardCodexHookTestCoordinator(t, "thread-1")
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "SessionStart", SessionID: "thread-1", HookSource: "compact", ObservedAt: time.Now(),
	})
	if graph := codexGraph(t, store); graph.Summary.Status != state.StatusWorking {
		t.Fatalf("compact SessionStart created an idle edge: %#v", graph.Summary)
	}
}
