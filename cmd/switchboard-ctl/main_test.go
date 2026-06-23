package main

import (
	"testing"

	"github.com/tjmisko/switchboard/internal/state"
)

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
