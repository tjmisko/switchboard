package mapping

import (
	"testing"

	"github.com/tjmisko/switchboard/internal/hyprland"
)

// §3.1 decodeCWD — seed table including the ⚠ file://host characterization
// (0.3 owns the full coverage). findHyprClient/Resolve/Reconcile get their
// harness consumers in §0.5 once matchUniqueClient is split out.
func TestDecodeCWD(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{"no file scheme", "/plain/path", ""},
		{"host stripped", "file://host/home/u/proj", "/home/u/proj"},
		{"empty host", "file:///home/u/proj", "/home/u/proj"},
		{"percent decode", "file:///home/u/my%20proj", "/home/u/my proj"},
		{"trailing slash trimmed", "file:///home/u/proj/", "/home/u/proj"},
		{"bad escape", "file:///home/%zz", ""},
		// ⚠ characterization: a host with no path leaks through as the path.
		{"host no path returns host", "file://justhost", "justhost"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decodeCWD(tt.url); got != tt.want {
				t.Errorf("decodeCWD(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}

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
