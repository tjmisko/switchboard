// Package mapping resolves a claude PID into a fully-decorated Session record
// by combining /proc (cwd, tty), wezterm cli list (pane/window IDs), and
// hyprctl clients (address, workspace).
//
// The match keys are:
//   - claude.tty == wezterm.pane.tty_name   (kernel-controlled, bulletproof)
//   - wezterm.mux_pid == hyprland.client.pid AND
//     wezterm.pane.window_title == hyprland.client.title  (titles agree by
//     construction because wezterm pushes its title to the WM)
package mapping

import (
	"context"
	"net/url"
	"strings"
	"time"

	"github.com/tjmisko/switchboard/internal/hyprland"
	"github.com/tjmisko/switchboard/internal/proc"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/wezterm"
)

// Resolve maps the given claude process to a Session, filling in wezterm and
// hyprland metadata as far as it can. Missing data is left nil — the caller
// can retry on the next reconcile tick.
func Resolve(ctx context.Context, info proc.Info) state.Session {
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

	pane, err := wezterm.FindByTTY(resolveCtx, info.TTY)
	if err != nil || pane == nil {
		return sess
	}
	sess.Wezterm = &state.WeztermInfo{
		MuxPID:      pane.MuxPID,
		MuxSocket:   pane.MuxSocket,
		PaneID:      pane.PaneID,
		TabID:       pane.TabID,
		WindowID:    pane.WindowID,
		WindowTitle: pane.WindowTitle,
	}
	if sess.CWD == "" {
		sess.CWD = decodeCWD(pane.CWDURL)
	}

	client := findHyprClient(resolveCtx, pane.MuxPID, pane.WindowTitle)
	if client != nil {
		sess.Hyprland = &state.HyprlandInfo{
			Address:     client.Address,
			Workspace:   client.Workspace.Name,
			WorkspaceID: client.Workspace.ID,
		}
	}
	return sess
}

// Reconcile re-runs the wezterm + hyprland match for a session whose claude
// process is still alive. Used after Hyprland events (movewindow,
// windowtitlev2) tell us the world changed underneath us.
func Reconcile(ctx context.Context, sess *state.Session) {
	if sess.TTY == "" {
		return
	}
	resolveCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	pane, err := wezterm.FindByTTY(resolveCtx, sess.TTY)
	if err != nil || pane == nil {
		return
	}
	if sess.Wezterm == nil {
		sess.Wezterm = &state.WeztermInfo{}
	}
	sess.Wezterm.MuxPID = pane.MuxPID
	sess.Wezterm.MuxSocket = pane.MuxSocket
	sess.Wezterm.PaneID = pane.PaneID
	sess.Wezterm.TabID = pane.TabID
	sess.Wezterm.WindowID = pane.WindowID
	sess.Wezterm.WindowTitle = pane.WindowTitle

	client := findHyprClient(resolveCtx, pane.MuxPID, pane.WindowTitle)
	if client != nil {
		if sess.Hyprland == nil {
			sess.Hyprland = &state.HyprlandInfo{}
		}
		sess.Hyprland.Address = client.Address
		sess.Hyprland.Workspace = client.Workspace.Name
		sess.Hyprland.WorkspaceID = client.Workspace.ID
	}
}

func findHyprClient(ctx context.Context, muxPID int, windowTitle string) *hyprland.Client {
	clients, err := hyprland.Clients(ctx)
	if err != nil {
		return nil
	}
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
	return nil // ambiguous — let the next reconcile try again
}

func decodeCWD(cwdURL string) string {
	if rest, ok := strings.CutPrefix(cwdURL, "file://"); ok {
		if idx := strings.IndexByte(rest, '/'); idx >= 0 {
			rest = rest[idx:]
		}
		if decoded, err := url.PathUnescape(rest); err == nil {
			return strings.TrimRight(decoded, "/")
		}
	}
	return ""
}
