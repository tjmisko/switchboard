// Package mapping resolves a claude PID into a fully-decorated Session record
// by combining the OS process snapshot (cwd, tty), the terminal locator
// (pane/window IDs), and the window manager (address, workspace).
//
// The match keys are:
//   - claude.tty == terminal pane tty (kernel-controlled, bulletproof)
//   - pane mux/window IDs == the stable marker on the matching WM client; or,
//     without the optional marker integration, pane.mux == wm.client.pid AND
//     pane.window_title == wm.client.title
package mapping

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tjmisko/switchboard/internal/osproc"
	"github.com/tjmisko/switchboard/internal/panebind"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/terminal"
	"github.com/tjmisko/switchboard/internal/wm"
)

// Resolver decorates sessions using injected seam backends: the terminal
// locator (Phase 1.2) and the window manager (Phase 1.3).
type Resolver struct {
	term    terminal.Locator
	wm      wm.Manager
	windows func([]wm.Window)
}

// NewResolver builds a Resolver over the given terminal locator and WM manager.
func NewResolver(term terminal.Locator, manager wm.Manager) *Resolver {
	return &Resolver{term: term, wm: manager}
}

// SetWindowObserver installs a callback handed each WM client enumeration this
// resolver fetches for a tick (Enumerate), including the nil one that means the
// query failed.
//
// It exists so a second consumer of the window list can read the enumeration
// this resolver ALREADY pays for. Federation needs one: to order a remote chip
// it must know which local workspace holds the window displaying that session,
// and it asks on the publish path, where forking its own hyprctl would
// reintroduce precisely the per-lookup enumeration the batched reconcile
// removed.
//
// Set it once at startup, before the reconcile and WM loops begin.
func (r *Resolver) SetWindowObserver(observe func([]wm.Window)) {
	r.windows = observe
}

// Resolve maps the given claude process to a Session, filling in terminal and
// WM metadata as far as it can. Missing data is left nil — the caller can retry
// on the next reconcile tick.
func (r *Resolver) Resolve(ctx context.Context, info osproc.Info) state.Session {
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
	if pane.Backend == "wezterm" {
		sess.Wezterm = weztermInfo(pane, time.Now())
	}
	if sess.CWD == "" {
		sess.CWD = pane.CWD
	}

	// The WM join keys on the terminal's mux pid; backends that don't expose one
	// (e.g. tmux, whose pane pid is the in-pane process) leave Mux 0 and the
	// session stays Observe-only on the WM axis.
	if pane.Mux != 0 {
		if win := r.findWindow(resolveCtx, pane.Mux, pane.WindowID, pane.WindowTitle); win != nil {
			sess.Hyprland = &state.HyprlandInfo{
				Address:     win.Address,
				Workspace:   win.Workspace,
				WorkspaceID: win.WorkspaceID,
			}
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
	if pane.Backend == "wezterm" {
		sess.Wezterm = weztermInfo(pane, time.Now())
	}

	if pane.Mux != 0 {
		if win := r.findWindow(resolveCtx, pane.Mux, pane.WindowID, pane.WindowTitle); win != nil {
			if sess.Hyprland == nil {
				sess.Hyprland = &state.HyprlandInfo{}
			}
			sess.Hyprland.Address = win.Address
			sess.Hyprland.Workspace = win.Workspace
			sess.Hyprland.WorkspaceID = win.WorkspaceID
		}
	}
}

// Enumerate fetches the terminal and WM enumerations ReconcileFrom needs — one
// call each, for a whole reconcile tick.
//
// It hangs on the Resolver rather than letting the caller assemble the two
// arguments itself, and that is load-bearing: it reads through the SAME seams
// Reconcile would have used. A caller that snapshotted a different Locator than
// the one backing the fallback path would resolve sessions against panes that
// path could never produce, and the two would disagree exactly when the batch
// path degraded — the worst possible time.
//
// panes is nil when the terminal backend offers no batch path OR its enumeration
// failed; both mean "no usable batch answer" and the caller falls back to
// per-session Reconcile (see terminal.SnapshotOrNil). Note that nil is NOT the
// same as an empty map, which is a real answer meaning no tty owns a pane.
// clients is nil on a WM error, matching findWindow, which swallows that error
// and returns no window.
//
// Carries Reconcile's 3s timeout so a wedged mux or compositor bounds the tick
// rather than stalling it.
func (r *Resolver) Enumerate(ctx context.Context) (map[string]terminal.PaneRef, []wm.Window) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	panes := terminal.SnapshotOrNil(ctx, r.term)
	clients, err := r.wm.Clients(ctx)
	if err != nil {
		clients = nil
	}
	if r.windows != nil {
		r.windows(clients)
	}
	return panes, clients
}

// ReconcileFrom is Reconcile with the two I/O calls hoisted out: it applies an
// already-fetched terminal + WM enumeration to one session and performs NO I/O
// of its own. Reconcile forks a terminal enumeration and a full WM client query
// PER SESSION, so a reconcile tick over N sessions paid N of each — identical
// data, fetched N times, with the store's write lock held the whole way. The
// batched call site fetches once and fans the same two snapshots across every
// session, which is why this takes plain values and no context: with nothing to
// wait on there is nothing to time out.
//
// panes is keyed by controlling tty — the portable join key (see the package
// doc); clients is one wm.Clients() result. Both are read-only here, so one
// snapshot pair is safe to hand to every session in the tick.
//
// now is the tick's clock, threaded in rather than sampled per session so a
// whole tick stamps one TitleAt. That keeps the H9 idle-title freshness gate
// (state.WeztermInfo.TitleAt) comparing sessions on equal footing, and leaves
// this function pure and testable.
//
// Behavior is observationally identical to Reconcile, including the parts that
// look like omissions:
//   - A tty absent from panes behaves exactly as Locate returning (nil, nil):
//     return without touching the session. A miss NEVER clears an already
//     resolved mapping — a terminal that is briefly unenumerable must not blank
//     a live chip; the next tick re-resolves it.
//   - Ambiguity is still resolved by matchUniqueClient, i.e. not at all: zero or
//     multiple (pid, title) matches leave the prior address in place rather than
//     guess (the "retry next tick" contract, decisions.md #4).
//   - No fallback to pane.CWD for an empty sess.CWD. Only the initial Resolve
//     does that; reconcile deliberately does not, and this mirrors reconcile.
//
// A WM query that failed at fetch time should arrive here as a nil clients
// slice, which matches findWindow swallowing the error and returning nil.
//
// The receiver is deliberately unnamed: the seams live on the Resolver, so
// having no handle on them makes "does no I/O" a compile-time property rather
// than a promise a later edit can quietly break. It stays a method so the
// batched call site reaches it through the same Resolver as Resolve/Reconcile.
func (*Resolver) ReconcileFrom(sess *state.Session, panes map[string]terminal.PaneRef, clients []wm.Window, now time.Time) {
	if sess.TTY == "" {
		return
	}
	pane, ok := panes[sess.TTY]
	if !ok {
		return // no pane owns this tty this tick — same as Locate finding nothing
	}
	if pane.Backend == "wezterm" {
		sess.Wezterm = weztermInfo(&pane, now)
	}

	// The WM join keys on the terminal's mux pid; backends that don't expose one
	// (e.g. tmux, whose pane pid is the in-pane process) leave Mux 0 and the
	// session stays Observe-only on the WM axis.
	if pane.Mux != 0 {
		if win := matchUniqueClient(clients, pane.Mux, pane.WindowID, pane.WindowTitle); win != nil {
			if sess.Hyprland == nil {
				sess.Hyprland = &state.HyprlandInfo{}
			}
			sess.Hyprland.Address = win.Address
			sess.Hyprland.Workspace = win.Workspace
			sess.Hyprland.WorkspaceID = win.WorkspaceID
		}
	}
}

// weztermInfo snapshots a pane into the session's wezterm block. now is passed
// in rather than sampled here so a batched tick shares one TitleAt across every
// session it stamps (see ReconcileFrom).
func weztermInfo(pane *terminal.PaneRef, now time.Time) *state.WeztermInfo {
	return &state.WeztermInfo{
		MuxPID:      pane.Mux,
		MuxSocket:   pane.MuxSocket,
		PaneID:      pane.PaneID,
		TabID:       pane.TabID,
		WindowID:    pane.WindowID,
		WindowTitle: pane.WindowTitle,
		Title:       pane.Title,
		TitleAt:     now,
	}
}

func (r *Resolver) findWindow(ctx context.Context, muxPID, windowID int, windowTitle string) *wm.Window {
	clients, err := r.wm.Clients(ctx)
	if err != nil {
		return nil
	}
	return matchUniqueClient(clients, muxPID, windowID, windowTitle)
}

// matchUniqueClient returns the one OS window owned by the terminal pane.
//
// The Switchboard WezTerm integration appends [sbw:<gui-pid>:<window-id>] to
// the compositor-visible title. `wezterm cli list`, however, reports the base
// window_title without that formatter suffix. Prefer the marker: it is a stable
// exact join even when two windows share the same user-facing title. Fall back
// to the legacy mux-pid + title join for terminals which have not installed the
// integration. Either path fails closed on ambiguity and retries next tick
// rather than guessing (decisions.md #4).
func matchUniqueClient(clients []wm.Window, muxPID, windowID int, windowTitle string) *wm.Window {
	marker := panebind.WindowMarker(panebind.LocalPaneRef{GUIPID: muxPID, WindowID: windowID})
	marked := uniqueClient(clients, func(c wm.Window) bool {
		return c.PID == muxPID && strings.HasSuffix(strings.TrimSpace(c.Title), marker)
	})
	if marked.count > 0 {
		return marked.one // nil when a duplicate marker makes the exact join ambiguous
	}

	wantTitle := normalizeTitle(windowTitle)
	legacy := uniqueClient(clients, func(c wm.Window) bool {
		return c.PID == muxPID && normalizeTitle(c.Title) == wantTitle
	})
	return legacy.one
}

type clientMatch struct {
	one   *wm.Window
	count int
}

func uniqueClient(clients []wm.Window, matches func(wm.Window) bool) clientMatch {
	var result clientMatch
	for i := range clients {
		if !matches(clients[i]) {
			continue
		}
		result.count++
		if result.count == 1 {
			result.one = &clients[i]
		} else {
			result.one = nil
		}
	}
	return result
}

// spinnerPrefixes contains the leading activity glyphs coding agents place in
// pane/window titles. The terminal and WM seams sample independently, so a
// spinner can advance between the two reads; it is presentation, not identity.
const spinnerPrefixes = "◐◑◒◓⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏✳⠂⠐⠁⠈⠠⠄⡀⢀"

func normalizeTitle(title string) string {
	title = strings.TrimSpace(title)
	if title == "" {
		return ""
	}
	r, size := utf8.DecodeRuneInString(title)
	if strings.ContainsRune(spinnerPrefixes, r) {
		title = title[size:]
	}
	return strings.TrimSpace(title)
}
