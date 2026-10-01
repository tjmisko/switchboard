package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/state"
)

func TestCodexTranscriptPollingWithoutProvider(t *testing.T) {
	for _, scenario := range []string{"complete", "silent", "new turn", "waiting for input"} {
		t.Run(scenario, func(t *testing.T) {
			c, store := newStandardCodexHookTestCoordinator(t, "thread-1")
			now := time.Now()
			path := filepath.Join(t.TempDir(), "rollout.jsonl")
			text := ""
			if scenario != "silent" {
				text = fmt.Sprintf("{\"timestamp\":%q,\"type\":\"event_msg\",\"payload\":{\"type\":\"task_complete\"}}\n", now.Add(time.Second).Format(time.RFC3339Nano))
			}
			if scenario == "new turn" {
				text += fmt.Sprintf("{\"timestamp\":%q,\"type\":\"event_msg\",\"payload\":{\"type\":\"task_started\"}}\n", now.Add(2*time.Second).Format(time.RFC3339Nano))
			}
			if err := os.WriteFile(path, []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			req := rpc.Request{Event: "PreToolUse", SessionID: "thread-1", ToolName: "exec_command", Transcript: path, ObservedAt: now}
			if scenario == "waiting for input" {
				req.ToolName = "request_user_input"
				req.ToolUseID = "question"
			}
			sendCodexHook(c, store, req)
			ref, ok := providerRootRef(store.Snapshot().Sessions[0])
			if !ok {
				t.Fatal("missing provider root")
			}
			before := codexGraph(t, store).Summary.Status
			c.observeAt(context.Background(), ref, now.Add(89*time.Second))
			if got := codexGraph(t, store).Summary.Status; got != before {
				t.Fatalf("early correction: %s -> %s", before, got)
			}
			c.observeAt(context.Background(), ref, now.Add(95*time.Second))
			want := before
			if scenario == "complete" {
				want = state.StatusIdle
			}
			if got := codexGraph(t, store).Summary.Status; got != want {
				t.Fatalf("status=%s want=%s", got, want)
			}
			if scenario == "complete" {
				sendCodexHook(c, store, rpc.Request{Event: "PreToolUse", SessionID: "thread-1", ToolName: "exec_command", ObservedAt: now.Add(96 * time.Second)})
				if got := codexGraph(t, store).Summary.Status; got != state.StatusWorking {
					t.Fatalf("new hook failed to reopen: %s", got)
				}
			}
		})
	}
}
