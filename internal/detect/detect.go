// Package detect probes the runtime environment and selects the backend stack
// for the three portability seams. Probing is cheap and side-effect-free
// (environment variables + socket/file existence): it never spawns a process
// or blocks. A wrong guess degrades to a lower tier, never crashes.
//
// One binary picks its backends live; build tags are used only for the OS
// syscall layer (osproc), never for the WM/terminal selection here.
package detect

import (
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
	WM       string // "auto" | "hyprland" | "none"
	Terminal string // "auto" | "wezterm" | "none"
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
	case "none":
		return wm.NewNone()
	}
	if h := wm.NewHyprland(); h.Available() {
		return h
	}
	return wm.NewNone()
}

func detectTerminal(force string) terminal.Locator {
	switch force {
	case "wezterm":
		return terminal.NewWezterm()
	case "none":
		return terminal.NewNone()
	}
	if w := terminal.NewWezterm(); w.Available() {
		return w
	}
	return terminal.NewNone()
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
