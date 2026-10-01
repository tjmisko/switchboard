package claude

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
)

func TestHandbackCompletionAfterRootStopStaysIdle(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	o.ApplyHook(HookSignal{Root: root, Event: "UserPromptSubmit", At: now})
	for _, id := range []string{"first", "second"} {
		writeClaudeChild(t, filepath.Join(filepath.Dir(root.Transcript), root.ProviderSessionID, "subagents"), id, "researcher", "report", "", now)
		path := filepath.Join(filepath.Dir(root.Transcript), root.ProviderSessionID, "subagents", "agent-"+id+".jsonl")
		appendClaudeLine(t, path, fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"role":"assistant","content":[{"type":"tool_use","name":"SubagentHandback","id":"handback"}]}}`, now.Add(time.Second).Format(time.RFC3339Nano)))
		appendClaudeLine(t, path, fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"handback","content":"success"}]}}`, now.Add(2*time.Second).Format(time.RFC3339Nano)))
	}
	appendClaudeLine(t, root.Transcript, fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"Done"}]}}`, now.Add(3*time.Second).Format(time.RFC3339Nano)))
	o.ApplyHook(HookSignal{Root: root, Event: "Stop", At: now.Add(3*time.Second + 10*time.Millisecond)})
	appendClaudeLine(t, root.Transcript, fmt.Sprintf(`{"type":"user","queueTranscriptOnly":true,"timestamp":%q,"message":{"role":"user","content":"<task-notification><task-id>second</task-id><status>completed</status></task-notification>"}}`, now.Add(3*time.Second+20*time.Millisecond).Format(time.RFC3339Nano)))
	for _, elapsed := range []time.Duration{4 * time.Second, 95 * time.Second, 10 * time.Minute} {
		obs, err := o.Observe(context.Background(), root, now.Add(elapsed))
		if err != nil {
			t.Fatal(err)
		}
		assertSummary(t, obs, now.Add(elapsed), agentgraph.LegacyIdle, agentgraph.AttentionNone)
		if o.Projection(root.Key()).InFlightSubagents != 0 {
			t.Fatal("finished children still in flight")
		}
	}
	// A real follow-up to the same child reopens it even before its transcript flushes.
	o.ApplyHook(HookSignal{Root: root, Event: "SubagentStart", AgentID: "second", At: now.Add(11 * time.Minute)})
	obs, err := o.Observe(context.Background(), root, now.Add(11*time.Minute+time.Second))
	if err != nil {
		t.Fatal(err)
	}
	assertSummary(t, obs, now.Add(11*time.Minute+time.Second), agentgraph.LegacyDelegating, agentgraph.AttentionNone)
	o.ApplyHook(HookSignal{Root: root, Event: "SubagentStop", AgentID: "second", At: now.Add(12 * time.Minute)})
	obs, err = o.Observe(context.Background(), root, now.Add(12*time.Minute+time.Second))
	if err != nil {
		t.Fatal(err)
	}
	assertSummary(t, obs, now.Add(12*time.Minute+time.Second), agentgraph.LegacyIdle, agentgraph.AttentionNone)
}

func TestTranscriptStopPollingRequiresQuietHooksAndTerminalEvidence(t *testing.T) {
	for _, scenario := range []string{"stopped", "silent", "later prompt", "later teammate message", "prevented stop"} {
		t.Run(scenario, func(t *testing.T) {
			o, root, now := newTestObserver(t)
			defer o.Close()
			o.ApplyHook(HookSignal{Root: root, Event: "UserPromptSubmit", At: now})
			if scenario != "silent" {
				appendClaudeLine(t, root.Transcript, fmt.Sprintf(`{"type":"system","subtype":"stop_hook_summary","preventedContinuation":%t,"timestamp":%q}`, scenario == "prevented stop", now.Add(time.Second).Format(time.RFC3339Nano)))
			}
			if scenario == "later prompt" {
				o.ApplyHook(HookSignal{Root: root, Event: "UserPromptSubmit", At: now.Add(2 * time.Second)})
			}
			if scenario == "later teammate message" {
				appendClaudeLine(t, root.Transcript, fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":"Another Claude session sent a message"}}`, now.Add(2*time.Second).Format(time.RFC3339Nano)))
			}
			obs, err := o.Observe(context.Background(), root, now.Add(89*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			assertSummary(t, obs, now.Add(89*time.Second), agentgraph.LegacyWorking, agentgraph.AttentionNone)
			obs, err = o.Observe(context.Background(), root, now.Add(95*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			want := agentgraph.LegacyWorking
			if scenario == "stopped" {
				want = agentgraph.LegacyIdle
			}
			assertSummary(t, obs, now.Add(95*time.Second), want, agentgraph.AttentionNone)
		})
	}
}

func TestPollingHealsRestoredFalseGreenWithNewerStatusAnchor(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	appendClaudeLine(t, root.Transcript, fmt.Sprintf(`{"type":"system","subtype":"stop_hook_summary","timestamp":%q}`, now.Format(time.RFC3339Nano)))
	if _, err := o.Restore(root, Compatibility{Status: "working", StatusSince: now.Add(time.Second)}, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	obs, err := o.Observe(context.Background(), root, now.Add(95*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	assertSummary(t, obs, now.Add(95*time.Second), agentgraph.LegacyIdle, agentgraph.AttentionNone)
}
