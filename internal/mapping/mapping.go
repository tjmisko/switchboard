// Package mapping resolves a claude PID into a fully-decorated Session record
// by combining /proc (cwd, tty), the terminal locator (pane/window IDs), and
// the window manager (address, workspace).
//
// The match keys are:
//   - claude.tty == terminal pane tty   (kernel-controlled, bulletproof)
//   - pane.mux == wm.client.pid AND pane.window_title == wm.client.title
//     (titles agree by construction because the terminal pushes its title to
//     the WM)
package mapping

import (
	"context"
	"time"

	"github.com/tjmisko/switchboard/internal/hyprland"
	"github.com/tjmisko/switchboard/internal/proc"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/terminal"
)

// Resolver decorates sessions using injected seam backends. The terminal
// locator is the Phase-1.2 seam; the WM join still calls Hyprland directly and
// moves behind the wm.Manager seam in Phase 1.3.
type Resolver struct {
	term terminal.Locator
}

// NewResolver builds a Resolver over the given terminal locator.
func NewResolver(term terminal.Locator) *Resolver {
	return &Resolver{term: term}
}

// Resolve maps the given claude process to a Session, filling in terminal and
// WM metadata as far as it can. Missing data is left nil — the caller can retry
// on the next reconcile tick.
func (r *Resolver) Resolve(ctx context.Context, info proc.Info) state.Session {
	sess := state.Session{
		PID:       info.PID,
		CWD:       info.CWD,
		TTY:       info.TTY,
		StartedAt: time.Now(),
	}
	if info.TTY == "" {
		return sess
	}

	resolveCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	pane, err := r.term.Locate(resolveCtx, info.TTY)
	if err != nil || pane == nil {
		return sess
	}
	sess.Wezterm = weztermInfo(pane)
	if sess.CWD == "" {
		sess.CWD = pane.CWD
	}

	client := findHyprClient(resolveCtx, pane.Mux, pane.WindowTitle)
	if client != nil {
		sess.Hyprland = &state.HyprlandInfo{
			Address:     client.Address,
			Workspace:   client.Workspace.Name,
			WorkspaceID: client.Workspace.ID,
		}
	}
	return sess
}

// Reconcile re-runs the terminal + WM match for a session whose claude process
// is still alive. Used after WM events (movewindow, windowtitle) tell us the
// world changed underneath us.
func (r *Resolver) Reconcile(ctx context.Context, sess *state.Session) {
	if sess.TTY == "" {
		return
	}
	resolveCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	pane, err := r.term.Locate(resolveCtx, sess.TTY)
	if err != nil || pane == nil {
		return
	}
	sess.Wezterm = weztermInfo(pane)

	client := findHyprClient(resolveCtx, pane.Mux, pane.WindowTitle)
	if client != nil {
		if sess.Hyprland == nil {
			sess.Hyprland = &state.HyprlandInfo{}
		}
		sess.Hyprland.Address = client.Address
		sess.Hyprland.Workspace = client.Workspace.Name
		sess.Hyprland.WorkspaceID = client.Workspace.ID
	}
}

func weztermInfo(pane *terminal.PaneRef) *state.WeztermInfo {
	return &state.WeztermInfo{
		MuxPID:      pane.Mux,
		MuxSocket:   pane.MuxSocket,
		PaneID:      pane.PaneID,
		TabID:       pane.TabID,
		WindowID:    pane.WindowID,
		WindowTitle: pane.WindowTitle,
	}
}

func findHyprClient(ctx context.Context, muxPID int, windowTitle string) *hyprland.Client {
	clients, err := hyprland.Clients(ctx)
	if err != nil {
		return nil
	}
	return matchUniqueClient(clients, muxPID, windowTitle)
}

// matchUniqueClient returns the single client matching BOTH the mux pid and the
// window title, or nil if zero or more than one match. An ambiguous match
// returns nil rather than guessing — the next reconcile tick retries (the
// "retry next tick" contract, decisions.md #4). Pure, so the join logic is
// testable without a live Hyprland.
func matchUniqueClient(clients []hyprland.Client, muxPID int, windowTitle string) *hyprland.Client {
	var matches []*hyprland.Client
	for i := range clients {
		c := &clients[i]
		if c.PID != muxPID {
			continue
		}
		if c.Title != windowTitle {
			continue
		}
		matches = append(matches, c)
	}
	if len(matches) == 1 {
		return matches[0]
	}
	return nil // zero or ambiguous — let the next reconcile try again
}
