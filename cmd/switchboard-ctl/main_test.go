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
	// The unbound remote row is not a ring member, so the ring is the lone
	// focused local session and the press has nowhere to go.
	if got := nextAttentionTarget([]state.Session{local, remote}); got != nil {
		t.Fatalf("attention target = %+v, want nil", got)
	}

	// With a second bound local session the aggregate path still moves, and
	// still never onto the unbound remote row.
	second := sess(3, "permission")
	second.Hostname, second.Navigable = "local", true
	got := nextAttentionTarget([]state.Session{local, remote, second})
	if got == nil || got.PID != 3 {
		t.Fatalf("attention target = %+v, want pid 3", got)
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

// focusedSess builds a session with the given pid and status that reports the
// focused window, so the table can express "the key was pressed here".
func focusedSess(pid int, status string) state.Session {
	s := sess(pid, status)
	s.Focused = true
	return s
}

func TestNextAttentionTarget(t *testing.T) {
	tests := []struct {
		name     string
		sessions []state.Session
		wantPID  int // 0 means expect nil
	}{
		{
			name:     "should return nil when there are no sessions",
			sessions: nil,
			wantPID:  0,
		},
		{
			name:     "should return nil when the ring is empty",
			sessions: []state.Session{sess(1, "unknown"), sess(2, "")},
			wantPID:  0,
		},
		{
			name:     "should return nil when the only navigable session is focused",
			sessions: []state.Session{focusedSess(1, "permission")},
			wantPID:  0,
		},
		{
			name:     "should jump to the first permission session when several are waiting",
			sessions: []state.Session{sess(1, "idle"), sess(2, "permission"), sess(3, "permission")},
			wantPID:  2,
		},
		{
			name:     "should prefer permission over idle even when idle comes first",
			sessions: []state.Session{sess(1, "idle"), sess(2, "permission")},
			wantPID:  2,
		},
		{
			name:     "should jump to the only idle session when no permission exists",
			sessions: []state.Session{sess(1, "working"), sess(2, "idle"), sess(3, "working")},
			wantPID:  2,
		},
		{
			name:     "should pick the first idle session when several are idle and none need permission",
			sessions: []state.Session{sess(1, "working"), sess(2, "idle"), sess(3, "idle")},
			wantPID:  2,
		},
		{
			name:     "should cycle to the next permission session when focused on one",
			sessions: []state.Session{focusedSess(1, "permission"), sess(2, "permission"), sess(3, "idle")},
			wantPID:  2,
		},
		{
			name:     "should cycle to the next idle session when focused on one",
			sessions: []state.Session{sess(1, "idle"), focusedSess(2, "idle"), sess(3, "idle"), sess(4, "working")},
			wantPID:  3,
		},
		{
			name:     "should jump to the first ring member when the focused session is not in the ring",
			sessions: []state.Session{sess(1, "idle"), sess(2, "idle"), focusedSess(3, "")},
			wantPID:  1,
		},
		{
			// C1: a singleton red level used to re-focus itself — the dead key.
			name:     "should pop out to the idle tier when the focused red is the only red",
			sessions: []state.Session{focusedSess(1, "permission"), sess(2, "idle"), sess(3, "working")},
			wantPID:  2,
		},
		{
			// C2: the last member of a tier pops out instead of wrapping inside it.
			name:     "should pop out to the idle tier from the last red of several",
			sessions: []state.Session{sess(1, "permission"), focusedSess(2, "permission"), sess(3, "idle")},
			wantPID:  3,
		},
		{
			name:     "should wrap from the last green back to the first red",
			sessions: []state.Session{sess(1, "permission"), sess(2, "idle"), focusedSess(3, "working")},
			wantPID:  1,
		},
		{
			name:     "should wrap from the last idle to the first red when no green exists",
			sessions: []state.Session{sess(1, "permission"), sess(2, "idle"), focusedSess(3, "idle")},
			wantPID:  1,
		},
		{
			// C3: grey is excluded from the ring and suppresses nothing.
			name:     "should cycle green when an unknown session is present",
			sessions: []state.Session{focusedSess(1, "working"), sess(2, "working"), sess(3, "unknown")},
			wantPID:  2,
		},
		{
			// C4: delegating is a first-class green member, not a phantom that
			// inflates a denominator and strands the key.
			name:     "should treat a delegating session as a green ring member",
			sessions: []state.Session{focusedSess(1, "working"), sess(2, "delegating")},
			wantPID:  2,
		},
		{
			name:     "should pop out of the delegating tier into the wrap",
			sessions: []state.Session{sess(1, "working"), focusedSess(2, "delegating"), sess(3, "permission")},
			wantPID:  3,
		},
		{
			// C5: the accepted trade — a green with a red elsewhere advances
			// within green and reaches the red at the end of the lap.
			name:     "should advance within green rather than jumping to a red",
			sessions: []state.Session{sess(1, "permission"), focusedSess(2, "working"), sess(3, "working")},
			wantPID:  3,
		},
		{
			// Focused is a window flag, so two panes of one wezterm window both
			// report it; skipping the whole focused set lands on a real move.
			name:     "should skip a sibling pane sharing the focused window",
			sessions: []state.Session{focusedSess(1, "working"), focusedSess(2, "working"), sess(3, "idle")},
			wantPID:  3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nextAttentionTarget(tt.sessions)
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
			if got.Focused {
				t.Fatalf("target pid %d is already focused", got.PID)
			}
		})
	}
}

func TestAttentionRingOrdersPermissionThenIdleThenGreenInSnapshotOrder(t *testing.T) {
	sessions := []state.Session{
		sess(1, "working"),
		sess(2, "permission"),
		sess(3, "unknown"),
		sess(4, "idle"),
		sess(5, "delegating"),
		sess(6, "permission"),
		sess(7, "idle"),
		headlessSess(8, "permission", false),
	}
	var got []int
	for _, session := range attentionRing(sessions) {
		got = append(got, session.PID)
	}
	want := []int{2, 6, 4, 7, 1, 5}
	if len(got) != len(want) {
		t.Fatalf("ring = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ring = %v, want %v", got, want)
		}
	}
}

func TestNextAttentionTargetVisitsEveryRingMemberOncePerLap(t *testing.T) {
	sessions := []state.Session{
		focusedSess(1, "permission"),
		sess(2, "idle"),
		sess(3, "working"),
		sess(4, "delegating"),
		sess(5, "unknown"),
	}
	ring := attentionRing(sessions)
	visited := []int{1}
	for press := 0; press < len(ring); press++ {
		target := nextAttentionTarget(sessions)
		if target == nil {
			t.Fatalf("press %d returned nil after visiting %v", press+1, visited)
		}
		if target.Focused {
			t.Fatalf("press %d returned the focused session pid %d", press+1, target.PID)
		}
		for i := range sessions {
			sessions[i].Focused = false
		}
		target.Focused = true
		visited = append(visited, target.PID)
	}
	// One lap of N presses from ring[0] visits every member and returns home.
	want := []int{1, 2, 3, 4, 1}
	if len(visited) != len(want) {
		t.Fatalf("visited = %v, want %v", visited, want)
	}
	for i := range want {
		if visited[i] != want[i] {
			t.Fatalf("visited = %v, want %v", visited, want)
		}
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

func TestNextAttentionTargetShouldNeverTargetHeadlessSessions(t *testing.T) {
	// The headless session reports "permission", but it has no window to jump
	// to — the jump must land on an interactive session instead, never on the
	// headless one.
	sessions := []state.Session{sess(1, "working"), headlessSess(2, "permission", false)}
	got := nextAttentionTarget(sessions)
	if got == nil || got.PID != 1 {
		t.Fatalf("expected pid 1 (headless permission ignored), got %v", got)
	}
}

func TestNextAttentionTargetShouldExcludeHeadlessFromTheRing(t *testing.T) {
	// A headless row must not join the ring, so the lap runs over the two
	// interactive sessions only.
	sessions := []state.Session{focusedSess(1, "working"), sess(2, "working"), headlessSess(3, "", false)}
	got := nextAttentionTarget(sessions)
	if got == nil || got.PID != 2 {
		t.Fatalf("expected pid 2 from the green ring, got %v", got)
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
