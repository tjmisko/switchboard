package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tjmisko/switchboard/internal/state"
)

func TestParseHookPayloadShouldReportUnreachableWhenClaudeCannotReachTheAPI(t *testing.T) {
	body := claudeStopFailure("server_error", "API Error: Can't reach the API server — check your internet or DNS (EAI_AGAIN)")
	req := parseHookPayloadAt(body, "StopFailure", state.AgentKindClaude, hookAt)
	if req.TurnFailure != "unreachable" || req.UsageLimit {
		t.Fatalf("turn_failure=%q usage_limit=%v, want unreachable and no limit", req.TurnFailure, req.UsageLimit)
	}
	wire, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(wire)), "eai_again") {
		t.Fatalf("request leaks the error text: %s", wire)
	}
}

func TestParseHookPayloadShouldReportOnlyTheUsageLimitWhenBothCouldMatch(t *testing.T) {
	req := parseHookPayloadAt(claudeStopFailure("rate_limit", "You've hit your session limit · resets 4pm (America/Los_Angeles)"), "StopFailure", state.AgentKindClaude, hookAt)
	if !req.UsageLimit || req.TurnFailure != "" {
		t.Fatalf("usage_limit=%v turn_failure=%q, want a limit only", req.UsageLimit, req.TurnFailure)
	}
}

func TestParseHookPayloadShouldReportUnreachableWhenPiHasAConnectionError(t *testing.T) {
	body := []byte(`{"session_id":"pi-1","cwd":"/home/u/p","error_message":"Connection error."}`)
	req := parseHookPayloadAt(body, "StopFailure", state.AgentKindPi, hookAt)
	if req.TurnFailure != "unreachable" {
		t.Fatalf("turn_failure = %q, want unreachable", req.TurnFailure)
	}
}

func TestParseHookPayloadShouldNotReportATurnFailureWhenTheEventIsNotStopFailure(t *testing.T) {
	body := claudeStopFailure("server_error", "API Error: Can't reach the API server (EAI_AGAIN)")
	req := parseHookPayloadAt(body, "Stop", state.AgentKindClaude, hookAt)
	if req.TurnFailure != "" {
		t.Fatalf("turn_failure = %q, want none for Stop", req.TurnFailure)
	}
}
