package main

import (
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	claudeprovider "github.com/tjmisko/switchboard/internal/provider/claude"
	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/state"
)

func TestCodexAsyncQuestionStaysRedUntilNextUserSubmission(t *testing.T) {
	coordinator, store := newStandardCodexHookTestCoordinator(t, "thread-1")
	store.Apply(func(sessions map[int]*state.Session) {
		sessions[8123].SetHerdr(state.HerdrReading{
			PaneID: "w1:p1", Socket: "/herdr.sock", Agent: "codex", Status: state.HerdrWorking, Live: true,
		}, time.Now())
	})
	base := time.Now().Add(-time.Second)
	for i, step := range []struct {
		event, tool string
		runtime     agentgraph.RuntimeState
	}{
		{"PreToolUse", "functions.request_user_input_async", agentgraph.RuntimeActive},
		{"PostToolUse", "functions.request_user_input_async", agentgraph.RuntimeActive},
		{"PostToolUse", "exec_command", agentgraph.RuntimeActive},
		{"Stop", "", agentgraph.RuntimeIdle},
	} {
		sendCodexHook(coordinator, store, rpc.Request{
			Event: step.event, SessionID: "thread-1", TurnID: "turn-1", ToolUseID: "async-question",
			ToolName: step.tool, ObservedAt: base.Add(time.Duration(i) * time.Millisecond),
		})
		graph := codexGraph(t, store)
		if graph.Summary.Attention != agentgraph.AttentionUserInput || graph.Summary.Runtime != step.runtime {
			t.Fatalf("%s %s: summary=%#v", step.event, step.tool, graph.Summary)
		}
		if session := store.Snapshot().Sessions[0]; session.Codex.Status != state.StatusPermission {
			t.Fatalf("herdr working hid the async question: status=%s", session.Codex.Status)
		}
	}
	ref, _ := providerRootRef(store.Snapshot().Sessions[0])
	at := base.Add(4 * time.Millisecond)
	observation := testCodexObservation(ref, "thread-1", at, agentgraph.RuntimeActive, agentgraph.AttentionNone)
	if !coordinator.applyObservation(ref, coordinator.begin(ref.Key()), observation, claudeprovider.Compatibility{}, at) {
		t.Fatal("snapshot did not apply")
	}
	if graph := codexGraph(t, store); graph.Summary.Attention != agentgraph.AttentionUserInput || graph.Summary.Runtime != agentgraph.RuntimeActive {
		t.Fatalf("snapshot lost async attention or stopped runtime: %#v", graph.Summary)
	}
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "UserPromptSubmit", SessionID: "thread-1", TurnID: "turn-2", ObservedAt: base.Add(5 * time.Millisecond),
	})
	if graph := codexGraph(t, store); graph.Summary.Attention != agentgraph.AttentionNone || graph.Summary.Runtime != agentgraph.RuntimeActive {
		t.Fatalf("next submission did not dismiss async question: %#v", graph.Summary)
	}
}

func TestCodexAsyncDismissalLeavesBlockingQuestionOwned(t *testing.T) {
	coordinator, store := newStandardCodexHookTestCoordinator(t, "thread-1")
	base := time.Now()
	for i, tool := range []string{"request_user_input_async", "request_user_input"} {
		sendCodexHook(coordinator, store, rpc.Request{
			Event: "PreToolUse", SessionID: "thread-1", TurnID: "turn-1", ToolUseID: tool,
			ToolName: tool, ObservedAt: base.Add(time.Duration(i) * time.Millisecond),
		})
	}
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "UserPromptSubmit", SessionID: "thread-1", TurnID: "turn-2", ObservedAt: base.Add(2 * time.Millisecond),
	})
	if graph := codexGraph(t, store); graph.Summary.Attention != agentgraph.AttentionUserInput || graph.Summary.Runtime != agentgraph.RuntimeIdle {
		t.Fatalf("async dismissal cleared or unblocked a separate question: %#v", graph.Summary)
	}
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "PostToolUse", SessionID: "thread-1", TurnID: "turn-1", ToolUseID: "request_user_input",
		ToolName: "request_user_input", ObservedAt: base.Add(3 * time.Millisecond),
	})
	if graph := codexGraph(t, store); graph.Summary.Attention != agentgraph.AttentionNone {
		t.Fatalf("exact blocking-question resolution did not clear red: %#v", graph.Summary)
	}
}
