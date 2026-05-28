package terminal

import "testing"

// §3.1 decodeCWD. This table moved here from internal/mapping when the terminal
// seam took ownership of cwd decoding (Phase 1.2). The host-no-path case is the
// Phase-0 ⚠ characterization (decisions.md #8), now FLIPPED to the intended
// behavior: a bare "file://host" with no path returns "" instead of leaking the
// host as if it were a path.
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
		// #8 fix: a host with no path no longer leaks through as the path.
		{"host no path returns empty", "file://justhost", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decodeCWD(tt.url); got != tt.want {
				t.Errorf("decodeCWD(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}

// The none backend resolves no tty and reports focus unsupported, never erroring
// on Locate so the daemon stays in the Observe tier.
func TestNoneLocator(t *testing.T) {
	l := NewNone()
	if l.Available() {
		t.Error("none.Available() = true, want false")
	}
	pane, err := l.Locate(t.Context(), "/dev/pts/3")
	if err != nil || pane != nil {
		t.Errorf("none.Locate = (%v, %v), want (nil, nil)", pane, err)
	}
	if err := l.Activate(t.Context(), &PaneRef{}); err != ErrUnsupported {
		t.Errorf("none.Activate err = %v, want ErrUnsupported", err)
	}
}
