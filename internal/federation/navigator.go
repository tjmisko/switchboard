package federation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tjmisko/switchboard/internal/panebind"
	"github.com/tjmisko/switchboard/internal/wezterm"
	"github.com/tjmisko/switchboard/internal/wm"
)

// LocalFocus is the existing host-local focus path with an exact PID-lifetime
// guard. rpc.Server.FocusLocalSession satisfies it.
type LocalFocus func(context.Context, int, time.Time) error

// ErrNavigateUnavailable is returned when the local desktop has no window
// manager actuator. Remote pane activation alone is intentionally insufficient:
// the contract is to activate the pane and raise its exact OS window.
var ErrNavigateUnavailable = errors.New("federation: navigation is unavailable")

// Navigator owns only the action-time correlation edges. It holds no durable
// route or remote state beyond Registry and View, both in-memory replacements.
type Navigator struct {
	LocalHostname string
	View          *View
	Registry      *panebind.Registry
	Routes        panebind.RouteResolver
	WM            wm.Manager
	FocusLocal    LocalFocus
	ActivatePane  func(context.Context, string, int) error
}

func (n *Navigator) activate() func(context.Context, string, int) error {
	if n.ActivatePane != nil {
		return n.ActivatePane
	}
	return wezterm.ActivatePane
}

func definitiveLocalRouteMiss(err error) bool {
	return errors.Is(err, panebind.ErrPaneNotFound) ||
		errors.Is(err, wezterm.ErrGUISocketPeer)
}

// pruneDefinitiveMiss removes only the exact candidate which produced a
// definitive local pane absence. UnbindRoute's compare-and-delete preserves a
// pane move or rebind that raced the failed lookup. Missing window markers,
// transient command errors, ambiguity, timeouts, and route changes deliberately
// remain retryable because title/WM observations can lag a fresh binding.
func (n *Navigator) pruneDefinitiveMiss(key panebind.ExactSessionKey, ref panebind.LocalPaneRef, err error) {
	if !definitiveLocalRouteMiss(err) || n.Registry == nil || !n.Registry.UnbindRoute(key, ref) {
		return
	}
	if n.View != nil {
		n.View.ClearRemoteFocusKey(key.Hostname, key.PID, key.StartedAt)
		n.View.Refresh()
	}
}

// RouteReady is a cheap in-memory/backend-availability hint for navigation
// controls. It never authorizes an action; Focus still repeats every local
// pane/WM check.
func (n *Navigator) RouteReady(host string, pid int, startedAt time.Time) bool {
	if host == n.LocalHostname {
		return true
	}
	if n.Registry == nil || n.WM == nil || n.WM.Name() == "none" || !n.WM.Available() {
		return false
	}
	_, err := n.Registry.Resolve(panebind.ExactSessionKey{Hostname: host, PID: pid, StartedAt: startedAt})
	return err == nil
}

// Focus revalidates the observed exact row, resolves one unique local pane,
// activates it, then resolves the possibly changed window again before raising
// that exact WM client.
func (n *Navigator) Focus(ctx context.Context, host string, pid int, startedAt time.Time) error {
	key := panebind.ExactSessionKey{Hostname: host, PID: pid, StartedAt: startedAt}
	if err := key.Validate(); err != nil {
		return err
	}
	if n.View == nil {
		return errors.New("federation: aggregate view is not configured")
	}
	var live, headless bool
	for _, session := range n.View.Snapshot().Sessions {
		if session.Hostname == host && session.PID == pid && session.StartedAt.Equal(startedAt) {
			live = true
			headless = session.Headless
			break
		}
	}
	if !live {
		return fmt.Errorf("session %s/%d is no longer live", host, pid)
	}
	if headless {
		return errors.New("headless session has no pane to focus")
	}
	if host == n.LocalHostname {
		if n.FocusLocal == nil {
			return errors.New("federation: local focus is not configured")
		}
		return n.FocusLocal(ctx, pid, startedAt)
	}
	if n.WM == nil || n.WM.Name() == "none" || !n.WM.Available() {
		return ErrNavigateUnavailable
	}
	if n.Registry == nil {
		return errors.New("federation: remote navigation is not configured")
	}
	candidate, err := n.Registry.Resolve(key)
	if err != nil {
		return fmt.Errorf("resolve remote binding: %w", err)
	}
	route, err := n.Routes.Resolve(ctx, key)
	if err != nil {
		n.pruneDefinitiveMiss(key, candidate, err)
		return fmt.Errorf("resolve remote pane: %w", err)
	}
	if err := n.activate()(ctx, route.Pane.MuxSocket, route.Pane.PaneID); err != nil {
		return fmt.Errorf("activate remote pane: %w", err)
	}
	prior := route
	route, err = n.Routes.RevalidateAfterActivation(ctx, key, prior)
	if err != nil {
		n.pruneDefinitiveMiss(key, prior.Ref, err)
		return fmt.Errorf("revalidate remote window: %w", err)
	}
	if err := n.WM.Focus(ctx, route.Window.Address); err != nil {
		return fmt.Errorf("focus remote window: %w", err)
	}
	return nil
}

// BindPane accepts a strictly decoded OSC payload and records it only as a
// candidate. Registry liveness remains the authorization gate.
func (n *Navigator) BindPane(_ context.Context, payload string, guiPID, windowID, paneID int) error {
	if n.Registry == nil {
		return errors.New("federation: pane registry is not configured")
	}
	key, err := panebind.Decode(payload)
	if err != nil {
		return err
	}
	ref := panebind.LocalPaneRef{GUIPID: guiPID, WindowID: windowID, PaneID: paneID}
	if err := ref.Validate(); err != nil {
		return err
	}
	// Every daemon may export its sessions, so a locally displayed session can
	// receive its own announcement even though only remote routes need this
	// registry. Acknowledge that valid local signal without retaining a forever-
	// non-live candidate or making WezTerm retry it on every status tick.
	if key.Hostname == n.LocalHostname {
		oldKey, oldBound, _ := n.Registry.SessionForPane(ref)
		if oldBound {
			n.Registry.UnbindPaneSession(oldKey, ref)
		}
		if n.View != nil {
			if oldBound {
				n.View.ClearRemoteFocusKey(oldKey.Hostname, oldKey.PID, oldKey.StartedAt)
			}
			n.View.Refresh()
		}
		return nil
	}
	replacement, err := n.Registry.BindReplacing(key, ref)
	if err != nil {
		return err
	}
	if n.View != nil && replacement.Changed {
		// A bind can move a pane or create an ambiguity. Require a fresh,
		// post-bind pane-state observation before projecting focus again. An
		// exact replay is deliberately idempotent: it may be a stale callback
		// which completed after a newer pane-state from another Lua state.
		if replacement.HadPrevious && !replacement.PreviousSession.Equal(key) {
			oldKey := replacement.PreviousSession
			n.View.ClearRemoteFocusKey(oldKey.Hostname, oldKey.PID, oldKey.StartedAt)
		}
		n.View.ClearRemoteFocusKey(key.Hostname, key.PID, key.StartedAt)
		n.View.Refresh()
	}
	return nil
}

func paneStateSource(guiPID, windowID int) string {
	return fmt.Sprintf("%d:%d", guiPID, windowID)
}

// PaneState turns WezTerm's active-pane/window-focus observation into a remote
// Focused bit only after independently checking the current pane, marker, WM
// client and active WM address. A late false edge clears only its own source.
func (n *Navigator) PaneState(ctx context.Context, guiPID, windowID, paneID int, windowFocused bool) error {
	if n.View == nil || n.Registry == nil || n.WM == nil {
		return errors.New("federation: pane focus is not configured")
	}
	source := paneStateSource(guiPID, windowID)
	if !windowFocused {
		n.View.ClearRemoteFocusFrom(source)
		return nil
	}
	if n.WM.Name() == "none" {
		// There is no outer OS-window focus fact to project on an Observe-only
		// stack. Acknowledge one fail-closed clear so WezTerm can dedupe it rather
		// than retrying an impossible route check on every update-status tick.
		n.View.ClearRemoteFocusFrom(source)
		return nil
	}
	if !n.WM.Available() {
		// A named backend may recover, so keep Lua retrying; clear the old
		// observation now so it cannot spring back merely because availability
		// changed, without a fresh pane/window fact.
		n.View.ClearRemoteFocusFrom(source)
		return ErrNavigateUnavailable
	}
	ref := panebind.LocalPaneRef{GUIPID: guiPID, WindowID: windowID, PaneID: paneID}
	if err := ref.Validate(); err != nil {
		n.View.ClearRemoteFocusFrom(source)
		return err
	}
	key, bound, live := n.Registry.SessionForPane(ref)
	if !bound {
		// Ordinary and local-session panes are not routes. Treat their state as
		// a successful fail-closed clear so WezTerm can dedupe it; a bound remote
		// candidate below still retries until its authoritative snapshot arrives.
		n.View.ClearRemoteFocusFrom(source)
		return nil
	}
	if !live {
		n.View.ClearRemoteFocusFrom(source)
		return panebind.ErrSessionNotLive
	}
	route, err := n.Routes.Resolve(ctx, key)
	if err != nil {
		n.View.ClearRemoteFocusFrom(source)
		n.pruneDefinitiveMiss(key, ref, err)
		return err
	}
	if route.Ref.WindowID != windowID {
		n.View.ClearRemoteFocusFrom(source)
		return panebind.ErrRouteChanged
	}
	active, err := n.WM.ActiveWindow(ctx)
	if err != nil {
		n.View.ClearRemoteFocusFrom(source)
		return err
	}
	if active == "" || active != route.Window.Address || !n.Registry.IsLiveRoute(key, route.Ref) {
		n.View.ClearRemoteFocusFrom(source)
		return panebind.ErrWindowNotFound
	}
	n.View.SetRemoteFocusFrom(source, key.Hostname, key.PID, key.StartedAt)
	return nil
}
