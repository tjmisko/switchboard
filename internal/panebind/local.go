package panebind

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/tjmisko/switchboard/internal/wezterm"
	"github.com/tjmisko/switchboard/internal/wm"
)

type PaneLookupFunc func(context.Context, LocalPaneRef) (wezterm.Pane, error)
type WindowListFunc func(context.Context) ([]wm.Window, error)

// LocalResolver holds narrow injectable seams for the two action-time
// enumerations. Its result is a momentary validation, not durable routing
// state; callers must not persist the WM address.
type LocalResolver struct {
	LookupPane  PaneLookupFunc
	ListWindows WindowListFunc
}

// LocalRoute contains the exact local handles needed for one focus attempt.
type LocalRoute struct {
	Ref    LocalPaneRef
	Pane   wezterm.Pane
	Window wm.Window
}

// NewLocalResolver wires the production WezTerm and WM enumerators. The
// WezTerm lookup enumerates only the gui-sock named by the locally-derived PID.
func NewLocalResolver(manager wm.Manager) LocalResolver {
	var listWindows WindowListFunc
	if manager != nil {
		listWindows = manager.Clients
	}
	return LocalResolver{LookupPane: lookupSystemPane, ListWindows: listWindows}
}

func lookupSystemPane(ctx context.Context, ref LocalPaneRef) (wezterm.Pane, error) {
	panes, err := wezterm.ListGUI(ctx, ref.GUIPID)
	if err != nil {
		return wezterm.Pane{}, localPaneLookupError(err)
	}
	var match wezterm.Pane
	count := 0
	for _, pane := range panes {
		if pane.MuxPID == ref.GUIPID && pane.PaneID == ref.PaneID {
			match = pane
			count++
		}
	}
	switch count {
	case 0:
		return wezterm.Pane{}, ErrPaneNotFound
	case 1:
		return match, nil
	default:
		return wezterm.Pane{}, ErrPaneAmbiguous
	}
}

func localPaneLookupError(err error) error {
	if errors.Is(err, wezterm.ErrGUISocketPeer) {
		// A live process behind gui-sock-<pid> with a different peer PID is
		// definitive PID reuse/stale-route evidence, not a transient CLI
		// failure. Preserve both classifications for action-time pruning.
		return fmt.Errorf("%w: %w", ErrPaneNotFound, err)
	}
	return err
}

// Resolve rechecks the complete local join: exact GUI socket pane/window and
// exactly one WM client whose PID and title suffix carry the stable marker.
func (r LocalResolver) Resolve(ctx context.Context, ref LocalPaneRef) (LocalRoute, error) {
	if err := ref.Validate(); err != nil {
		return LocalRoute{}, err
	}
	if r.LookupPane == nil || r.ListWindows == nil {
		return LocalRoute{}, errors.New("panebind: local resolver is not configured")
	}
	pane, err := r.LookupPane(ctx, ref)
	if err != nil {
		return LocalRoute{}, err
	}
	wantSocketName := fmt.Sprintf("gui-sock-%d", ref.GUIPID)
	if pane.MuxPID != ref.GUIPID || pane.PaneID != ref.PaneID ||
		filepath.Base(pane.MuxSocket) != wantSocketName {
		return LocalRoute{}, ErrPaneNotFound
	}
	currentRef := LocalPaneRef{GUIPID: ref.GUIPID, WindowID: pane.WindowID, PaneID: ref.PaneID}
	windows, err := r.ListWindows(ctx)
	if err != nil {
		return LocalRoute{}, err
	}
	marker := WindowMarker(currentRef)
	var window wm.Window
	count := 0
	for _, candidate := range windows {
		if candidate.PID == ref.GUIPID && candidate.Address != "" &&
			strings.HasSuffix(strings.TrimSpace(candidate.Title), marker) {
			window = candidate
			count++
		}
	}
	switch count {
	case 0:
		return LocalRoute{}, ErrWindowNotFound
	case 1:
		return LocalRoute{Ref: currentRef, Pane: pane, Window: window}, nil
	default:
		return LocalRoute{}, ErrWindowAmbiguous
	}
}

// RouteResolver composes registry liveness/ambiguity gating with the local
// action-time lookup. The final registry check detects a disconnect, PID
// replacement, ambiguity, or pane rebind that raced the local enumeration.
type RouteResolver struct {
	Registry *Registry
	Local    LocalResolver
}

func (r RouteResolver) Resolve(ctx context.Context, key ExactSessionKey) (LocalRoute, error) {
	if r.Registry == nil {
		return LocalRoute{}, errors.New("panebind: registry is not configured")
	}
	pane, err := r.Registry.Resolve(key)
	if err != nil {
		return LocalRoute{}, err
	}
	route, err := r.Local.Resolve(ctx, pane)
	if err != nil {
		return LocalRoute{}, err
	}
	if !r.Registry.RefreshLiveRoute(key, route.Ref) || !r.Registry.IsLiveRoute(key, route.Ref) {
		return LocalRoute{}, ErrRouteChanged
	}
	return route, nil
}

// RevalidateAfterActivation performs the required second action-time lookup
// after WezTerm pane activation and before the WM raise. It permits WindowID to
// refresh if the same stable pane moved, but refuses to switch to a different
// pane midway through one focus attempt.
func (r RouteResolver) RevalidateAfterActivation(ctx context.Context, key ExactSessionKey, prior LocalRoute) (LocalRoute, error) {
	current, err := r.Resolve(ctx, key)
	if err != nil {
		return LocalRoute{}, err
	}
	if identityOf(current.Ref) != identityOf(prior.Ref) {
		return LocalRoute{}, ErrRouteChanged
	}
	return current, nil
}
