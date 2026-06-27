// Package detect probes the runtime environment and selects the backend stack
// for the three portability seams. Probing is cheap and side-effect-free
// (environment variables + socket/file existence): it never spawns a process
// or blocks. A wrong guess degrades to a lower tier, never crashes.
//
// One binary picks its backends live; build tags are used only for the OS
// syscall layer (osproc), never for the WM/terminal selection here.
package detect

import (
	"os"

	"github.com/tjmisko/switchboard/internal/osproc"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/terminal"
	"github.com/tjmisko/switchboard/internal/wm"
)

// Stack is the selected backend set. OSProc is always present (it is the
// Observe-tier floor); Terminal and WM degrade to their none backends when no
// supported integration is detected.
type Stack struct {
	OSProc   osproc.Source
	Terminal terminal.Locator
	WM       wm.Manager
}

// Options forces specific backends. The zero value (or "auto") probes the
// environment; "none" forces the Observe-only backend; a named backend forces
// that one regardless of availability (useful for testing degradation).
type Options struct {
	WM       string // "auto" | "hyprland" | "sway" | "i3" | "x11" | "none"
	Terminal string // "auto" | "wezterm" | "tmux" | "none"
}

// Detect selects the backend stack for the current environment.
func Detect(opts Options) Stack {
	return Stack{
		OSProc:   osproc.New(),
		Terminal: detectTerminal(opts.Terminal),
		WM:       detectWM(opts.WM),
	}
}

func detectWM(force string) wm.Manager {
	switch force {
	case "hyprland":
		return wm.NewHyprland()
	case "sway", "i3":
		return wm.NewI3()
	case "x11":
		return wm.NewX11()
	case "none":
		return wm.NewNone()
	}
	return detectWMAuto()
}

// detectWMAuto selects the WM backend by environment precedence:
//
//	Hyprland  ($HYPRLAND_INSTANCE_SIGNATURE, or a live instance on disk)
//	sway      (SWAYSOCK)
//	i3        (I3SOCK)
//	X11/EWMH  (DISPLAY)
//	none
//
// A native Wayland compositor or i3's own IPC wins over generic X11/EWMH: under
// sway or i3, DISPLAY is also set (XWayland), but the richer native IPC — with
// pids (sway) and a precise event stream — is preferred. X11/EWMH is the
// fallback for any other DISPLAY-backed WM.
//
// The Hyprland probe asks the backend itself (Available), which discovers a
// running instance under $XDG_RUNTIME_DIR/hypr even when
// $HYPRLAND_INSTANCE_SIGNATURE is absent — the daemon may be (re)started by a
// fresh user manager that never inherited the compositor's environment.
func detectWMAuto() wm.Manager {
	if h := wm.NewHyprland(); h.Available() {
		return h
	}
	switch {
	case os.Getenv("SWAYSOCK") != "":
		return wm.NewI3()
	case os.Getenv("I3SOCK") != "":
		return wm.NewI3()
	case os.Getenv("DISPLAY") != "":
		return wm.NewX11()
	default:
		return wm.NewNone()
	}
}

func detectTerminal(force string) terminal.Locator {
	switch force {
	case "wezterm":
		return terminal.NewWezterm()
	case "tmux":
		return terminal.NewTmux()
	case "none":
		return terminal.NewNone()
	}
	// auto: a self-redetecting locator that composes whatever backends are live
	// at call time (innermost-first: tmux owns the pane a claude runs in, wezterm
	// is the outer window). Detection must not be one-shot here — the daemon
	// autostarts before the terminal emulator on login, so a frozen
	// terminal="none" would strand every chip on its cwd basename, never
	// reflecting window-title changes, for the whole session.
	return terminal.NewAuto()
}

// Capabilities summarizes the stack for the state.json capabilities block.
// Observe is always true (the floor tier); Navigate requires both a real
// terminal locator AND a real WM focus backend.
func (s Stack) Capabilities() state.Capabilities {
	return state.Capabilities{
		Observe:  true,
		Navigate: s.Terminal.Name() != "none" && s.WM.Name() != "none",
		WM:       s.WM.Name(),
		Terminal: s.Terminal.Name(),
	}
}
