// Package terminal is the Seam-2 terminal locator: given a controlling tty, it
// finds the multiplexer pane that owns it and can focus that pane. The tty is
// the portable join key (kernel-controlled, identical across terminals); only
// the tool that resolves it is backend-specific.
//
// Backends: wezterm (existing), none (terminals without IPC → Observe only),
// and tmux in Phase 3. Each adopts the internal/conformance Locator contract
// (RunLocatorContract): an unknown tty resolves to no pane without error or
// hang; an owned tty resolves to a pane with a stable (mux, pane) identity.
package terminal

import (
	"context"
	"errors"
)

// PaneRef is the neutral pane record. It carries the mux-scoped identity the
// WM join and the focus dispatch both need; (Mux, PaneID) is the stable
// identity (PaneID is unique only within its mux).
//
// Backend names the locator that produced the ref so a Chain can route Activate
// back to it. Handle is an opaque, backend-specific focus token (tmux uses it
// for the pane target; wezterm focuses via MuxSocket+PaneID instead).
type PaneRef struct {
	Backend     string // locator that produced this ref ("wezterm", "tmux", …)
	Handle      string // opaque per-backend focus token (e.g. tmux pane id "%3")
	Mux         int    // multiplexer process id owning the pane (0 when N/A)
	MuxSocket   string // control socket of that mux
	PaneID      int    // pane id within the mux's namespace
	TabID       int
	WindowID    int
	Title       string // the pane's OWN title (agent CLIs paint status glyphs here); "" when the backend has no per-pane title
	WindowTitle string // best-effort join key to the WM window title
	TTY         string // the controlling tty this pane owns
	CWD         string // decoded working directory, or "" if unavailable
}

// ErrUnsupported is returned by a backend that cannot focus (the none backend,
// or any terminal without an IPC channel). Navigate degrades to Observe.
var ErrUnsupported = errors.New("terminal: focus unsupported on this backend")

// Locator is the terminal seam.
type Locator interface {
	// Name is the backend identifier reported in the capabilities block.
	Name() string
	// Available reports — cheaply, without side effects — whether this backend
	// is present. Returns false (never panics/hangs) when it is not.
	Available() bool
	// Locate returns the pane attached to tty, or (nil, nil) when none owns it.
	Locate(ctx context.Context, tty string) (*PaneRef, error)
	// Activate focuses the pane. Backends that cannot focus return ErrUnsupported.
	Activate(ctx context.Context, ref *PaneRef) error
}
