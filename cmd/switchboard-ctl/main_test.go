package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/state"
)

func TestRenderAgentDiagnostics(t *testing.T) {
	var output bytes.Buffer
	renderAgentDiagnostics(&output, []rpc.AgentDiagnostic{
		{Provider: state.AgentKindCodex, Category: "hook_client_identity_unique", Count: 2, LastAt: time.Date(2026, 8, 23, 20, 0, 0, 0, time.FixedZone("offset", -7*60*60))},
	})
	want := "codex hook_client_identity_unique count=2 last_at=2026-08-24T03:00:00Z\n"
	if got := output.String(); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestResolveFocusSelectorNamespacesDuplicatePIDs(t *testing.T) {
	startedA := time.Unix(10, 0).UTC()
	startedB := time.Unix(20, 0).UTC()
	sessions := []state.Session{
		{Hostname: "alpha", PID: 42, StartedAt: startedA, Navigable: true},
		{Hostname: "beta", PID: 42, StartedAt: startedB, Navigable: true},
	}
	if _, err := resolveFocusSelector(sessions, "pid:42"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("duplicate PID error = %v", err)
	}
	token := "host:beta:pid:42:started:" + startedB.Format(time.RFC3339Nano)
	target, err := resolveFocusSelector(sessions, token)
	if err != nil {
		t.Fatal(err)
	}
	if target.Hostname != "beta" || !target.StartedAt.Equal(startedB) {
		t.Fatalf("target = %+v", target)
	}
	stale := "host:beta:pid:42:started:" + startedA.Format(time.RFC3339Nano)
	if _, err := resolveFocusSelector(sessions, stale); err == nil {
		t.Fatal("stale exact token selected a replacement lifetime")
	}
}

func TestAggregateNavigationSkipsUnboundRemoteRows(t *testing.T) {
	local := sess(1, "working")
	local.Hostname, local.Navigable, local.Focused = "local", true, true
	remote := sess(2, "permission")
	remote.Hostname, remote.Navigable = "remote", false

	target, ok := cycleTargetSession([]state.Session{local, remote}, "next")
	if !ok || target.Hostname != "local" {
		t.Fatalf("cycle target = %+v, ok=%t", target, ok)
	}
	if got := pickAttentionExact([]state.Session{local, remote}, &local); got == nil || got.Hostname != "local" {
		t.Fatalf("attention target = %+v", got)
	}
}

func TestRenderAgentDiagnosticsEmpty(t *testing.T) {
	var output bytes.Buffer
	renderAgentDiagnostics(&output, nil)
	if got, want := output.String(), "no agent diagnostics\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

// sess builds a session with the given pid and Claude status. A status of ""
// leaves Claude nil, exercising the unknown fallback.
func sess(pid int, status string) state.Session {
	s := state.Session{PID: pid}
	if status != "" {
		s.Claude = &state.ClaudeInfo{Status: status}
	}
	return s
}

func TestPickAttention(t *testing.T) {
	tests := []struct {
		name       string
		sessions   []state.Session
		focusedPID int // 0 means nothing focused / focused outside the tier
		wantPID    int // 0 means expect nil
	}{
		{
			name:     "should return nil when there are no sessions",
			sessions: nil,
			wantPID:  0,
		},
		{
			name:     "should return nil when working sessions are mixed with unknown ones",
			sessions: []state.Session{sess(1, "working"), sess(2, "unknown"), sess(3, "")},
			wantPID:  0,
		},
		{
			// delegating is green (work happening, no action needed), so it must
			// never qualify for the attention jump — same as working.
			name:     "should ignore delegating sessions (green, no action needed)",
			sessions: []state.Session{sess(1, "working"), sess(2, "delegating"), sess(3, "delegating")},
			wantPID:  0,
		},
		{
			name:     "should jump past a delegating session to a genuinely idle one",
			sessions: []state.Session{sess(1, "delegating"), sess(2, "idle")},
			wantPID:  2,
		},
		{
			name:     "should jump to the first green session when all are green and none focused",
			sessions: []state.Session{sess(1, "working"), sess(2, "working"), sess(3, "working")},
			wantPID:  1,
		},
		{
			name:       "should cycle to the next green session when all are green",
			sessions:   []state.Session{sess(1, "working"), sess(2, "working"), sess(3, "working")},
			focusedPID: 2,
			wantPID:    3,
		},
		{
			name:       "should wrap to the first green session from the last when all are green",
			sessions:   []state.Session{sess(1, "working"), sess(2, "working"), sess(3, "working")},
			focusedPID: 3,
			wantPID:    1,
		},
		{
			name:     "should not cycle green when a single unknown session is present",
			sessions: []state.Session{sess(1, "working"), sess(2, "working"), sess(3, "")},
			wantPID:  0,
		},
		{
			name:     "should jump to the only idle session when no permission exists",
			sessions: []state.Session{sess(1, "working"), sess(2, "idle"), sess(3, "working")},
			wantPID:  2,
		},
		{
			name:     "should jump to the permission session over a working one",
			sessions: []state.Session{sess(1, "working"), sess(2, "permission")},
			wantPID:  2,
		},
		{
			name:     "should prefer permission over idle even when idle comes first",
			sessions: []state.Session{sess(1, "idle"), sess(2, "permission")},
			wantPID:  2,
		},
		{
			name:     "should pick the first permission session when several are waiting",
			sessions: []state.Session{sess(1, "idle"), sess(2, "permission"), sess(3, "permission")},
			wantPID:  2,
		},
		{
			name:     "should pick the first idle session when several are idle and none need permission",
			sessions: []state.Session{sess(1, "working"), sess(2, "idle"), sess(3, "idle")},
			wantPID:  2,
		},
		{
			name:       "should cycle to the next permission session when focused on one",
			sessions:   []state.Session{sess(1, "permission"), sess(2, "permission"), sess(3, "idle")},
			focusedPID: 1,
			wantPID:    2,
		},
		{
			name:       "should wrap to the first permission session from the last",
			sessions:   []state.Session{sess(1, "permission"), sess(2, "permission"), sess(3, "idle")},
			focusedPID: 2,
			wantPID:    1,
		},
		{
			name:       "should ignore idle sessions while cycling when permission sessions exist",
			sessions:   []state.Session{sess(1, "permission"), sess(2, "permission"), sess(3, "idle")},
			focusedPID: 3,
			wantPID:    1,
		},
		{
			name:       "should cycle among idle sessions (three orange and one green)",
			sessions:   []state.Session{sess(1, "idle"), sess(2, "idle"), sess(3, "idle"), sess(4, "working")},
			focusedPID: 2,
			wantPID:    3,
		},
		{
			name:       "should wrap to the first idle session from the last",
			sessions:   []state.Session{sess(1, "idle"), sess(2, "idle"), sess(3, "idle"), sess(4, "working")},
			focusedPID: 3,
			wantPID:    1,
		},
		{
			name:       "should jump to the first tier member when the focused session is not in the tier",
			sessions:   []state.Session{sess(1, "idle"), sess(2, "idle"), sess(3, "working")},
			focusedPID: 3,
			wantPID:    1,
		},
		{
			name:       "should stay put when the top tier has a single member",
			sessions:   []state.Session{sess(1, "working"), sess(2, "permission"), sess(3, "working")},
			focusedPID: 2,
			wantPID:    2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pickAttention(tt.sessions, tt.focusedPID)
			if tt.wantPID == 0 {
				if got != nil {
					t.Fatalf("expected nil, got pid %d", got.PID)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected pid %d, got nil", tt.wantPID)
			}
			if got.PID != tt.wantPID {
				t.Fatalf("expected pid %d, got %d", tt.wantPID, got.PID)
			}
		})
	}
}

// headlessSess builds a headless (claude -p) session with the given pid,
// status, and focus flag.
func headlessSess(pid int, status string, focused bool) state.Session {
	s := sess(pid, status)
	s.Headless = true
	s.Focused = focused
	return s
}

func TestPickAttentionShouldNeverTargetHeadlessSessions(t *testing.T) {
	// The headless session reports "permission", but it has no window to jump
	// to — the jump must land on an interactive session instead (here the
	// all-green fallback), never on the headless one.
	sessions := []state.Session{sess(1, "working"), headlessSess(2, "permission", false)}
	got := pickAttention(sessions, 0)
	if got == nil || got.PID != 1 {
		t.Fatalf("expected pid 1 (headless permission ignored), got %v", got)
	}
}

func TestPickAttentionShouldExcludeHeadlessFromAllGreenDenominator(t *testing.T) {
	// Both interactive sessions are green; the headless one (unknown status)
	// must not suppress the all-green fallback.
	sessions := []state.Session{sess(1, "working"), sess(2, "working"), headlessSess(3, "", false)}
	got := pickAttention(sessions, 0)
	if got == nil || got.PID != 1 {
		t.Fatalf("expected pid 1 from the all-green tier, got %v", got)
	}
}

func TestCycleTargetPID(t *testing.T) {
	focused := func(pid int) state.Session {
		s := sess(pid, "working")
		s.Focused = true
		return s
	}
	tests := []struct {
		name      string
		sessions  []state.Session
		direction string
		wantPID   int
		wantOK    bool
	}{
		{
			name:      "should skip a headless session when cycling next",
			sessions:  []state.Session{focused(1), headlessSess(2, "", false), sess(3, "working")},
			direction: "next",
			wantPID:   3,
			wantOK:    true,
		},
		{
			name:      "should skip a headless session when cycling prev",
			sessions:  []state.Session{sess(1, "working"), headlessSess(2, "", false), focused(3)},
			direction: "prev",
			wantPID:   1,
			wantOK:    true,
		},
		{
			name:      "should report no target when every session is headless",
			sessions:  []state.Session{headlessSess(1, "", false), headlessSess(2, "", false)},
			direction: "next",
			wantOK:    false,
		},
		{
			name:      "should pick the first navigable session when nothing is focused",
			sessions:  []state.Session{headlessSess(1, "", false), sess(2, "working")},
			direction: "next",
			wantPID:   2,
			wantOK:    true,
		},
		{
			name:      "should wrap within the navigable ring",
			sessions:  []state.Session{sess(1, "working"), headlessSess(2, "", false), focused(3)},
			direction: "next",
			wantPID:   1,
			wantOK:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pid, ok := cycleTargetPID(tt.sessions, tt.direction)
			if ok != tt.wantOK {
				t.Fatalf("ok = %t, want %t", ok, tt.wantOK)
			}
			if ok && pid != tt.wantPID {
				t.Fatalf("pid = %d, want %d", pid, tt.wantPID)
			}
		})
	}
}
