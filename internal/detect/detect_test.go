package detect

import "testing"

// Forcing the none backends yields an Observe-only stack: both edges are the
// none backend and Navigate is false while Observe stays true.
func TestForceNoneIsObserveOnly(t *testing.T) {
	s := Detect(Options{WM: "none", Terminal: "none"})
	if got := s.WM.Name(); got != "none" {
		t.Errorf("WM = %q, want none", got)
	}
	if got := s.Terminal.Name(); got != "none" {
		t.Errorf("Terminal = %q, want none", got)
	}
	if s.OSProc == nil {
		t.Error("OSProc must always be present (Observe floor)")
	}
	caps := s.Capabilities()
	if !caps.Observe {
		t.Error("Observe = false, want true (always the floor)")
	}
	if caps.Navigate {
		t.Error("Navigate = true with both edges none, want false")
	}
}

// Forcing the named backends selects them regardless of availability, and a
// fully-resolved stack reports Navigate true.
func TestForceNamedBackendsEnablesNavigate(t *testing.T) {
	s := Detect(Options{WM: "hyprland", Terminal: "wezterm"})
	if got := s.WM.Name(); got != "hyprland" {
		t.Errorf("WM = %q, want hyprland", got)
	}
	if got := s.Terminal.Name(); got != "wezterm" {
		t.Errorf("Terminal = %q, want wezterm", got)
	}
	caps := s.Capabilities()
	if !caps.Navigate {
		t.Error("Navigate = false with both edges resolved, want true")
	}
	if caps.WM != "hyprland" || caps.Terminal != "wezterm" {
		t.Errorf("caps backends = %q/%q, want hyprland/wezterm", caps.WM, caps.Terminal)
	}
}

// A mixed stack (one edge missing) cannot Navigate.
func TestPartialStackCannotNavigate(t *testing.T) {
	s := Detect(Options{WM: "hyprland", Terminal: "none"})
	if s.Capabilities().Navigate {
		t.Error("Navigate = true with terminal=none, want false")
	}
}
