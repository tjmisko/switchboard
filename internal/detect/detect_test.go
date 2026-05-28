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

// Forcing x11 / none selects that backend regardless of environment (these two
// derive their Name without reading env, so the assertion is deterministic).
func TestForceX11AndNone(t *testing.T) {
	if got := detectWM("x11").Name(); got != "x11" {
		t.Errorf("force x11 = %q, want x11", got)
	}
	if got := detectWM("none").Name(); got != "none" {
		t.Errorf("force none = %q, want none", got)
	}
}

// §2.3 auto-detection precedence: a native Wayland compositor or i3's own IPC
// wins over X11/XWayland; X11 is the DISPLAY-only fallback; nothing → none.
func TestWMDetectionPrecedence(t *testing.T) {
	const (
		hypr = "HYPRLAND_INSTANCE_SIGNATURE"
		sway = "SWAYSOCK"
		i3   = "I3SOCK"
		disp = "DISPLAY"
	)
	// set applies env for the case (empty value clears, matching != "" probes).
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"hyprland wins over everything", map[string]string{hypr: "sig", sway: "/s", i3: "/i", disp: ":0"}, "hyprland"},
		{"sway over x11", map[string]string{hypr: "", sway: "/run/sway.sock", i3: "", disp: ":0"}, "sway"},
		{"i3 over x11", map[string]string{hypr: "", sway: "", i3: "/run/i3.sock", disp: ":0"}, "i3"},
		{"x11 fallback", map[string]string{hypr: "", sway: "", i3: "", disp: ":0"}, "x11"},
		{"nothing -> none", map[string]string{hypr: "", sway: "", i3: "", disp: ""}, "none"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, k := range []string{hypr, sway, i3, disp} {
				t.Setenv(k, tt.env[k])
			}
			if got := detectWMAuto().Name(); got != tt.want {
				t.Errorf("auto WM = %q, want %q", got, tt.want)
			}
		})
	}
}
