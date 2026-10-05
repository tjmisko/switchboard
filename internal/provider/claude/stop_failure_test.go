package claude

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
)

// Claude Code fires StopFailure, and no Stop, when an API error ends the turn
// (hookcap 2026-09-29: UserPromptSubmit → StopFailure(rate_limit) → nothing).
func TestStopFailureShouldEndTheRootTurnWhenNoStopFollows(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	o.ApplyHook(HookSignal{Root: root, Event: "UserPromptSubmit", At: now})
	appendClaudeLine(t, root.Transcript, fmt.Sprintf(`{"type":"assistant","isApiErrorMessage":true,"error":"rate_limit","apiErrorStatus":429,"timestamp":%q,"message":{"role":"assistant","content":[{"type":"text","text":"You've hit your session limit · resets 4pm (America/Los_Angeles)"}]}}`,
		now.Add(500*time.Millisecond).Format(time.RFC3339Nano)))
	o.ApplyHook(HookSignal{Root: root, Event: "StopFailure", At: now.Add(time.Second)})

	for _, elapsed := range []time.Duration{2 * time.Second, 95 * time.Second} {
		obs, err := o.Observe(context.Background(), root, now.Add(elapsed))
		if err != nil {
			t.Fatal(err)
		}
		assertSummary(t, obs, now.Add(elapsed), agentgraph.LegacyIdle, agentgraph.AttentionNone)
	}
}

func TestStopFailureFromASubagentShouldNotEndTheRootTurn(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	o.ApplyHook(HookSignal{Root: root, Event: "UserPromptSubmit", At: now})
	o.ApplyHook(HookSignal{Root: root, Event: "StopFailure", AgentID: "child", AgentType: "researcher", At: now.Add(time.Second)})
	obs, err := o.Observe(context.Background(), root, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	assertSummary(t, obs, now.Add(2*time.Second), agentgraph.LegacyWorking, agentgraph.AttentionNone)
}
