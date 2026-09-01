package main

import (
	"context"
	"encoding/json"
	"net"
	"sort"
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

func TestGenericCodexPermissionRequestUsesAmbiguousApprovalGrace(t *testing.T) {
	coordinator, store := newStandardCodexHookTestCoordinator(t, "thread-1")
	coordinator.codexApprovalGrace = 30 * time.Millisecond
	base := time.Now()
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "UserPromptSubmit", SessionID: "thread-1", TurnID: "turn-1", ObservedAt: base,
	})
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "PermissionRequest", SessionID: "thread-1", TurnID: "turn-1", ToolUseID: "call-1",
		ToolName: "exec_command", ObservedAt: base.Add(time.Millisecond),
	})
	if graph := codexGraph(t, store); graph.Summary.Status != state.StatusWorking || graph.Summary.Attention != agentgraph.AttentionNone {
		t.Fatalf("ambiguous hook approval became red before grace: %#v", graph.Summary)
	}

	waitForCodexGraph(t, store, "thread-1", state.StatusPermission)
	graph := codexGraph(t, store)
	if graph.Summary.Attention != agentgraph.AttentionApproval {
		t.Fatalf("unresolved hook approval did not use timeout fallback: %#v", graph.Summary)
	}

	sendCodexHook(coordinator, store, rpc.Request{
		Event: "PreToolUse", SessionID: "thread-1", TurnID: "turn-1", ToolUseID: "call-1",
		ToolName: "exec_command", ObservedAt: time.Now(),
	})
	if graph = codexGraph(t, store); graph.Summary.Status != state.StatusWorking || graph.Summary.Attention != agentgraph.AttentionNone {
		t.Fatalf("matching tool progress did not clear timeout fallback: %#v", graph.Summary)
	}
}

func TestGenericCodexPermissionResolutionInsideGraceNeverPublishesRed(t *testing.T) {
	coordinator, store := newStandardCodexHookTestCoordinator(t, "thread-1")
	coordinator.codexApprovalGrace = 40 * time.Millisecond
	base := time.Now()
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "PermissionRequest", SessionID: "thread-1", TurnID: "turn-1", ToolUseID: "call-1",
		ToolName: "exec_command", ObservedAt: base,
	})
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "PreToolUse", SessionID: "thread-1", TurnID: "turn-1", ToolUseID: "call-1",
		ToolName: "exec_command", ObservedAt: base.Add(time.Millisecond),
	})
	time.Sleep(2 * coordinator.codexApprovalGrace)
	if graph := codexGraph(t, store); graph.Summary.Status != state.StatusWorking || graph.Summary.Attention != agentgraph.AttentionNone {
		t.Fatalf("resolved hook approval published a late red edge: %#v", graph.Summary)
	}
}

func TestNewerAppServerNonAttentionCancelsHookTimeoutFallback(t *testing.T) {
	coordinator, store := newStandardCodexHookTestCoordinator(t, "thread-1")
	coordinator.codexApprovalGrace = 40 * time.Millisecond
	base := time.Now()
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "PermissionRequest", SessionID: "thread-1", TurnID: "turn-1", ToolUseID: "call-1",
		ToolName: "exec_command", ObservedAt: base,
	})
	ref, _ := providerRootRef(store.Snapshot().Sessions[0])
	observation := testCodexObservation(ref, "thread-1", base.Add(time.Millisecond), agentgraph.RuntimeActive, agentgraph.AttentionNone)
	if !coordinator.applyObservation(ref, coordinator.begin(ref.Key()), observation, claudeprovider.Compatibility{}, base.Add(time.Millisecond)) {
		t.Fatal("newer app-server resolution observation was not applied")
	}

	time.Sleep(2 * coordinator.codexApprovalGrace)
	if graph := codexGraph(t, store); graph.Summary.Status != state.StatusWorking || graph.Summary.Attention != agentgraph.AttentionNone {
		t.Fatalf("settled app-server state allowed a late hook red edge: %#v", graph.Summary)
	}
}

func TestCodexHumanInputPermissionRemainsImmediate(t *testing.T) {
	coordinator, store := newStandardCodexHookTestCoordinator(t, "thread-1")
	coordinator.codexApprovalGrace = time.Second
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "PermissionRequest", SessionID: "thread-1", TurnID: "turn-1", ToolUseID: "question-1",
		ToolName: "AskUserQuestion", ObservedAt: time.Now(),
	})
	graph := codexGraph(t, store)
	if graph.Summary.Status != state.StatusPermission || graph.Summary.Attention != agentgraph.AttentionUserInput {
		t.Fatalf("structured human input was delayed by approval grace: %#v", graph.Summary)
	}
}

func TestAmbiguousHookApprovalCannotClearExistingHumanInput(t *testing.T) {
	coordinator, store := newStandardCodexHookTestCoordinator(t, "thread-1")
	coordinator.codexApprovalGrace = 50 * time.Millisecond
	base := time.Now()
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "PreToolUse", SessionID: "thread-1", TurnID: "turn-1", ToolUseID: "question-1",
		ToolName: "request_user_input", ObservedAt: base,
	})
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "PermissionRequest", SessionID: "thread-1", TurnID: "turn-1", ToolUseID: "approval-1",
		ToolName: "exec_command", ObservedAt: base.Add(time.Millisecond),
	})
	graph := codexGraph(t, store)
	if graph.Summary.Status != state.StatusPermission || graph.Summary.Attention != agentgraph.AttentionUserInput {
		t.Fatalf("ambiguous approval cleared existing human input: %#v", graph.Summary)
	}
	time.Sleep(2 * coordinator.codexApprovalGrace)
	if graph = codexGraph(t, store); graph.Summary.Attention != agentgraph.AttentionUserInput {
		t.Fatalf("approval timeout replaced existing human-input reason: %#v", graph.Summary)
	}

	sendCodexHook(coordinator, store, rpc.Request{
		Event: "PreToolUse", SessionID: "thread-1", TurnID: "turn-1", ToolUseID: "approval-1",
		ToolName: "exec_command", ObservedAt: base.Add(2 * time.Millisecond),
	})
	if graph = codexGraph(t, store); graph.Summary.Attention != agentgraph.AttentionUserInput {
		t.Fatalf("approval resolution cleared unrelated human input: %#v", graph.Summary)
	}
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

// A Codex AskUserQuestion raises the red through isCodexHumanInputPermission,
// which is strictly wider than the isCodexUserInputTool predicate that opens a
// pending record. Without an owner the question is re-asserted by nobody, so the
// next hook edge mapping to active — from any writer — republishes
// attention=none over a person who is still being asked a question. That is a
// missed RED: silent, and it costs the user however long they stay away.
func TestCodexQuestionShouldSurviveUnrelatedWriterProgressWhenOpenedByPermissionRequest(t *testing.T) {
	coordinator, store := newStandardCodexHookTestCoordinator(t, "thread-1")
	base := time.Now()
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "PermissionRequest", SessionID: "thread-1", TurnID: "turn-1", ToolUseID: "question-1",
		ToolName: "AskUserQuestion", ObservedAt: base,
	})
	if graph := codexGraph(t, store); graph.Summary.Status != state.StatusPermission ||
		graph.Summary.Attention != agentgraph.AttentionUserInput {
		t.Fatalf("AskUserQuestion permission did not become waiting-for-user: %#v", graph.Summary)
	}

	sendCodexHook(coordinator, store, rpc.Request{
		Event: "PostToolUse", SessionID: "thread-1", TurnID: "turn-1", AgentID: "sibling-writer",
		ToolUseID: "exec-1", ToolName: "exec_command", ObservedAt: base.Add(time.Millisecond),
	})
	if graph := codexGraph(t, store); graph.Summary.Status != state.StatusPermission ||
		graph.Summary.Attention != agentgraph.AttentionUserInput {
		t.Fatalf("an unrelated writer's progress erased the question red: %#v", graph.Summary)
	}
}

func codexPendingWriters(t *testing.T, coordinator *agentCoordinator, store *state.Store) []string {
	t.Helper()
	ref, ok := providerRootRef(store.Snapshot().Sessions[0])
	if !ok {
		t.Fatal("session has no provider root ref")
	}
	coordinator.codexHookMu.Lock()
	defer coordinator.codexHookMu.Unlock()
	rootState := coordinator.codexHookRoots[ref.Key()]
	if rootState == nil {
		return nil
	}
	writers := make([]string, 0, len(rootState.pending))
	for _, pending := range rootState.pending {
		writers = append(writers, pending.writer)
	}
	sort.Strings(writers)
	return writers
}

func codexApprovalWriters(t *testing.T, coordinator *agentCoordinator, store *state.Store) []string {
	t.Helper()
	ref, ok := providerRootRef(store.Snapshot().Sessions[0])
	if !ok {
		t.Fatal("session has no provider root ref")
	}
	coordinator.codexHookMu.Lock()
	defer coordinator.codexHookMu.Unlock()
	rootState := coordinator.codexHookRoots[ref.Key()]
	if rootState == nil {
		return nil
	}
	writers := make([]string, 0, len(rootState.approvals))
	for _, pending := range rootState.approvals {
		writers = append(writers, pending.writer)
	}
	sort.Strings(writers)
	return writers
}

func TestCodexQuestionShouldClearWhenItsOwnPostToolUseIDMatches(t *testing.T) {
	coordinator, store := newStandardCodexHookTestCoordinator(t, "thread-1")
	base := time.Now()
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "PermissionRequest", SessionID: "thread-1", TurnID: "turn-1", ToolUseID: "question-1",
		ToolName: "AskUserQuestion", ObservedAt: base,
	})
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "PostToolUse", SessionID: "thread-1", TurnID: "turn-1", ToolUseID: "question-other",
		ToolName: "AskUserQuestion", ObservedAt: base.Add(time.Millisecond),
	})
	if graph := codexGraph(t, store); graph.Summary.Status != state.StatusPermission {
		t.Fatalf("another question's answer cleared this one: %#v", graph.Summary)
	}

	sendCodexHook(coordinator, store, rpc.Request{
		Event: "PostToolUse", SessionID: "thread-1", TurnID: "turn-1", ToolUseID: "question-1",
		ToolName: "AskUserQuestion", ObservedAt: base.Add(2 * time.Millisecond),
	})
	if graph := codexGraph(t, store); graph.Summary.Status != state.StatusWorking ||
		graph.Summary.Attention != agentgraph.AttentionNone {
		t.Fatalf("the answered question did not resume working: %#v", graph.Summary)
	}
	if writers := codexPendingWriters(t, coordinator, store); len(writers) != 0 {
		t.Fatalf("answered question stayed owned: %v", writers)
	}
}

// The red is bounded rather than latched: without this the question would hold
// idle for the full 24h codexHookAttentionFreshness window when the person
// answers in the TUI in a way that emits no PostToolUse, or interrupts.
//
// The unrelated-progress step in the middle is load-bearing. Without it the test
// passes on a source where AskUserQuestion opens no pending at all — the shape
// this phase exists to fix — because the observation edges alone happen to land
// on the same colours.
func TestCodexQuestionShouldClearOnTheTurnStopWhenNoPostToolUseArrives(t *testing.T) {
	coordinator, store := newStandardCodexHookTestCoordinator(t, "thread-1")
	base := time.Now()
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "PermissionRequest", SessionID: "thread-1", TurnID: "turn-1", AgentID: "writer-a",
		ToolUseID: "question-1", ToolName: "AskUserQuestion", ObservedAt: base,
	})
	if graph := codexGraph(t, store); graph.Summary.Attention != agentgraph.AttentionUserInput {
		t.Fatalf("AskUserQuestion permission did not become waiting-for-user: %#v", graph.Summary)
	}

	sendCodexHook(coordinator, store, rpc.Request{
		Event: "PostToolUse", SessionID: "thread-1", TurnID: "turn-1", AgentID: "writer-a",
		ToolUseID: "exec-1", ToolName: "exec_command", ObservedAt: base.Add(time.Millisecond),
	})
	if graph := codexGraph(t, store); graph.Summary.Attention != agentgraph.AttentionUserInput {
		t.Fatalf("unrelated progress erased the question before the Stop: %#v", graph.Summary)
	}

	sendCodexHook(coordinator, store, rpc.Request{
		Event: "Stop", SessionID: "thread-1", TurnID: "turn-1", AgentID: "writer-a",
		ObservedAt: base.Add(2 * time.Millisecond),
	})
	if graph := codexGraph(t, store); graph.Summary.Status != state.StatusIdle ||
		graph.Summary.Attention != agentgraph.AttentionNone {
		t.Fatalf("the turn Stop did not release the question: %#v", graph.Summary)
	}
	if writers := codexPendingWriters(t, coordinator, store); len(writers) != 0 {
		t.Fatalf("the turn Stop left the question owned: %v", writers)
	}
}

// Every Stop that reaches the reducer is the root turn boundary: HandleHook
// diverts SubagentStart/SubagentStop to enqueueCodexChildHook, and a child has no
// other stop edge. So a pending opened by a hook that happened to carry an
// agent_id must still be released by it. Scoping the sweep to the pending's own
// writer strands the record instead — no release edge exists at all, and
// overlayCodexPendingObservation re-asserts the red on every subsequent snapshot
// until conversation rotation or the 24h freshness expiry.
func TestCodexQuestionShouldClearOnTheRootStopWhenTheOpeningHookNamedAWriter(t *testing.T) {
	coordinator, store := newStandardCodexHookTestCoordinator(t, "thread-1")
	base := time.Now()
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "PermissionRequest", SessionID: "thread-1", TurnID: "turn-1", AgentID: "child-1",
		ToolUseID: "question-1", ToolName: "AskUserQuestion", ObservedAt: base,
	})
	if graph := codexGraph(t, store); graph.Summary.Attention != agentgraph.AttentionUserInput {
		t.Fatalf("AskUserQuestion permission did not become waiting-for-user: %#v", graph.Summary)
	}

	sendCodexHook(coordinator, store, rpc.Request{
		Event: "Stop", SessionID: "thread-1", ObservedAt: base.Add(time.Millisecond),
	})
	if graph := codexGraph(t, store); graph.Summary.Status != state.StatusIdle ||
		graph.Summary.Attention != agentgraph.AttentionNone {
		t.Fatalf("the root Stop did not release a child-attributed question: %#v", graph.Summary)
	}
	if writers := codexPendingWriters(t, coordinator, store); len(writers) != 0 {
		t.Fatalf("the root Stop left a child-attributed question owned: %v", writers)
	}
}

// A denied or interrupted gate emits no PreToolUse and no PostToolUse, so the
// conversation-level sweeps are its only cancelling edge. Scoping them to the
// gate's own writer leaves it armed, and the grace timer then publishes an
// approval red over work the user has already resumed — the oscillation shape.
func TestCodexApprovalShouldResolveOnTheNextTurnWhenTheGateNamedAWriter(t *testing.T) {
	coordinator, store := newStandardCodexHookTestCoordinator(t, "thread-1")
	coordinator.codexApprovalGrace = 40 * time.Millisecond
	base := time.Now()
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "PermissionRequest", SessionID: "thread-1", TurnID: "turn-1", AgentID: "child-1",
		ToolUseID: "gate-1", ToolName: "exec_command", ObservedAt: base,
	})
	if want := []string{"child-1"}; !equalStrings(codexApprovalWriters(t, coordinator, store), want) {
		t.Fatalf("permission request was not owned: %v, want %v", codexApprovalWriters(t, coordinator, store), want)
	}

	sendCodexHook(coordinator, store, rpc.Request{
		Event: "UserPromptSubmit", SessionID: "thread-1", TurnID: "turn-2", ObservedAt: base.Add(time.Millisecond),
	})
	if got := codexApprovalWriters(t, coordinator, store); len(got) != 0 {
		t.Fatalf("a new user prompt left the abandoned gate armed: %v", got)
	}

	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		if graph := codexGraph(t, store); graph.Summary.Attention != agentgraph.AttentionNone {
			t.Fatalf("the abandoned gate published a red over resumed work: %#v", graph.Summary)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// One question announcing itself on both onset edges must leave one record. When
// the edges disagree about carrying a tool_use_id their keys differ, and the
// id-less twin has no exact close edge: the answer clears the id-keyed record and
// the chip stays red for the whole remainder of the turn while the agent works.
func TestCodexQuestionShouldOpenOneRecordWhenBothOnsetEdgesDescribeTheSameCall(t *testing.T) {
	coordinator, store := newStandardCodexHookTestCoordinator(t, "thread-1")
	base := time.Now()
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "PreToolUse", SessionID: "thread-1", TurnID: "turn-1",
		ToolName: "AskUserQuestion", ToolInputHash: "ask-hash", ObservedAt: base,
	})
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "PermissionRequest", SessionID: "thread-1", TurnID: "turn-1", ToolUseID: "call-1",
		ToolName: "AskUserQuestion", ToolInputHash: "ask-hash", ObservedAt: base.Add(time.Millisecond),
	})
	if writers := codexPendingWriters(t, coordinator, store); len(writers) != 1 {
		t.Fatalf("one question opened %d records: %v", len(writers), writers)
	}

	// The answer's PostToolUse input is a strict superset of the question's, so
	// its hash differs; only the id can close this record.
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "PostToolUse", SessionID: "thread-1", TurnID: "turn-1", ToolUseID: "call-1",
		ToolName: "AskUserQuestion", ToolInputHash: "answer-hash", ObservedAt: base.Add(2 * time.Millisecond),
	})
	if graph := codexGraph(t, store); graph.Summary.Attention != agentgraph.AttentionNone {
		t.Fatalf("the answered question stayed red: %#v", graph.Summary)
	}
	if writers := codexPendingWriters(t, coordinator, store); len(writers) != 0 {
		t.Fatalf("the answered question stayed owned: %v", writers)
	}
}

// A generic gate that comes up while a question red is already published cannot
// be distinguished on the chip, but discarding it is a missed RED: answering the
// question would then paint green over an approval modal nobody has decided.
func TestCodexApprovalShouldOutliveAQuestionRedRatherThanBeDiscarded(t *testing.T) {
	coordinator, store := newStandardCodexHookTestCoordinator(t, "thread-1")
	coordinator.codexApprovalGrace = 20 * time.Millisecond
	base := time.Now()
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "PermissionRequest", SessionID: "thread-1", TurnID: "turn-1", ToolUseID: "question-1",
		ToolName: "AskUserQuestion", ObservedAt: base,
	})
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "PermissionRequest", SessionID: "thread-1", TurnID: "turn-1", ToolUseID: "gate-1",
		ToolName: "exec_command", ObservedAt: base.Add(time.Millisecond),
	})

	// Past the grace the gate is deferred, not forgotten, because the question red
	// is still up.
	deadline := time.Now().Add(time.Second)
	for len(codexApprovalWriters(t, coordinator, store)) > 0 && time.Now().Before(deadline) {
		if graph := codexGraph(t, store); graph.Summary.Attention != agentgraph.AttentionUserInput {
			t.Fatalf("the question red was disturbed while the gate waited: %#v", graph.Summary)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := codexApprovalWriters(t, coordinator, store); len(got) != 1 {
		t.Fatalf("the gate was discarded under the question red: %v", got)
	}

	sendCodexHook(coordinator, store, rpc.Request{
		Event: "PostToolUse", SessionID: "thread-1", TurnID: "turn-1", ToolUseID: "question-1",
		ToolName: "AskUserQuestion", ObservedAt: base.Add(2 * time.Millisecond),
	})
	graph := waitForCodexAttention(t, store, agentgraph.AttentionApproval)
	if graph.Summary.Status != state.StatusPermission {
		t.Fatalf("the surviving gate did not reach red once the question cleared: %#v", graph.Summary)
	}
}

// The published approval red is the one human-owned colour nothing re-asserts.
// state.approvals keeps the record, but handleCodexHookNow maps the next Stop or
// PostToolUse from ANY writer to active/none and overlayCodexHookObservation
// writes that straight over the red, while the gate itself is still undecided.
// That is the missed RED of §2.3, one provider and one attention reason across:
// silent, and it lasts until the user next looks at the modal.
func TestCodexApprovalRedShouldSurviveUnrelatedProgressWhenTheGateIsUndecided(t *testing.T) {
	coordinator, store := newStandardCodexHookTestCoordinator(t, "thread-1")
	coordinator.codexApprovalGrace = 20 * time.Millisecond
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "PermissionRequest", SessionID: "thread-1", TurnID: "turn-1", ToolUseID: "gate-1",
		ToolName: "exec_command", ObservedAt: time.Now(),
	})
	waitForCodexAttention(t, store, agentgraph.AttentionApproval)

	// A sibling's completed call says nothing about this gate. The timestamps must
	// be real wall clock and later than the timer's publication, or shouldApplyObservation
	// fences the edge on staleness and the test passes without proving anything.
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "PostToolUse", SessionID: "thread-1", TurnID: "turn-1", AgentID: "sibling-writer",
		ToolUseID: "exec-1", ToolName: "exec_command", ObservedAt: time.Now(),
	})
	if graph := codexGraph(t, store); graph.Summary.Status != state.StatusPermission ||
		graph.Summary.Attention != agentgraph.AttentionApproval {
		t.Fatalf("an unrelated writer's progress erased the published approval red: %#v", graph.Summary)
	}

	sendCodexHook(coordinator, store, rpc.Request{
		Event: "PreToolUse", SessionID: "thread-1", TurnID: "turn-1", ToolUseID: "gate-1",
		ToolName: "exec_command", ObservedAt: time.Now(),
	})
	if graph := codexGraph(t, store); graph.Summary.Status != state.StatusWorking ||
		graph.Summary.Attention != agentgraph.AttentionNone {
		t.Fatalf("the decided gate did not resume working: %#v", graph.Summary)
	}
	if got := codexApprovalWriters(t, coordinator, store); len(got) != 0 {
		t.Fatalf("the decided gate stayed armed: %v", got)
	}
}

// Re-assertion buys a latch, and a latch that outlives its release edges is a red
// nobody can clear. Every edge that resolved an approval record before this change
// must still resolve one whose red is already up.
func TestCodexPublishedApprovalRedShouldStillReleaseWhenItsResolvingEdgeArrives(t *testing.T) {
	release := []struct {
		name       string
		req        rpc.Request
		wantStatus string
	}{
		{
			name: "should release when its own call completes",
			req: rpc.Request{
				Event: "PostToolUse", SessionID: "thread-1", TurnID: "turn-1", ToolUseID: "gate-1",
				ToolName: "exec_command",
			},
			wantStatus: state.StatusWorking,
		},
		{
			name:       "should release when the root turn ends",
			req:        rpc.Request{Event: "Stop", SessionID: "thread-1", TurnID: "turn-1"},
			wantStatus: state.StatusIdle,
		},
		{
			name:       "should release when the turn boundary names no turn",
			req:        rpc.Request{Event: "Stop", SessionID: "thread-1"},
			wantStatus: state.StatusIdle,
		},
		{
			name:       "should release when the user starts another turn",
			req:        rpc.Request{Event: "UserPromptSubmit", SessionID: "thread-1", TurnID: "turn-2"},
			wantStatus: state.StatusWorking,
		},
	}
	for _, testCase := range release {
		t.Run(testCase.name, func(t *testing.T) {
			coordinator, store := newStandardCodexHookTestCoordinator(t, "thread-1")
			coordinator.codexApprovalGrace = 20 * time.Millisecond
			sendCodexHook(coordinator, store, rpc.Request{
				Event: "PermissionRequest", SessionID: "thread-1", TurnID: "turn-1", ToolUseID: "gate-1",
				ToolName: "exec_command", ObservedAt: time.Now(),
			})
			waitForCodexAttention(t, store, agentgraph.AttentionApproval)

			testCase.req.ObservedAt = time.Now()
			sendCodexHook(coordinator, store, testCase.req)
			if graph := codexGraph(t, store); graph.Summary.Status != testCase.wantStatus ||
				graph.Summary.Attention != agentgraph.AttentionNone {
				t.Fatalf("%s left the approval red latched: %#v", testCase.req.Event, graph.Summary)
			}
			if got := codexApprovalWriters(t, coordinator, store); len(got) != 0 {
				t.Fatalf("%s left the gate armed: %v", testCase.req.Event, got)
			}
		})
	}
}

// A question is the more specific human reason and it already has its own owner.
// Re-asserting approval must never repaint over it, or answering the question
// would show the wrong reason while the gate still holds — and the gate's own red
// must come back once the question clears, which is what D3's deferral bought.
func TestCodexPublishedApprovalRedShouldYieldToAQuestionRedWhileOneIsOpen(t *testing.T) {
	coordinator, store := newStandardCodexHookTestCoordinator(t, "thread-1")
	coordinator.codexApprovalGrace = 20 * time.Millisecond
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "PermissionRequest", SessionID: "thread-1", TurnID: "turn-1", ToolUseID: "gate-1",
		ToolName: "exec_command", ObservedAt: time.Now(),
	})
	waitForCodexAttention(t, store, agentgraph.AttentionApproval)

	sendCodexHook(coordinator, store, rpc.Request{
		Event: "PermissionRequest", SessionID: "thread-1", TurnID: "turn-1", ToolUseID: "question-1",
		ToolName: "AskUserQuestion", ObservedAt: time.Now(),
	})
	if graph := codexGraph(t, store); graph.Summary.Attention != agentgraph.AttentionUserInput {
		t.Fatalf("the published approval red masked a live question: %#v", graph.Summary)
	}

	sendCodexHook(coordinator, store, rpc.Request{
		Event: "PostToolUse", SessionID: "thread-1", TurnID: "turn-1", ToolUseID: "question-1",
		ToolName: "AskUserQuestion", ObservedAt: time.Now(),
	})
	if graph := codexGraph(t, store); graph.Summary.Status != state.StatusPermission ||
		graph.Summary.Attention != agentgraph.AttentionApproval {
		t.Fatalf("answering the question painted green over the undecided gate: %#v", graph.Summary)
	}
}

func waitForCodexAttention(t *testing.T, store *state.Store, attention agentgraph.AttentionState) *state.AgentGraph {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if graph := store.Snapshot().Sessions[0].AgentGraph; graph != nil && graph.Summary.Attention == attention {
			return graph
		}
		time.Sleep(time.Millisecond)
	}
	graph := store.Snapshot().Sessions[0].AgentGraph
	t.Fatalf("Codex graph never reached attention=%q: %#v", attention, graph)
	return nil
}

func TestCodexPendingMatchersShouldFallBackToTheCompositeWhenOnlyOneSideCarriesAnID(t *testing.T) {
	cases := []struct {
		name    string
		pending codexPendingInput
		req     rpc.Request
		want    bool
	}{
		{
			name:    "should match on the id alone when both sides carry one",
			pending: codexPendingInput{toolUseID: "call-1", writer: "a", turnID: "t1", toolName: "AskUserQuestion"},
			req:     rpc.Request{ToolUseID: "call-1", AgentID: "b", TurnID: "t2", ToolName: "other"},
			want:    true,
		},
		{
			name:    "should reject on the id alone when both sides carry different ones",
			pending: codexPendingInput{toolUseID: "call-1", writer: "a", turnID: "t1", toolName: "AskUserQuestion", inputHash: "h"},
			req:     rpc.Request{ToolUseID: "call-2", AgentID: "a", TurnID: "t1", ToolName: "AskUserQuestion", ToolInputHash: "h"},
			want:    false,
		},
		{
			name:    "should match on the composite when only the request carries an id",
			pending: codexPendingInput{writer: "a", turnID: "t1", toolName: "AskUserQuestion", inputHash: "h"},
			req:     rpc.Request{ToolUseID: "call-1", AgentID: "a", TurnID: "t1", ToolName: "AskUserQuestion", ToolInputHash: "h"},
			want:    true,
		},
		{
			// The composite is the fallback for a pending that never got an id, not a
			// second way to close one that has it. Matching here would let an id-less
			// edge from a sibling call of the same shape release a gate the user has
			// not decided — the missed-RED direction.
			name:    "should reject when the pending names a call and the request does not",
			pending: codexPendingInput{toolUseID: "call-1", writer: "a", turnID: "t1", toolName: "AskUserQuestion", inputHash: "h"},
			req:     rpc.Request{AgentID: "a", TurnID: "t1", ToolName: "AskUserQuestion", ToolInputHash: "h"},
			want:    false,
		},
		{
			name:    "should reject the composite when the writer differs",
			pending: codexPendingInput{writer: "a", turnID: "t1", toolName: "AskUserQuestion", inputHash: "h"},
			req:     rpc.Request{ToolUseID: "call-1", AgentID: "b", TurnID: "t1", ToolName: "AskUserQuestion", ToolInputHash: "h"},
			want:    false,
		},
		{
			name:    "should reject the composite when neither side carries a hash",
			pending: codexPendingInput{writer: "a", turnID: "t1", toolName: "AskUserQuestion"},
			req:     rpc.Request{ToolUseID: "call-1", AgentID: "a", TurnID: "t1", ToolName: "AskUserQuestion"},
			want:    false,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := codexPendingInputMatches(testCase.pending, testCase.req); got != testCase.want {
				t.Fatalf("codexPendingInputMatches = %t, want %t", got, testCase.want)
			}
			approval := &codexPendingApproval{
				turnID: testCase.pending.turnID, toolUseID: testCase.pending.toolUseID,
				writer: testCase.pending.writer, toolName: testCase.pending.toolName,
				inputHash: testCase.pending.inputHash,
			}
			if got := codexPendingApprovalMatches(approval, testCase.req); got != testCase.want {
				t.Fatalf("codexPendingApprovalMatches = %t, want %t", got, testCase.want)
			}
		})
	}
}

// request_user_input already had an owner before this phase. Its onset, its
// exact close and its sibling-proofing must be unchanged, or the fix for
// AskUserQuestion has been paid for out of the path that already worked.
func TestCodexRequestUserInputLifecycleShouldBeUnchangedWhenOpenedByPreToolUse(t *testing.T) {
	coordinator, store := newStandardCodexHookTestCoordinator(t, "thread-1")
	base := time.Now()
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "PreToolUse", SessionID: "thread-1", TurnID: "turn-1", ToolUseID: "question-1",
		ToolName: "request_user_input", ObservedAt: base,
	})
	steps := []struct {
		req        rpc.Request
		wantStatus string
	}{
		{rpc.Request{
			Event: "PreToolUse", SessionID: "thread-1", TurnID: "turn-1", ToolUseID: "exec-1",
			ToolName: "exec_command",
		}, state.StatusPermission},
		{rpc.Request{
			Event: "PostToolUse", SessionID: "thread-1", TurnID: "turn-1", ToolUseID: "exec-1",
			ToolName: "exec_command",
		}, state.StatusPermission},
		{rpc.Request{
			Event: "PostToolUse", SessionID: "thread-1", TurnID: "turn-1", ToolUseID: "question-other",
			ToolName: "request_user_input",
		}, state.StatusPermission},
		{rpc.Request{
			Event: "PostToolUse", SessionID: "thread-1", TurnID: "turn-1", ToolUseID: "question-1",
			ToolName: "functions.request_user_input",
		}, state.StatusWorking},
	}
	for i, step := range steps {
		step.req.ObservedAt = base.Add(time.Duration(i+1) * time.Millisecond)
		sendCodexHook(coordinator, store, step.req)
		if got := codexGraph(t, store).Summary.Status; got != step.wantStatus {
			t.Fatalf("step %d (%s %s) status = %q, want %q", i, step.req.Event, step.req.ToolName, got, step.wantStatus)
		}
	}
}
