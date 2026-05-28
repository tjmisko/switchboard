package rpc

import (
	"testing"

	"github.com/tjmisko/switchboard/internal/state"
)

// §5.1 statusFromHookEvent — full mapping table including the ⚠ never-emit
// "unknown" contract.
func TestStatusFromHookEvent(t *testing.T) {
	tests := []struct {
		event string
		want  string
	}{
		{"UserPromptSubmit", "working"},
		{"PostToolUse", "working"},
		{"PermissionRequest", "permission"},
		{"Stop", "idle"},
		{"SessionStart", "idle"},
		{"Bogus", ""},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.event, func(t *testing.T) {
			got := statusFromHookEvent(tt.event)
			if got != tt.want {
				t.Errorf("statusFromHookEvent(%q) = %q, want %q", tt.event, got, tt.want)
			}
			if got == "unknown" {
				t.Errorf("statusFromHookEvent emitted \"unknown\", which must never happen")
			}
		})
	}
}

// §5.2 pickSession — seed cases over a pure slice, including the ⚠ PID-vs-index
// collision (selector "2" resolves to PID 2 when present, else index 2).
func TestPickSession(t *testing.T) {
	sessions := []state.Session{
		{PID: 100},
		{PID: 2, Focused: true},
		{PID: 300},
	}

	if got := pickSession(sessions, "active"); got == nil || got.PID != 2 {
		t.Errorf(`pickSession "active" = %v, want focused PID 2`, got)
	}
	// "2" matches PID 2 (index 1), NOT index 2 (PID 300) — the collision.
	if got := pickSession(sessions, "2"); got == nil || got.PID != 2 {
		t.Errorf(`pickSession "2" = %v, want PID 2 (not index 2)`, got)
	}
	// "0" matches no PID, falls back to index 0.
	if got := pickSession(sessions, "0"); got == nil || got.PID != 100 {
		t.Errorf(`pickSession "0" = %v, want index 0 (PID 100)`, got)
	}
	if got := pickSession(sessions, "nope"); got != nil {
		t.Errorf(`pickSession "nope" = %v, want nil`, got)
	}
	if got := pickSession(sessions, "99"); got != nil {
		t.Errorf(`pickSession "99" = %v, want nil (no PID, out of range)`, got)
	}

	// "active"/"" with none focused falls back to sessions[0].
	noneFocused := []state.Session{{PID: 7}, {PID: 8}}
	if got := pickSession(noneFocused, ""); got == nil || got.PID != 7 {
		t.Errorf(`pickSession "" (none focused) = %v, want PID 7`, got)
	}
}
