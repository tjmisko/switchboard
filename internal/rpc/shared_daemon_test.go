package rpc

import (
	"fmt"
	"testing"

	"github.com/tjmisko/switchboard/internal/proc"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/terminal"
	"github.com/tjmisko/switchboard/internal/wm"
)

func TestSharedDaemonRoutesSeparateDirectoriesWithoutRotatingLaunchingTUI(t *testing.T) {
	store := state.New("")
	store.Apply(func(m map[int]*state.Session) {
		m[100] = &state.Session{PID: 100, Agent: state.AgentKindCodex, CWD: "/functionary"}
		m[200] = &state.Session{PID: 200, Agent: state.AgentKindCodex, CWD: "/switchboard"}
	})
	s := New(store, "", terminal.NewNone(), wm.NewNone())
	processes := map[int]proc.Info{
		101: {PID: 101, PPID: 100, Comm: "codex", Args: []string{"codex", "app-server", "--managed-daemon"}},
		100: {PID: 100, Comm: "codex", Args: []string{"codex"}, CWD: "/functionary"},
		200: {PID: 200, Comm: "codex", Args: []string{"codex"}, CWD: "/switchboard"},
	}
	s.readProc = func(pid int) (proc.Info, error) {
		p, ok := processes[pid]
		if !ok {
			return p, fmt.Errorf("gone")
		}
		return p, nil
	}
	var deliveries []string
	s.SetAgentHookHandler(func(req Request, sess state.Session) {
		deliveries = append(deliveries, fmt.Sprintf("%d/%s/%s", sess.PID, req.SessionID, req.Event))
	})
	for _, req := range []Request{
		{PID: 101, Agent: state.AgentKindCodex, HookCWD: "/switchboard", SessionID: "switchboard-thread", Event: "PreToolUse"},
		{PID: 101, Agent: state.AgentKindCodex, HookCWD: "/functionary", SessionID: "functionary-thread", Event: "PermissionRequest"},
		{PID: 101, Agent: state.AgentKindCodex, HookCWD: "/switchboard", SessionID: "switchboard-thread", Event: "PostToolUse"},
		{PID: 101, Agent: state.AgentKindCodex, HookCWD: "/functionary", SessionID: "functionary-new-thread", Event: "SessionStart", HookSource: "clear"},
	} {
		s.dispatchAgentHook(req)
	}
	want := []string{"200/switchboard-thread/PreToolUse", "100/functionary-thread/PermissionRequest", "200/switchboard-thread/PostToolUse", "100/functionary-new-thread/SessionStart"}
	if fmt.Sprint(deliveries) != fmt.Sprint(want) {
		t.Fatalf("deliveries=%v want=%v", deliveries, want)
	}
	// Two live clients in one directory are deliberately unresolved, even when
	// one was the daemon launcher or has a previous matching conversation ID.
	store.Apply(func(m map[int]*state.Session) { m[200].CWD = "/functionary" })
	p := processes[200]
	p.CWD = "/functionary"
	processes[200] = p
	s.dispatchAgentHook(Request{PID: 101, Agent: state.AgentKindCodex, HookCWD: "/functionary", SessionID: "functionary-thread", Event: "PermissionRequest"})
	if len(deliveries) != len(want) {
		t.Fatal("ambiguous hook was delivered")
	}
}

func TestSharedDirectoryFallbackRequiresDaemonAndLiveDirectory(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		args                           []string
		hookCWD, liveCWD, processState string
		deliver                        bool
	}{
		{"configured daemon", []string{"codex", "-c", "model=example", "app-server"}, "/project", "/project", "S", true},
		{"missing directory", []string{"codex", "app-server"}, "", "/project", "S", false},
		{"relative directory", []string{"codex", "app-server"}, "project", "/project", "S", false},
		{"stale directory", []string{"codex", "app-server"}, "/project", "/elsewhere", "S", false},
		{"zombie client", []string{"codex", "app-server"}, "/project", "/project", "Z", false},
		{"nested exec", []string{"codex", "exec"}, "/project", "/project", "S", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := state.New("")
			store.Apply(func(m map[int]*state.Session) {
				m[100] = &state.Session{PID: 100, Agent: state.AgentKindCodex, CWD: "/project"}
			})
			s := New(store, "", terminal.NewNone(), wm.NewNone())
			s.readProc = func(pid int) (proc.Info, error) {
				if pid == 101 {
					return proc.Info{PID: 101, PPID: 100, Comm: "codex", Args: tc.args}, nil
				}
				return proc.Info{PID: 100, Comm: "codex", Args: []string{"codex"}, CWD: tc.liveCWD, State: tc.processState}, nil
			}
			delivered := false
			s.SetAgentHookHandler(func(Request, state.Session) { delivered = true })
			s.dispatchAgentHook(Request{PID: 101, Agent: state.AgentKindCodex, HookCWD: tc.hookCWD, SessionID: "thread", Event: "PreToolUse"})
			if delivered != tc.deliver {
				t.Fatalf("delivered=%t want=%t", delivered, tc.deliver)
			}
		})
	}
}
