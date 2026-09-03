package rpc

import (
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/osproc"
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

func TestUnattributedCodexHookClientIdentityRemainsDiagnosticOnly(t *testing.T) {
	tests := []struct {
		name     string
		sessions []state.Session
		hints    []HookClientHint
		want     []string
		wantPID  int
	}{
		{
			name: "unique tty",
			sessions: []state.Session{
				{PID: 41, Agent: state.AgentKindCodex, TTY: "/dev/pts/1"},
				{PID: 42, Agent: state.AgentKindCodex, TTY: "/dev/pts/2"},
			},
			hints:   []HookClientHint{{Kind: HookClientHintTTY, Value: "/dev/pts/2"}},
			want:    []string{"hook_client_hint_tty_present", "hook_client_identity_unique"},
			wantPID: 42,
		},
		{
			name: "unique wezterm pane",
			sessions: []state.Session{
				{PID: 41, Agent: state.AgentKindCodex, Wezterm: &state.WeztermInfo{PaneID: 7}},
				{PID: 42, Agent: state.AgentKindCodex, Wezterm: &state.WeztermInfo{PaneID: 9}},
			},
			hints:   []HookClientHint{{Kind: HookClientHintWeztermPane, Value: "9"}},
			want:    []string{"hook_client_hint_wezterm_pane_present", "hook_client_identity_unique"},
			wantPID: 42,
		},
		{
			name: "ambiguous",
			sessions: []state.Session{
				{PID: 41, Agent: state.AgentKindCodex, TTY: "/dev/pts/1"},
				{PID: 42, Agent: state.AgentKindCodex, TTY: "/dev/pts/1"},
			},
			hints: []HookClientHint{{Kind: HookClientHintTTY, Value: "/dev/pts/1"}},
			want:  []string{"hook_client_hint_tty_present", "hook_client_identity_ambiguous"},
		},
		{
			name: "unmatched",
			sessions: []state.Session{
				{PID: 41, Agent: state.AgentKindCodex, TTY: "/dev/pts/1"},
			},
			hints: []HookClientHint{{Kind: HookClientHintTTY, Value: "/dev/pts/8"}},
			want:  []string{"hook_client_hint_tty_present", "hook_client_identity_unmatched"},
		},
		{
			name: "tmux presence is retained without an unsafe join",
			sessions: []state.Session{
				{PID: 41, Agent: state.AgentKindCodex, TTY: "/dev/pts/1"},
			},
			hints: []HookClientHint{{Kind: HookClientHintTmuxPane, Value: "%3"}},
			want:  []string{"hook_client_hint_tmux_pane_present", "hook_client_identity_unmatched"},
		},
		{
			name: "absent",
			sessions: []state.Session{
				{PID: 41, Agent: state.AgentKindCodex, TTY: "/dev/pts/1"},
			},
			want: []string{"hook_client_identity_absent"},
		},
		{
			name: "overlong value is ignored",
			sessions: []state.Session{
				{PID: 41, Agent: state.AgentKindCodex, TTY: "/dev/pts/1"},
			},
			hints: []HookClientHint{
				{Kind: HookClientHintTTY, Value: strings.Repeat("x", maxHookClientHintValueLen+1)},
				{Kind: HookClientHintTTY, Value: "/dev/pts/1"},
			},
			want:    []string{"hook_client_hint_tty_present", "hook_client_identity_unique"},
			wantPID: 41,
		},
		{
			name: "hints after the bounded prefix are ignored",
			sessions: []state.Session{
				{PID: 41, Agent: state.AgentKindCodex, TTY: "/dev/pts/9"},
			},
			hints: []HookClientHint{
				{Kind: HookClientHintTTY, Value: "/dev/pts/1"},
				{Kind: HookClientHintTTY, Value: "/dev/pts/2"},
				{Kind: HookClientHintTTY, Value: "/dev/pts/3"},
				{Kind: HookClientHintTTY, Value: "/dev/pts/4"},
				{Kind: HookClientHintTTY, Value: "/dev/pts/9"},
			},
			want: []string{"hook_client_hint_tty_present", "hook_client_identity_unmatched"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := state.New("")
			store.Apply(func(sessions map[int]*state.Session) {
				for i := range test.sessions {
					session := test.sessions[i]
					sessions[session.PID] = &session
				}
			})
			server := New(store, "", terminal.NewNone(), wm.NewNone())
			server.readProc = func(pid int) (osproc.Info, error) {
				return osproc.Info{PID: pid, PPID: 1, Comm: "sh"}, nil
			}
			delivered := false
			server.SetAgentHookHandler(func(Request, state.Session) { delivered = true })
			var diagnostics []HookAttributionDiagnostic
			server.SetHookAttributionDiagnostic(func(diagnostic HookAttributionDiagnostic) {
				diagnostics = append(diagnostics, diagnostic)
			})

			server.handleHook(Request{
				Cmd: "hook", Agent: state.AgentKindCodex, Event: "SessionStart",
				PID: 900, SessionID: "root-thread", HookClientHints: test.hints,
			})
			if delivered {
				t.Fatal("diagnostic-only client identity authorized hook delivery")
			}
			if len(diagnostics) != len(test.want) {
				t.Fatalf("diagnostics = %v, want %v", diagnostics, test.want)
			}
			for i := range test.want {
				if diagnostics[i].Category != test.want[i] {
					t.Fatalf("diagnostic[%d] = %q, want %q", i, diagnostics[i].Category, test.want[i])
				}
			}
			if got := diagnostics[len(diagnostics)-1].MatchedPID; got != test.wantPID {
				t.Fatalf("matched pid = %d, want %d", got, test.wantPID)
			}
		})
	}
}

func TestAttributedCodexHookDoesNotRunClientIdentityProbe(t *testing.T) {
	store := state.New("")
	store.Apply(func(sessions map[int]*state.Session) {
		sessions[41] = &state.Session{PID: 41, Agent: state.AgentKindCodex, TTY: "/dev/pts/1"}
	})
	server := New(store, "", terminal.NewNone(), wm.NewNone())
	delivered := false
	server.SetAgentHookHandler(func(Request, state.Session) { delivered = true })
	var diagnostics []HookAttributionDiagnostic
	server.SetHookAttributionDiagnostic(func(diagnostic HookAttributionDiagnostic) { diagnostics = append(diagnostics, diagnostic) })

	server.handleHook(Request{
		Cmd: "hook", Agent: state.AgentKindCodex, Event: "SessionStart",
		PID: 41, SessionID: "root-thread",
		HookClientHints: []HookClientHint{{Kind: HookClientHintTTY, Value: "/dev/pts/1"}},
	})
	if !delivered {
		t.Fatal("ordinary ancestry-attributed hook was not delivered")
	}
	if len(diagnostics) != 0 {
		t.Fatalf("ordinary attributed hook emitted probe diagnostics: %v", diagnostics)
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
