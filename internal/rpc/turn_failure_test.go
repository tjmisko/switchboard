package rpc

import (
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/state"
)

func TestHookShouldRecordTheFailureWhenTheCtlEdgeReportsOneAndClearItOnTheNextPrompt(t *testing.T) {
	server, store, _ := usageLimitServer(t, state.AgentKindClaude)
	at := time.Now().UTC()
	server.handleHook(Request{PID: 42, Agent: state.AgentKindClaude, Event: "StopFailure", ObservedAt: at, TurnFailure: "unreachable"})

	sess := store.PublishedSnapshot().Sessions[0]
	if sess.AgentGraph.Summary.Status != state.StatusError || sess.TurnFailure == nil ||
		sess.TurnFailure.Kind != state.TurnFailureUnreachable || sess.TurnFailure.Source != state.TurnFailureSourceClaudeHook {
		t.Fatalf("published %q %+v, want error from the claude hook", sess.AgentGraph.Summary.Status, sess.TurnFailure)
	}

	server.handleHook(Request{PID: 42, Agent: state.AgentKindClaude, Event: "UserPromptSubmit", ObservedAt: at.Add(time.Second)})
	sess = store.PublishedSnapshot().Sessions[0]
	if sess.AgentGraph.Summary.Status == state.StatusError || sess.TurnFailure != nil {
		t.Fatalf("after prompt: published %q %+v, want the failure cleared", sess.AgentGraph.Summary.Status, sess.TurnFailure)
	}
}
