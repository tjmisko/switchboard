package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/state"
)

// 21:28 UTC is 2:28pm in Los Angeles, before the 4pm reset the payloads name.
var hookAt = time.Date(2026, 10, 5, 21, 28, 0, 0, time.UTC)

// claudeStopFailure is the shape hookcap recorded for a real Claude Code limit
// (2026-09-29), with the scrubbed text restored to the transcript's.
func claudeStopFailure(errorType, message string) []byte {
	body, _ := json.Marshal(map[string]any{
		"session_id": "1ef7d551-8ce8-4817-a946-053e92281db6", "hook_event_name": "StopFailure",
		"transcript_path": "/home/u/.claude/projects/p/1ef7d551.jsonl", "cwd": "/home/u/p",
		"error": errorType, "error_details": nil, "last_assistant_message": message,
	})
	return body
}

func TestParseHookPayloadShouldReportAUsageLimitWhenClaudeHitsItsSessionLimit(t *testing.T) {
	req := parseHookPayloadAt(claudeStopFailure("rate_limit", "You've hit your session limit · resets 4pm (America/Los_Angeles)"), "StopFailure", state.AgentKindClaude, hookAt)
	if !req.UsageLimit || req.UsageLimitResetsAt == nil {
		t.Fatalf("usage_limit=%v resets_at=%v, want a limit with a reset", req.UsageLimit, req.UsageLimitResetsAt)
	}
	if want := time.Date(2026, 10, 5, 23, 0, 0, 0, time.UTC); !req.UsageLimitResetsAt.Equal(want) {
		t.Fatalf("resets_at = %v, want %v", req.UsageLimitResetsAt, want)
	}
	if req.SessionID != "1ef7d551-8ce8-4817-a946-053e92281db6" {
		t.Fatalf("session_id = %q; the limit must not cost the lifecycle fields", req.SessionID)
	}
}

func TestParseHookPayloadShouldNotReportAUsageLimitWhenA429IsTransient(t *testing.T) {
	for _, body := range [][]byte{
		claudeStopFailure("rate_limit", "API Error: 429 This request would exceed the rate limit for your organization"),
		claudeStopFailure("rate_limit", ""),
		claudeStopFailure("server_error", "API Error: 500 Internal server error"),
		claudeStopFailure("overloaded", "API Error: 529 Overloaded"),
		[]byte(`{"session_id":"s","error":{"type":"rate_limit"},"last_assistant_message":"You've hit your session limit · resets 4pm"}`),
		[]byte(`{"session_id":"s","error":7}`),
		[]byte(`{"session_id":"s"`),
		nil,
	} {
		if req := parseHookPayloadAt(body, "StopFailure", state.AgentKindClaude, hookAt); req.UsageLimit {
			t.Errorf("payload %s reported a usage limit", body)
		}
	}
}

func TestParseHookPayloadShouldKeepLifecycleFieldsWhenErrorIsNotAString(t *testing.T) {
	req := parseHookPayloadAt([]byte(`{"session_id":"thread-1","turn_id":"turn-1","error":{"message":"x"}}`), "Stop", state.AgentKindCodex, hookAt)
	if req.SessionID != "thread-1" || req.TurnID != "turn-1" {
		t.Fatalf("an object-valued error broke the decode: %+v", req)
	}
}

func TestParseHookPayloadShouldOnlyClassifyStopFailure(t *testing.T) {
	body := claudeStopFailure("rate_limit", "You've hit your session limit · resets 4pm (America/Los_Angeles)")
	for _, event := range []string{"Stop", "PostToolUse", "UserPromptSubmit"} {
		if req := parseHookPayloadAt(body, event, state.AgentKindClaude, hookAt); req.UsageLimit {
			t.Errorf("%s reported a usage limit", event)
		}
	}
}

func TestParseHookPayloadShouldReportAPiUsageLimitWithItsRelativeReset(t *testing.T) {
	body := []byte(`{"session_id":"pi-1","cwd":"/home/u/p","error_message":"You have hit your ChatGPT usage limit (plus plan). Try again in ~47 min."}`)
	req := parseHookPayloadAt(body, "StopFailure", state.AgentKindPi, hookAt)
	if !req.UsageLimit || req.UsageLimitResetsAt == nil || !req.UsageLimitResetsAt.Equal(hookAt.Add(47*time.Minute)) {
		t.Fatalf("usage_limit=%v resets_at=%v, want a limit 47 minutes out", req.UsageLimit, req.UsageLimitResetsAt)
	}
	if req.Agent != state.AgentKindPi {
		t.Fatalf("agent = %q, want pi", req.Agent)
	}
}

func TestParseHookPayloadShouldNeverForwardTheErrorText(t *testing.T) {
	secret := "You've hit your session limit · resets 4pm (America/Los_Angeles) SECRET-PROJECT-NAME"
	req := parseHookPayloadAt(claudeStopFailure("rate_limit", secret), "StopFailure", state.AgentKindClaude, hookAt)
	wire, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), "SECRET-PROJECT-NAME") || strings.Contains(string(wire), "hit your") {
		t.Fatalf("error text crossed the privacy boundary: %s", wire)
	}
}
