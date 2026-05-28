package mapping

import "testing"

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
