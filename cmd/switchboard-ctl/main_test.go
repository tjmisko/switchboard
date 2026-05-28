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
			name:     "should return nil when all sessions are working or unknown",
			sessions: []state.Session{sess(1, "working"), sess(2, "unknown"), sess(3, "")},
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pickAttention(tt.sessions)
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
