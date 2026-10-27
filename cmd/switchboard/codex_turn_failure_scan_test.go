package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/state"
)

// rolloutNetworkFailure is the terminal row Codex writes when a turn fails
// offline (codex-cli 0.163.0).
func rolloutNetworkFailure(at time.Time) string {
	return fmt.Sprintf("{\"timestamp\":%q,\"type\":\"event_msg\",\"payload\":{\"type\":\"task_complete\",\"last_agent_message\":null,\"error\":{\"message\":\"workspace routing discovery failed\",\"codex_error_info\":\"other\"}}}\n",
		at.Format(time.RFC3339Nano))
}

func sessionTurnFailure(store *state.Store) *state.TurnFailure {
	return store.Snapshot().Sessions[0].TurnFailure
}

func TestCodexTurnFailureScanShouldRecordWhenTheRootRolloutEndedOnANetworkError(t *testing.T) {
	c, store := newStandardCodexHookTestCoordinator(t, "root")
	now := time.Now()
	rootPath := writeRollout(t, t.TempDir(), "root.jsonl", rolloutActive(now), rolloutNetworkFailure(now.Add(4*time.Second)))
	sendCodexHook(c, store, rpc.Request{Event: "PostToolUse", SessionID: "root", ToolName: "spawn_agent", Transcript: rootPath, ObservedAt: now})

	c.observeAt(context.Background(), codexRootRef(t, store), now.Add(95*time.Second))

	got := sessionTurnFailure(store)
	if got == nil || got.Kind != "unreachable" || got.Source != state.TurnFailureSourceCodexRollout {
		t.Fatalf("turn failure = %+v, want unreachable from the rollout", got)
	}
}

func TestCodexTurnFailureScanShouldIgnoreASubagentFailureWhenTheRootHasNone(t *testing.T) {
	c, store := newStandardCodexHookTestCoordinator(t, "root")
	now := time.Now()
	dir := t.TempDir()
	rootPath := writeRollout(t, dir, "root.jsonl", rolloutActive(now))
	childPath := writeRollout(t, dir, "child.jsonl", rolloutActive(now.Add(time.Second)), rolloutNetworkFailure(now.Add(2*time.Second)))
	sendCodexHook(c, store, rpc.Request{Event: "PostToolUse", SessionID: "root", ToolName: "spawn_agent", Transcript: rootPath, ObservedAt: now})
	sendCodexHook(c, store, rpc.Request{Event: "PostToolUse", SessionID: "root", AgentID: "child", ToolName: "exec_command", Transcript: childPath, ObservedAt: now.Add(time.Second)})

	c.observeAt(context.Background(), codexRootRef(t, store), now.Add(95*time.Second))

	if got := sessionTurnFailure(store); got != nil {
		t.Fatalf("turn failure = %+v, want none for a subagent-only failure", got)
	}
}

func TestCodexTurnFailureScanShouldRefuseAFailureOlderThanTheNewestActivity(t *testing.T) {
	c, store := newStandardCodexHookTestCoordinator(t, "root")
	now := time.Now()
	rootPath := writeRollout(t, t.TempDir(), "root.jsonl", rolloutNetworkFailure(now.Add(-time.Minute)))
	// The user re-prompted after the failure: the hook is newer than the row.
	sendCodexHook(c, store, rpc.Request{Event: "PostToolUse", SessionID: "root", ToolName: "spawn_agent", Transcript: rootPath, ObservedAt: now})

	c.observeAt(context.Background(), codexRootRef(t, store), now.Add(95*time.Second))

	if got := sessionTurnFailure(store); got != nil {
		t.Fatalf("turn failure = %+v, want the stale failure refused", got)
	}
}
