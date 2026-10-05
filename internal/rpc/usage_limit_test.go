package rpc

import (
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/terminal"
	"github.com/tjmisko/switchboard/internal/wm"
)

func usageLimitServer(t *testing.T, agent string) (*Server, *state.Store, *[]Request) {
	t.Helper()
	store := state.New("")
	store.Apply(func(sessions map[int]*state.Session) {
		sessions[42] = &state.Session{
			PID: 42, StartedAt: time.Now().Add(-time.Hour), Agent: agent,
			AgentGraph: &state.AgentGraph{RootID: "root", Summary: state.AgentGraphSummary{Status: state.StatusWorking}},
		}
	})
	server := New(store, "", terminal.NewNone(), wm.NewNone())
	var forwarded []Request
	server.SetAgentHookHandler(func(req Request, _ state.Session) { forwarded = append(forwarded, req) })
	return server, store, &forwarded
}

func publishedStatus(store *state.Store) (string, *state.UsageLimit) {
	sess := store.PublishedSnapshot().Sessions[0]
	return sess.AgentGraph.Summary.Status, sess.UsageLimit
}

func TestHookShouldPublishLimitedWhenTheCtlEdgeReportsAUsageLimit(t *testing.T) {
	server, store, forwarded := usageLimitServer(t, state.AgentKindClaude)
	at := time.Now().UTC()
	resetsAt := at.Add(2 * time.Hour)
	server.handleHook(Request{PID: 42, Agent: state.AgentKindClaude, Event: "StopFailure", ObservedAt: at, UsageLimit: true, UsageLimitResetsAt: &resetsAt})

	status, limit := publishedStatus(store)
	if status != state.StatusLimited || limit == nil || !limit.ResetsAt.Equal(resetsAt) || limit.Source != state.UsageLimitSourceClaudeHook {
		t.Fatalf("published %q %+v, want limited until %v from the claude hook", status, limit, resetsAt)
	}
	if len(*forwarded) != 1 || (*forwarded)[0].Event != "StopFailure" {
		t.Fatalf("provider saw %+v, want the StopFailure itself so the turn ends", *forwarded)
	}
}

func TestHookShouldNotLimitWhenAStopFailureCarriesNoVerdict(t *testing.T) {
	server, store, _ := usageLimitServer(t, state.AgentKindClaude)
	server.handleHook(Request{PID: 42, Agent: state.AgentKindClaude, Event: "StopFailure", ObservedAt: time.Now()})
	if status, limit := publishedStatus(store); status == state.StatusLimited || limit != nil {
		t.Fatalf("a transient failure published %q %+v", status, limit)
	}
}

func TestHookShouldClearTheLimitWhenTheSessionShowsNewActivity(t *testing.T) {
	for _, event := range []string{"UserPromptSubmit", "PreToolUse", "PostToolUse", "PermissionRequest"} {
		t.Run(event, func(t *testing.T) {
			server, store, _ := usageLimitServer(t, state.AgentKindCodex)
			at := time.Now().UTC()
			server.handleHook(Request{PID: 42, Agent: state.AgentKindCodex, Event: "StopFailure", ObservedAt: at, UsageLimit: true})
			server.handleHook(Request{PID: 42, Agent: state.AgentKindCodex, Event: event, ObservedAt: at.Add(time.Second)})
			if status, limit := publishedStatus(store); status == state.StatusLimited || limit != nil {
				t.Fatalf("after %s: published %q %+v, want the limit cleared", event, status, limit)
			}
		})
	}
}

func TestHookShouldKeepTheLimitWhenActivityWasObservedBeforeIt(t *testing.T) {
	server, store, _ := usageLimitServer(t, state.AgentKindClaude)
	at := time.Now().UTC()
	server.handleHook(Request{PID: 42, Agent: state.AgentKindClaude, Event: "StopFailure", ObservedAt: at, UsageLimit: true})
	server.handleHook(Request{PID: 42, Agent: state.AgentKindClaude, Event: "PostToolUse", ObservedAt: at.Add(-time.Second)})
	if status, _ := publishedStatus(store); status != state.StatusLimited {
		t.Fatalf("a delayed pre-limit hook cleared the limit: %q", status)
	}
}

func TestHookShouldKeepTheLimitWhenTheEventIsNotActivity(t *testing.T) {
	for _, event := range []string{"Stop", "SessionStart", "SubagentStop", "Notification"} {
		server, store, _ := usageLimitServer(t, state.AgentKindClaude)
		at := time.Now().UTC()
		server.handleHook(Request{PID: 42, Agent: state.AgentKindClaude, Event: "StopFailure", ObservedAt: at, UsageLimit: true})
		server.handleHook(Request{PID: 42, Agent: state.AgentKindClaude, Event: event, ObservedAt: at.Add(time.Second)})
		if status, _ := publishedStatus(store); status != state.StatusLimited {
			t.Fatalf("%s cleared the limit: %q", event, status)
		}
	}
}

func TestPiHookShouldLimitAHerdrOnlySession(t *testing.T) {
	server, store, _ := usageLimitServer(t, state.AgentKindPi)
	at := time.Now().UTC()
	server.handleHook(Request{PID: 42, Agent: state.AgentKindPi, Event: "StopFailure", ObservedAt: at, UsageLimit: true})
	status, limit := publishedStatus(store)
	if status != state.StatusLimited || limit.Source != state.UsageLimitSourcePiHook {
		t.Fatalf("pi published %q %+v, want limited from the pi hook", status, limit)
	}
	server.handleHook(Request{PID: 42, Agent: state.AgentKindPi, Event: "UserPromptSubmit", ObservedAt: at.Add(time.Second)})
	if status, _ := publishedStatus(store); status == state.StatusLimited {
		t.Fatal("pi activity did not clear the limit")
	}
}

func TestLegacyPiHookShouldNotCreateAProviderBlock(t *testing.T) {
	store := state.New("")
	store.Apply(func(sessions map[int]*state.Session) {
		sessions[42] = &state.Session{PID: 42, Agent: state.AgentKindPi}
	})
	server := New(store, "", terminal.NewNone(), wm.NewNone())
	server.handleHook(Request{PID: 42, Agent: state.AgentKindPi, Event: "UserPromptSubmit", ObservedAt: time.Now()})
	if sess := store.Snapshot().Sessions[0]; sess.Claude != nil || sess.Codex != nil {
		t.Fatalf("pi hook wrote a provider block: claude=%+v codex=%+v", sess.Claude, sess.Codex)
	}
}
