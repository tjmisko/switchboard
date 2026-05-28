package mapping

import (
	"testing"

	"github.com/tjmisko/switchboard/internal/hyprland"
)

// §3.2 matchUniqueClient — the wezterm<->WM join requires BOTH the mux pid and
// the window title, and returns nil on zero or ambiguous matches.
func TestMatchUniqueClient(t *testing.T) {
	clients := []hyprland.Client{
		{Address: "0xA", PID: 10, Title: "A"},
		{Address: "0xB", PID: 10, Title: "B"},
		{Address: "0xC", PID: 20, Title: "A"},
	}

	if got := matchUniqueClient(clients, 10, "A"); got == nil || got.Address != "0xA" {
		t.Errorf("unique match = %v, want 0xA", got)
	}
	if got := matchUniqueClient(clients, 99, "A"); got != nil {
		t.Errorf("no pid match = %v, want nil", got)
	}
	// Both keys required: pid matches but title doesn't, and vice versa.
	if got := matchUniqueClient(clients, 10, "Z"); got != nil {
		t.Errorf("pid-only match = %v, want nil", got)
	}
	if got := matchUniqueClient(clients, 30, "A"); got != nil {
		t.Errorf("title-only match = %v, want nil", got)
	}

	// Ambiguous: two clients share pid+title → nil (retry next tick).
	ambiguous := []hyprland.Client{
		{Address: "0xA", PID: 10, Title: "A"},
		{Address: "0xB", PID: 10, Title: "A"},
	}
	if got := matchUniqueClient(ambiguous, 10, "A"); got != nil {
		t.Errorf("ambiguous match = %v, want nil", got)
	}
}
