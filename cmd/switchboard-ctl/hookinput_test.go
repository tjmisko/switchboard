package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/rpc"
)

// hashToolInput is the "which call" correlator forwarded as
// rpc.Request.ToolInputHash. The contract these tests pin:
//
//   - the same logical input hashes identically no matter how the emitter
//     serialized it (T6's whole reason for canonicalizing);
//   - a no-signal input yields "" and never a hash that could accidentally
//     match another signal-less event;
//   - nothing panics, because cmdHook silences failures and a panic there would
//     take the agent's hook down with it.

func TestHashToolInputShouldMatchWhenKeyOrderDiffers(t *testing.T) {
	// The load-bearing case: PermissionRequest and PostToolUse describe the same
	// call, but nothing guarantees the two emitters serialize the object's keys
	// in the same order.
	permissionRequest := json.RawMessage(`{"command":"git status","description":"check tree","timeout":120}`)
	postToolUse := json.RawMessage(`{"timeout":120,"description":"check tree","command":"git status"}`)

	got, want := hashToolInput(postToolUse), hashToolInput(permissionRequest)
	if want == "" {
		t.Fatal("expected a non-empty hash for a populated tool_input")
	}
	if got != want {
		t.Errorf("key order changed the hash: PermissionRequest=%q PostToolUse=%q", want, got)
	}
}

func TestHashToolInputShouldMatchWhenNestedKeyOrderDiffers(t *testing.T) {
	// Canonicalization has to reach every level, not just the top one — tool
	// inputs nest (e.g. MCP tool args, structured edits).
	a := json.RawMessage(`{"edits":[{"new":"b","old":"a"}],"file":{"path":"/x","mode":"rw"}}`)
	b := json.RawMessage(`{"file":{"mode":"rw","path":"/x"},"edits":[{"old":"a","new":"b"}]}`)

	if hashToolInput(a) != hashToolInput(b) {
		t.Errorf("nested key order changed the hash: %q vs %q", hashToolInput(a), hashToolInput(b))
	}
}

func TestHashToolInputShouldMatchWhenWhitespaceDiffers(t *testing.T) {
	compact := json.RawMessage(`{"command":"ls"}`)
	pretty := json.RawMessage("{\n  \"command\" : \"ls\"\n}")

	if hashToolInput(compact) != hashToolInput(pretty) {
		t.Errorf("insignificant whitespace changed the hash: %q vs %q",
			hashToolInput(compact), hashToolInput(pretty))
	}
}

func TestHashToolInputShouldPreserveArrayOrderWhenElementsAreReordered(t *testing.T) {
	// Object key order is insignificant in JSON; array order is not. Normalizing
	// it away would make two genuinely different calls collide.
	first := json.RawMessage(`{"paths":["a","b"]}`)
	second := json.RawMessage(`{"paths":["b","a"]}`)

	if hashToolInput(first) == hashToolInput(second) {
		t.Error("expected array order to be significant, but the two inputs hashed alike")
	}
}

func TestHashToolInputShouldReturnEmptyWhenInputIsAbsent(t *testing.T) {
	// The common case: UserPromptSubmit/Stop/SessionStart carry no tool_input at
	// all, so the field decodes to a nil RawMessage.
	var absent json.RawMessage
	if got := hashToolInput(absent); got != "" {
		t.Errorf("expected an empty hash for absent tool_input, got %q", got)
	}
}

func TestHashToolInputShouldReturnEmptyWhenInputIsMalformed(t *testing.T) {
	// A truncated or non-JSON tool_input must degrade to "no signal" rather than
	// panicking — cmdHook silences hook failures and must stay that way.
	malformed := []json.RawMessage{
		json.RawMessage(`{"command":`),
		json.RawMessage(`not json at all`),
		json.RawMessage(`{"a":1,}`),
		json.RawMessage(`"`),
		json.RawMessage("\x00\x01\x02"),
	}
	for _, raw := range malformed {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("hashToolInput panicked on %q: %v", string(raw), r)
				}
			}()
			if got := hashToolInput(raw); got != "" {
				t.Errorf("expected an empty hash for malformed input %q, got %q", string(raw), got)
			}
		}()
	}
}

func TestHashToolInputShouldReturnEmptyWhenInputCarriesNoSignal(t *testing.T) {
	// null and empty containers are not evidence of *which* call this is. Hashing
	// them would mint one digest shared by every signal-less event, which would
	// then false-match — the exact failure this correlator exists to prevent.
	for _, raw := range []json.RawMessage{
		json.RawMessage(`null`),
		json.RawMessage(`{}`),
		json.RawMessage(`[]`),
	} {
		if got := hashToolInput(raw); got != "" {
			t.Errorf("expected an empty hash for no-signal input %q, got %q", string(raw), got)
		}
	}
}

func TestHashToolInputShouldDifferWhenInputsDiffer(t *testing.T) {
	// The point of the correlator: two Bash calls by the same writer must be
	// distinguishable, including when they differ only slightly.
	cases := []struct {
		name string
		a, b json.RawMessage
	}{
		{"different command", json.RawMessage(`{"command":"rm -rf /"}`), json.RawMessage(`{"command":"ls"}`)},
		{"one character apart", json.RawMessage(`{"command":"ls a"}`), json.RawMessage(`{"command":"ls b"}`)},
		{"extra key", json.RawMessage(`{"command":"ls"}`), json.RawMessage(`{"command":"ls","timeout":1}`)},
		{"different key same value", json.RawMessage(`{"command":"ls"}`), json.RawMessage(`{"cmd":"ls"}`)},
		{"string vs number", json.RawMessage(`{"timeout":"1"}`), json.RawMessage(`{"timeout":1}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if hashToolInput(tc.a) == hashToolInput(tc.b) {
				t.Errorf("expected different hashes, both were %q", hashToolInput(tc.a))
			}
		})
	}
}

func TestHashToolInputShouldReturnStableShortHexWhenInputIsPopulated(t *testing.T) {
	raw := json.RawMessage(`{"command":"git status"}`)
	got := hashToolInput(raw)

	if len(got) != toolInputHashLen {
		t.Errorf("expected a %d-char digest, got %d (%q)", toolInputHashLen, len(got), got)
	}
	for _, c := range got {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Fatalf("expected lowercase hex, got %q", got)
		}
	}
	if again := hashToolInput(raw); again != got {
		t.Errorf("hash is not deterministic: %q then %q", got, again)
	}
}

func TestHashToolInputShouldNotLeakRawInputWhenInputIsSensitive(t *testing.T) {
	// R4: the raw input can be large and can carry secrets. Only the digest may
	// cross the socket.
	secret := "hunter2-do-not-forward"
	raw := json.RawMessage(`{"command":"export TOKEN=` + secret + `"}`)

	got := hashToolInput(raw)
	if got == "" {
		t.Fatal("expected a hash for a populated tool_input")
	}
	if len(got) != toolInputHashLen {
		t.Errorf("digest should be a fixed %d chars regardless of input size, got %d", toolInputHashLen, len(got))
	}
	if contains(got, secret) {
		t.Errorf("digest %q leaked the raw input", got)
	}
}

// --- transport: what cmdHook actually puts on the wire ---

// hookRequestForPayload runs cmdHook against a real socket with the given hook
// payload on stdin, and returns the rpc.Request the daemon side received.
func hookRequestForPayload(t *testing.T, payload string) rpc.Request {
	return hookRequestForEventPayload(t, "PermissionRequest", "claude", payload)
}

func hookRequestForEventPayload(t *testing.T, event, agent, payload string) rpc.Request {
	t.Helper()

	socket := filepath.Join(t.TempDir(), "s")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	received := make(chan rpc.Request, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var req rpc.Request
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			return
		}
		// cmdHook blocks on Recv, so the daemon side must answer.
		_ = json.NewEncoder(conn).Encode(rpc.Response{OK: true})
		received <- req
	}()

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	origStdin := os.Stdin
	os.Stdin = reader
	defer func() { os.Stdin = origStdin }()
	go func() {
		_, _ = writer.WriteString(payload)
		_ = writer.Close()
	}()

	client, err := rpc.Dial(socket)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	cmdHook(client, event, agent)

	select {
	case req := <-received:
		return req
	case <-time.After(5 * time.Second):
		t.Fatal("daemon side never received the hook request")
		return rpc.Request{}
	}
}

func TestCmdHookShouldForwardContentFreeStandardCodexLifecycleMetadata(t *testing.T) {
	req := hookRequestForEventPayload(t, "PreToolUse", "codex", `{
		"session_id": "thread-new",
		"hook_event_name": "PreToolUse",
		"source": "clear",
		"turn_id": "turn-7",
		"tool_use_id": "call-9",
		"permission_mode": "plan",
		"tool_name": "request_user_input"
	}`)

	if req.HookSource != "clear" || req.TurnID != "turn-7" || req.ToolUseID != "call-9" || req.PermissionMode != "plan" {
		t.Fatalf("Codex lifecycle metadata was not forwarded: %+v", req)
	}
	if req.ToolName != "request_user_input" || req.Agent != "codex" || req.ObservedAt.IsZero() {
		t.Fatalf("standard Codex hook routing fields were disturbed: %+v", req)
	}
}

func TestHookClientHintsShouldUseOnlyBoundedTerminalIdentityAllowlist(t *testing.T) {
	environment := map[string]string{
		"SSH_TTY":         "/dev/pts/7", // duplicate of the direct fd-derived tty
		"WEZTERM_PANE":    "12",
		"TMUX_PANE":       "%3",
		"TERM_SESSION_ID": "must-not-cross-the-hook-socket",
	}
	hints := hookClientHintsFrom("/dev/pts/7", func(key string) string { return environment[key] })
	want := []rpc.HookClientHint{
		{Kind: rpc.HookClientHintTTY, Value: "/dev/pts/7"},
		{Kind: rpc.HookClientHintWeztermPane, Value: "12"},
		{Kind: rpc.HookClientHintTmuxPane, Value: "%3"},
	}
	if len(hints) != len(want) {
		t.Fatalf("hints = %+v, want %+v", hints, want)
	}
	for i := range want {
		if hints[i] != want[i] {
			t.Fatalf("hint[%d] = %+v, want %+v", i, hints[i], want[i])
		}
	}
	body, err := json.Marshal(hints)
	if err != nil {
		t.Fatal(err)
	}
	if contains(string(body), environment["TERM_SESSION_ID"]) {
		t.Fatalf("non-allowlisted environment value crossed the hook socket: %s", body)
	}

	overlong := ""
	for range maxHookClientHintLen + 1 {
		overlong += "x"
	}
	if got := hookClientHintsFrom("", func(string) string { return overlong }); len(got) != 0 {
		t.Fatalf("overlong hook identity was forwarded: %+v", got)
	}
}

func TestCmdHookShouldForwardToolInputHashWhenPayloadCarriesToolInput(t *testing.T) {
	req := hookRequestForPayload(t, `{
		"session_id": "sess-1",
		"transcript_path": "/tmp/t.jsonl",
		"hook_event_name": "PermissionRequest",
		"tool_name": "Bash",
		"tool_input": {"command": "git status", "description": "check"},
		"agent_id": "agent-abc"
	}`)

	if req.ToolInputHash == "" {
		t.Fatal("expected cmdHook to forward a tool_input_hash")
	}
	want := hashToolInput(json.RawMessage(`{"description":"check","command":"git status"}`))
	if req.ToolInputHash != want {
		t.Errorf("forwarded hash %q, want %q", req.ToolInputHash, want)
	}
	// The existing correlators must still ride along untouched.
	if req.ToolName != "Bash" || req.AgentID != "agent-abc" || req.SessionID != "sess-1" {
		t.Errorf("cmdHook disturbed the existing fields: %+v", req)
	}
}

func TestCmdHookShouldForwardEmptyToolInputHashWhenPayloadHasNoToolInput(t *testing.T) {
	req := hookRequestForPayload(t, `{
		"session_id": "sess-1",
		"transcript_path": "/tmp/t.jsonl",
		"hook_event_name": "Stop"
	}`)

	if req.ToolInputHash != "" {
		t.Errorf("expected no tool_input_hash for a payload without tool_input, got %q", req.ToolInputHash)
	}
}

func TestCmdHookShouldNotPutRawToolInputOnTheWireWhenPayloadCarriesSecrets(t *testing.T) {
	// R4: the raw input must never leave the ctl edge, on any field.
	secret := "hunter2-do-not-forward"
	req := hookRequestForPayload(t, `{
		"session_id": "sess-1",
		"tool_name": "Bash",
		"tool_input": {"command": "export TOKEN=`+secret+`"}
	}`)

	encoded, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if contains(string(encoded), secret) {
		t.Errorf("raw tool_input leaked onto the wire: %s", encoded)
	}
	if req.ToolInputHash == "" {
		t.Error("expected the input to still be correlatable via its hash")
	}
}

func TestCmdHookShouldStillSendWhenPayloadIsMalformed(t *testing.T) {
	// A broken hook must never block the agent: an unparseable payload still
	// sends, just without any parsed fields.
	req := hookRequestForPayload(t, `{"session_id": "sess-1", "tool_input":`)

	if req.Cmd != "hook" || req.Event != "PermissionRequest" {
		t.Errorf("expected the hook request to be sent anyway, got %+v", req)
	}
	if req.ToolInputHash != "" {
		t.Errorf("expected no hash from a malformed payload, got %q", req.ToolInputHash)
	}
}

// contains avoids pulling strings in for one assertion.
func contains(haystack, needle string) bool {
	if len(needle) > len(haystack) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
