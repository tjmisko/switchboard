// Package wm is the Seam-3 window manager: list client windows, report/Set the
// active window, and stream window-lifecycle events. The window Address is an
// opaque, backend-owned ref (Hyprland 0x…, sway con_id, X11 window id) that
// shared code never parses.
//
// Backends: hyprland (existing), none (Observe only), and sway/i3 + X11/EWMH in
// Phase 2. Each adopts the internal/conformance Manager contract
// (RunManagerContract): event-address normalization round-trips, the recognized
// event vocabulary is reported, and Available() reports false cleanly when the
// WM is not running.
//
// The seam owns address normalization (decisions.md #1, the single most fragile
// cross-layer contract): each backend converts its event-stream address form
// into the Clients() form internally, so consumers receive already-comparable
// refs and never reconstruct "0x"+data at the event boundary.
package wm

import (
	"context"
	"errors"
)

// Window is the neutral client record.
type Window struct {
	Address     string // opaque, backend-owned window ref
	PID         int
	Title       string
	Workspace   string
	WorkspaceID int
}

// EventKind is the neutral window-event vocabulary the daemon reacts to. Each
// backend translates its raw event stream into these kinds.
type EventKind string

const (
	// EventFocusChanged: the active window changed; Address is the new active
	// window (Clients form), "" if nothing is focused.
	EventFocusChanged EventKind = "focus-changed"
	// EventWindowClosed: a window closed; Address is the closed window.
	EventWindowClosed EventKind = "window-closed"
	// EventLayoutChanged: a window moved / retitled / opened — something that may
	// change a session's mapping. Address is unset; the daemon re-reconciles.
	EventLayoutChanged EventKind = "layout-changed"
)

// Event is a neutralized window event. Address is in Clients() form (already
// normalized by the backend), or "" for EventLayoutChanged.
type Event struct {
	Kind    EventKind
	Address string
}

// ErrUnsupported is returned by a backend that cannot focus (the none backend).
// Navigate degrades to Observe.
var ErrUnsupported = errors.New("wm: focus unsupported on this backend")

// Manager is the window-manager seam.
type Manager interface {
	// Name is the backend identifier reported in the capabilities block.
	Name() string
	// Available reports — cheaply, without side effects — whether this backend's
	// WM is running. Returns false (never panics/hangs) when it is not.
	Available() bool
	// Clients returns every client window the WM knows about.
	Clients(ctx context.Context) ([]Window, error)
	// ActiveWindow returns the focused window's address, or "" if none.
	ActiveWindow(ctx context.Context) (string, error)
	// Focus moves focus to ref. Backends that cannot focus return ErrUnsupported.
	Focus(ctx context.Context, ref string) error
	// Subscribe streams neutral events until ctx is cancelled or the backend
	// connection ends, then closes the channel.
	Subscribe(ctx context.Context) (<-chan Event, error)
}
