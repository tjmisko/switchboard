package rpc

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/terminal"
	"github.com/tjmisko/switchboard/internal/wm"
)

func TestGraphHookHandlerReceivesRawIdentityOutsideStoreLock(t *testing.T) {
	store := state.New("")
	store.Apply(func(sessions map[int]*state.Session) {
		sessions[42] = &state.Session{
			PID: 42, StartedAt: time.Now().Add(-time.Minute), Agent: state.AgentKindClaude,
		}
	})
	server := New(store, "", terminal.NewNone(), wm.NewNone())
	entered := make(chan Request, 1)
	release := make(chan struct{})
	server.SetAgentHookHandler(func(req Request, _ state.Session) {
		entered <- req
		<-release
	})

	done := make(chan struct{})
	go func() {
		server.handleHook(Request{PID: 42, Agent: state.AgentKindClaude, Event: "PermissionRequest", AgentID: "agent-agent-child"})
		close(done)
	}()
	var received Request
	select {
	case received = <-entered:
	case <-time.After(time.Second):
		t.Fatal("provider handler was not called")
	}
	if received.AgentID != "agent-agent-child" {
		t.Fatalf("AgentID = %q, want raw value for adapter-owned normalization", received.AgentID)
	}

	readDone := make(chan struct{})
	go func() { store.Snapshot(); close(readDone) }()
	select {
	case <-readDone:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("provider hook callback ran under Store.Apply")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("provider hook callback did not return")
	}
}

func TestAgentDiagnosticWireIsContentFreeAndAdditive(t *testing.T) {
	response := Response{OK: true, Diagnostics: []AgentDiagnostic{{
		Provider: "codex", Category: "observe_error", Count: 3,
		LastAt: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC),
	}}}
	body, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"diagnostics":[{"provider":"codex","category":"observe_error","count":3,"last_at":"2026-08-21T12:00:00Z"}],"ok":true}`
	if string(body) != want {
		t.Fatalf("diagnostic response = %s, want %s", body, want)
	}
}

func TestAgentDiagnosticsCommandReturnsProviderHealth(t *testing.T) {
	server := New(state.New(""), "", terminal.NewNone(), wm.NewNone())
	want := []AgentDiagnostic{{
		Provider: "codex", Category: "exact_binding_unavailable", Count: 2,
		LastAt: time.Date(2026, 8, 21, 13, 0, 0, 0, time.UTC),
	}}
	server.SetAgentDiagnosticSource(func() []AgentDiagnostic { return append([]AgentDiagnostic(nil), want...) })
	serverSide, clientSide := net.Pipe()
	defer clientSide.Close()
	done := make(chan struct{})
	go func() {
		server.handle(t.Context(), serverSide)
		close(done)
	}()
	encoder, decoder := json.NewEncoder(clientSide), json.NewDecoder(clientSide)
	if err := encoder.Encode(Request{Cmd: "agent-diagnostics"}); err != nil {
		t.Fatal(err)
	}
	var response Response
	if err := decoder.Decode(&response); err != nil {
		t.Fatal(err)
	}
	if !response.OK || len(response.Diagnostics) != 1 || response.Diagnostics[0] != want[0] {
		t.Fatalf("response = %+v, want %+v", response, want)
	}
	_ = clientSide.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RPC handler did not exit")
	}
}
