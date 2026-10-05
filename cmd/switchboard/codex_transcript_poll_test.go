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

// Codex ends a capped turn with task_complete{error: usage_limit_exceeded} and
// fires no Stop hook (functionary session, 2026-10-05); the quiet-window read
// is the only witness, and it must publish limited until the stated reset.
func codexUsageLimitRollout(t *testing.T, now time.Time) (path string, resetsAt time.Time) {
	t.Helper()
	resetsAt = now.Add(5 * time.Hour).Truncate(time.Second)
	text := fmt.Sprintf("{\"timestamp\":%q,\"type\":\"event_msg\",\"payload\":{\"type\":\"token_count\",\"rate_limits\":{\"limit_id\":\"codex\",\"primary\":{\"used_percent\":100,\"window_minutes\":300,\"resets_at\":%d}}}}\n",
		now.Add(500*time.Millisecond).Format(time.RFC3339Nano), resetsAt.Unix())
	text += fmt.Sprintf("{\"timestamp\":%q,\"type\":\"event_msg\",\"payload\":{\"type\":\"task_complete\",\"last_agent_message\":null,\"error\":{\"message\":\"You’ve hit your usage limit.\",\"codex_error_info\":\"usage_limit_exceeded\"}}}\n",
		now.Add(time.Second).Format(time.RFC3339Nano))
	path = filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return path, resetsAt
}

func TestCodexTranscriptPollShouldPublishLimitedWhenTheRolloutEndsOnTheUsageLimit(t *testing.T) {
	c, store := newStandardCodexHookTestCoordinator(t, "thread-1")
	now := time.Now()
	path, resetsAt := codexUsageLimitRollout(t, now)
	sendCodexHook(c, store, rpc.Request{Event: "PostToolUse", SessionID: "thread-1", ToolName: "exec_command", Transcript: path, ObservedAt: now})
	ref, ok := providerRootRef(store.Snapshot().Sessions[0])
	if !ok {
		t.Fatal("missing provider root")
	}
	c.observeAt(context.Background(), ref, now.Add(95*time.Second))

	published := store.PublishedSnapshot().Sessions[0]
	if published.Codex == nil || published.Codex.Status != state.StatusLimited {
		t.Fatalf("published codex block = %+v, want limited", published.Codex)
	}
	if published.UsageLimit == nil || published.UsageLimit.ResetsAt == nil || !published.UsageLimit.ResetsAt.Equal(resetsAt) {
		t.Fatalf("usage_limit = %+v, want resets_at %v", published.UsageLimit, resetsAt)
	}
	if got := codexGraph(t, store).Summary.Status; got != state.StatusIdle {
		t.Fatalf("internal graph status = %s, want the turn's idle under the limit", got)
	}
}

func TestCodexTranscriptPollShouldNotLimitWhenActivityOutranTheRead(t *testing.T) {
	c, store := newStandardCodexHookTestCoordinator(t, "thread-1")
	now := time.Now()
	path, _ := codexUsageLimitRollout(t, now)
	sendCodexHook(c, store, rpc.Request{Event: "PostToolUse", SessionID: "thread-1", ToolName: "exec_command", Transcript: path, ObservedAt: now})
	ref, ok := providerRootRef(store.Snapshot().Sessions[0])
	if !ok {
		t.Fatal("missing provider root")
	}
	// The next prompt's hook reached the daemon (rpc applyUsageLimitHook) before
	// the quiet-window read did.
	store.Apply(func(sessions map[int]*state.Session) {
		for _, sess := range sessions {
			sess.ClearUsageLimit(now.Add(2 * time.Second))
		}
	})
	c.observeAt(context.Background(), ref, now.Add(95*time.Second))
	if published := store.PublishedSnapshot().Sessions[0]; published.UsageLimit != nil {
		t.Fatalf("a stale rollout read greyed a resumed session: %+v", published.UsageLimit)
	}
}
