package main

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/state"
)

// The events integrations/pi/switchboard.ts sends, in Claude's vocabulary.
var piHookEvents = []string{
	"SessionStart", "SessionEnd", "UserPromptSubmit", "PreToolUse", "PostToolUse",
	"PermissionRequest", "PermissionResolved", "Usage", "Stop", "StopFailure", "SessionName",
}

func TestPiHookShouldCarryItsSessionIDTranscriptAndDialogCountIntoTheDaemon(t *testing.T) {
	req := hookRequestForEventPayload(t, "PermissionRequest", state.AgentKindPi, `{
		"cwd": "/home/u/project",
		"session_id": "0199a1b2-pi-session",
		"transcript_path": "/home/u/.pi/agent/sessions/--home-u-project--/2026_0199a1b2.jsonl",
		"open_dialogs": 2
	}`)

	if req.Agent != state.AgentKindPi || req.Event != "PermissionRequest" || req.ObservedAt.IsZero() {
		t.Fatalf("pi hook routing = %+v", req)
	}
	if req.SessionID != "0199a1b2-pi-session" {
		t.Fatalf("session_id = %q", req.SessionID)
	}
	if req.Transcript != "/home/u/.pi/agent/sessions/--home-u-project--/2026_0199a1b2.jsonl" {
		t.Fatalf("transcript = %q", req.Transcript)
	}
	if req.OpenDialogs == nil || *req.OpenDialogs != 2 {
		t.Fatalf("open_dialogs = %v, want 2", req.OpenDialogs)
	}
}

func TestPiHookShouldCarryAZeroDialogCountWhenTheLastDialogCloses(t *testing.T) {
	req := hookRequestForEventPayload(t, "PermissionResolved", state.AgentKindPi,
		`{"session_id":"pi-1","open_dialogs":0}`)
	if req.OpenDialogs == nil || *req.OpenDialogs != 0 {
		t.Fatalf("open_dialogs = %v, want an explicit 0", req.OpenDialogs)
	}
}

func TestPiHookShouldClampTheDialogCountWhenItIsNegative(t *testing.T) {
	req := parseHookPayloadAt([]byte(`{"open_dialogs":-3}`), "PermissionResolved", state.AgentKindPi, hookAt)
	if req.OpenDialogs == nil || *req.OpenDialogs != 0 {
		t.Fatalf("open_dialogs = %v, want 0", req.OpenDialogs)
	}
}

func TestPiHookShouldCarryNoPromptDialogTitleOrToolInputFromAnyPiEvent(t *testing.T) {
	const secret = "SECRET-USER-CONTENT"
	// Every content-bearing field a Pi event has, and every one a careless
	// extension might forward, spelled the way it would arrive.
	payload := `{
		"session_id": "pi-1",
		"transcript_path": "/home/u/.pi/agent/sessions/s.jsonl",
		"prompt": "` + secret + ` prompt",
		"title": "` + secret + ` dialog title",
		"label": "` + secret + ` herdr label",
		"message": "` + secret + ` message",
		"content": [{"type":"text","text":"` + secret + ` content"}],
		"args": {"command": "` + secret + ` args"},
		"tool_input": {"command": "` + secret + ` tool input"},
		"result": {"output": "` + secret + ` tool result"},
		"last_assistant_message": "` + secret + ` reply",
		"error_message": "` + secret + ` provider error",
		"tool_name": "bash",
		"tool_use_id": "call-1",
		"open_dialogs": 1,
		"input_tokens": 10
	}`
	for _, event := range piHookEvents {
		req := hookRequestForEventPayload(t, event, state.AgentKindPi, payload)
		wire, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("%s: marshal: %v", event, err)
		}
		if strings.Contains(string(wire), secret) {
			t.Errorf("%s put content on the wire: %s", event, wire)
		}
		if req.SessionID != "pi-1" {
			t.Errorf("%s lost its session id: %+v", event, req)
		}
	}
}

func TestPiHookShouldCarryRotationMetadataWhenASessionStarts(t *testing.T) {
	name := strings.Repeat("界", maxPiSessionName+10)
	body := `{"session_id":"pi-2","source":"resume","busy":true,` +
		`"previous_session_file":"/home/u/.pi/agent/sessions/x/../old.jsonl","session_name":"` + name + `"}`
	req := parseHookPayloadAt([]byte(body), "SessionStart", state.AgentKindPi, hookAt)

	if req.HookSource != "resume" || !req.Busy {
		t.Fatalf("source=%q busy=%v, want resume and busy", req.HookSource, req.Busy)
	}
	if req.PreviousSessionFile != "/home/u/.pi/agent/sessions/old.jsonl" {
		t.Fatalf("previous_session_file = %q, want the cleaned path", req.PreviousSessionFile)
	}
	if got := len([]rune(req.SessionName)); got != maxPiSessionName {
		t.Fatalf("session_name runes = %d, want %d", got, maxPiSessionName)
	}
}

func TestPiHookShouldDropThePreviousSessionFileWhenItIsNotAnAbsolutePath(t *testing.T) {
	for _, path := range []string{"old.jsonl", "", "/" + strings.Repeat("a", maxHookPath)} {
		body, _ := json.Marshal(map[string]any{"previous_session_file": path})
		if req := parseHookPayloadAt(body, "SessionStart", state.AgentKindPi, hookAt); req.PreviousSessionFile != "" {
			t.Errorf("previous_session_file %q forwarded as %q", path, req.PreviousSessionFile)
		}
	}
}

func TestPiHookShouldCarryPiOwnCostAndTokenCountsWhenAMessageEnds(t *testing.T) {
	body := `{"session_id":"pi-1","message_id":"resp_1","provider":"openai-codex","model":"gpt-5.5",` +
		`"input_tokens":1200,"output_tokens":300,"cache_read_tokens":4000,"cache_write_tokens":50,` +
		`"total_tokens":5550,"cost_total":0.0123}`
	req := hookRequestForEventPayload(t, "Usage", state.AgentKindPi, body)

	usage := req.Usage
	if usage == nil {
		t.Fatalf("usage missing: %+v", req)
	}
	if usage.MessageID != "resp_1" || usage.Provider != "openai-codex" || usage.Model != "gpt-5.5" {
		t.Fatalf("usage identity = %+v", usage)
	}
	if usage.InputTokens != 1200 || usage.OutputTokens != 300 || usage.CacheReadTokens != 4000 ||
		usage.CacheWriteTokens != 50 || usage.TotalTokens != 5550 {
		t.Fatalf("usage counts = %+v", usage)
	}
	if usage.CostTotal == nil || *usage.CostTotal != 0.0123 {
		t.Fatalf("cost_total = %v, want Pi's own 0.0123", usage.CostTotal)
	}
}

func TestPiHookShouldTellAFreeMessageFromAnUnpricedOneWhenCostIsZeroOrAbsent(t *testing.T) {
	free := parseHookPayloadAt([]byte(`{"cost_total":0}`), "Usage", state.AgentKindPi, hookAt)
	if free.Usage == nil || free.Usage.CostTotal == nil || *free.Usage.CostTotal != 0 {
		t.Fatalf("zero cost = %+v, want an explicit 0", free.Usage)
	}
	unpriced := parseHookPayloadAt([]byte(`{"input_tokens":5}`), "Usage", state.AgentKindPi, hookAt)
	if unpriced.Usage == nil || unpriced.Usage.CostTotal != nil {
		t.Fatalf("absent cost = %+v, want nil", unpriced.Usage)
	}
	negative := parseHookPayloadAt([]byte(`{"cost_total":-1,"input_tokens":-5}`), "Usage", state.AgentKindPi, hookAt)
	if negative.Usage.CostTotal != nil || negative.Usage.InputTokens != 0 {
		t.Fatalf("negative usage = %+v, want no cost and zero tokens", negative.Usage)
	}
}

func TestPiHookShouldCarryEachFieldOnlyOnItsOwnEventWhenAPayloadHasThemAll(t *testing.T) {
	body := []byte(`{"open_dialogs":1,"busy":true,"previous_session_file":"/p.jsonl","session_name":"n","input_tokens":1}`)
	for _, event := range []string{"Stop", "UserPromptSubmit", "PreToolUse", "SessionEnd"} {
		req := parseHookPayloadAt(body, event, state.AgentKindPi, hookAt)
		if req.OpenDialogs != nil || req.Busy || req.PreviousSessionFile != "" || req.SessionName != "" || req.Usage != nil {
			t.Errorf("%s carried another event's field: %+v", event, req)
		}
	}
}

func TestPiFieldsShouldNotReachTheWireWhenClaudeOrCodexSendThem(t *testing.T) {
	body := []byte(`{"open_dialogs":1,"busy":true,"session_name":"n","model":"m","input_tokens":1}`)
	for _, agent := range []string{state.AgentKindClaude, state.AgentKindCodex} {
		for _, event := range []string{"SessionStart", "PermissionRequest", "Usage"} {
			req := parseHookPayloadAt(body, event, agent, hookAt)
			if req.OpenDialogs != nil || req.Busy || req.SessionName != "" || req.Usage != nil {
				t.Errorf("%s %s carried Pi fields: %+v", agent, event, req)
			}
		}
	}
}

func TestPiHookShouldKeepItsSessionIdentityWhenAPiFieldIsMalformed(t *testing.T) {
	body := []byte(`{"session_id":"pi-1","transcript_path":"/t.jsonl","open_dialogs":"two"}`)
	req := parseHookPayloadAt(body, "PermissionRequest", state.AgentKindPi, hookAt)
	if req.SessionID != "pi-1" || req.Transcript != "/t.jsonl" || req.OpenDialogs != nil {
		t.Fatalf("malformed open_dialogs = %+v, want identity kept and no count", req)
	}
}

func piBodyAt(eventAt string) []byte {
	return []byte(`{"session_id":"pi-1","open_dialogs":1,"event_at":` + eventAt + `}`)
}

func TestPiHookShouldStampTheEventInstantWhenEventAtIsSane(t *testing.T) {
	fired := hookAt.Add(-300 * time.Millisecond)
	req := parseHookPayloadAt(piBodyAt(strconv.FormatInt(fired.UnixMilli(), 10)), "PermissionRequest", state.AgentKindPi, hookAt)
	if !req.ObservedAt.Equal(time.UnixMilli(fired.UnixMilli())) {
		t.Fatalf("observed_at = %v, want the Pi event's own instant %v", req.ObservedAt, fired)
	}
}

func TestPiHookShouldUseTheCtlClockWhenEventAtIsOutOfBounds(t *testing.T) {
	cases := map[string]string{
		"far future": strconv.FormatInt(hookAt.Add(piEventAtMaxFuture+time.Second).UnixMilli(), 10),
		"too old":    strconv.FormatInt(hookAt.Add(-piEventAtMaxAge-time.Second).UnixMilli(), 10),
		"malformed":  `"soon"`,
		"zero":       "0",
	}
	for name, eventAt := range cases {
		req := parseHookPayloadAt(piBodyAt(eventAt), "PermissionRequest", state.AgentKindPi, hookAt)
		if !req.ObservedAt.Equal(hookAt) {
			t.Errorf("%s: observed_at = %v, want the ctl clock %v", name, req.ObservedAt, hookAt)
		}
		if req.OpenDialogs == nil || *req.OpenDialogs != 1 {
			t.Errorf("%s: a bad event_at cost the dialog count: %v", name, req.OpenDialogs)
		}
	}
}

func TestPiHookShouldClampEventAtToTheCtlClockWhenItLeadsWithinTheSkew(t *testing.T) {
	ahead := strconv.FormatInt(hookAt.Add(500*time.Millisecond).UnixMilli(), 10)
	req := parseHookPayloadAt(piBodyAt(ahead), "PermissionRequest", state.AgentKindPi, hookAt)
	if !req.ObservedAt.Equal(hookAt) {
		t.Fatalf("observed_at = %v, want it clamped to %v", req.ObservedAt, hookAt)
	}
}

func TestEventAtShouldNotStampClaudeOrCodexHooks(t *testing.T) {
	for _, agent := range []string{state.AgentKindClaude, state.AgentKindCodex} {
		req := parseHookPayloadAt(piBodyAt(strconv.FormatInt(hookAt.Add(-time.Second).UnixMilli(), 10)), "Stop", agent, hookAt)
		if !req.ObservedAt.IsZero() {
			t.Errorf("%s took event_at: %v", agent, req.ObservedAt)
		}
	}
}

func TestPiHookShouldCarryOnlyTheBoundedNameWhenPiIsRenamedMidSession(t *testing.T) {
	name := strings.Repeat("界", maxPiSessionName+10)
	body := `{"session_id":"pi-1","session_name":"` + name + `","busy":true,"open_dialogs":1,"input_tokens":1}`
	req := parseHookPayloadAt([]byte(body), "SessionName", state.AgentKindPi, hookAt)
	if got := len([]rune(req.SessionName)); got != maxPiSessionName || req.SessionID != "pi-1" {
		t.Fatalf("rename = %+v (name runes %d), want the session id and a %d-rune name", req, got, maxPiSessionName)
	}
	if req.Busy || req.OpenDialogs != nil || req.Usage != nil {
		t.Fatalf("rename carried another event's field: %+v", req)
	}
	cleared := parseHookPayloadAt([]byte(`{"session_id":"pi-1","session_name":""}`), "SessionName", state.AgentKindPi, hookAt)
	if cleared.SessionName != "" || cleared.SessionID != "pi-1" {
		t.Fatalf("cleared name = %+v", cleared)
	}
}
