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

// Snapshotter is the optional batch fast-path a Locator may provide: resolve
// many ttys against ONE enumeration. It is deliberately NOT part of the neutral
// Locator contract — the reconciler upgrades to it when present and degrades to
// per-session Locate when absent, so a new backend drops in either way.
//
// It exists because Locate is per-tty while every multiplexer backend answers it
// by enumerating the whole mux. With N sessions across M muxes the reconciler
// forks N×M `wezterm cli list` calls per tick, each returning the identical pane
// list, and it pays that while holding the store's write lock — the dominant
// source of both daemon CPU and RPC stalls. One Snapshot serves every session.
type Snapshotter interface {
	// Snapshot returns every pane the backend owns, keyed by the pane's own
	// controlling tty (PaneRef.TTY) — the join key the whole mapping layer is
	// anchored on, and the same key Locate matches. Panes with no tty are
	// omitted: nothing can ever join to them.
	//
	// A key's absence must mean "no pane owns this tty", so implementations
	// return a non-nil (possibly empty) map on success and an error whenever the
	// set would be incomplete. Owning nothing is success, not failure.
	Snapshot(ctx context.Context) (map[string]PaneRef, error)
}

// SnapshotOrNil returns loc's batch pane set, or nil when loc does not implement
// Snapshotter (the caller then falls back to per-session Locate). A backend that
// implements it but fails also yields nil, so a transient enumeration error
// degrades to the single path rather than blanking every session's pane — an
// empty map would read as "no tty owns a pane" and wipe every chip's title.
//
// nil is therefore unambiguous ("no usable batch answer"); a non-nil result is
// complete and its missing keys are real misses.
func SnapshotOrNil(ctx context.Context, loc Locator) map[string]PaneRef {
	s, ok := loc.(Snapshotter)
	if !ok {
		return nil
	}
	panes, err := s.Snapshot(ctx)
	if err != nil {
		return nil
	}
	return panes
}
