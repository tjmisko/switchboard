package federation

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/panebind"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/wezterm"
	"github.com/tjmisko/switchboard/internal/wm"
)

type navigatorWM struct {
	windows     []wm.Window
	active      string
	focused     []string
	activeErr   error
	focusErr    error
	unavailable bool
}

func (m *navigatorWM) Name() string    { return "test" }
func (m *navigatorWM) Available() bool { return !m.unavailable }
func (m *navigatorWM) Clients(context.Context) ([]wm.Window, error) {
	return append([]wm.Window(nil), m.windows...), nil
}
func (m *navigatorWM) ActiveWindow(context.Context) (string, error) { return m.active, m.activeErr }
func (m *navigatorWM) Focus(_ context.Context, address string) error {
	m.focused = append(m.focused, address)
	return m.focusErr
}
func (m *navigatorWM) Subscribe(context.Context) (<-chan wm.Event, error) {
	ch := make(chan wm.Event)
	close(ch)
	return ch, nil
}

func remoteNavigator(t *testing.T) (*Navigator, panebind.ExactSessionKey, panebind.LocalPaneRef, *navigatorWM) {
	t.Helper()
	startedAt := time.Unix(10, 0).UTC()
	key := panebind.ExactSessionKey{Hostname: "remote", PID: 5, StartedAt: startedAt}
	ref := panebind.LocalPaneRef{GUIPID: 10, WindowID: 11, PaneID: 12}
	remote := newFakeRemote()
	remote.replace(map[string]state.Snapshot{"remote": {Sessions: []state.Session{{PID: 5, StartedAt: startedAt}}}})
	view, _ := NewView(state.New(""), "local", remote)
	registry := panebind.NewRegistry()
	if err := registry.Bind(key, ref); err != nil {
		t.Fatal(err)
	}
	if err := registry.ReplaceLive("remote", []panebind.ExactSessionKey{key}); err != nil {
		t.Fatal(err)
	}
	manager := &navigatorWM{
		windows: []wm.Window{{Address: "window-address", PID: 10, Title: "shell [sbw:10:11]"}},
		active:  "window-address",
	}
	local := panebind.LocalResolver{
		LookupPane: func(context.Context, panebind.LocalPaneRef) (wezterm.Pane, error) {
			return wezterm.Pane{MuxPID: 10, MuxSocket: "/run/wezterm/gui-sock-10", WindowID: 11, PaneID: 12}, nil
		},
		ListWindows: manager.Clients,
	}
	navigator := &Navigator{
		LocalHostname: "local", View: view, Registry: registry, WM: manager,
		Routes: panebind.RouteResolver{Registry: registry, Local: local},
	}
	view.SetRouteReady(navigator.RouteReady)
	return navigator, key, ref, manager
}

func TestRemoteFocusActivatesThenRevalidatesThenRaises(t *testing.T) {
	navigator, key, _, manager := remoteNavigator(t)
	activations := 0
	navigator.ActivatePane = func(_ context.Context, socket string, paneID int) error {
		activations++
		if socket != "/run/wezterm/gui-sock-10" || paneID != 12 {
			t.Fatalf("activation = %q pane %d", socket, paneID)
		}
		return nil
	}
	if err := navigator.Focus(context.Background(), key.Hostname, key.PID, key.StartedAt); err != nil {
		t.Fatal(err)
	}
	if activations != 1 || len(manager.focused) != 1 || manager.focused[0] != "window-address" {
		t.Fatalf("activations=%d wm focus=%v", activations, manager.focused)
	}
}

func TestRemoteActivationFailureNeverRaisesWindow(t *testing.T) {
	navigator, key, _, manager := remoteNavigator(t)
	navigator.ActivatePane = func(context.Context, string, int) error { return errors.New("activation failed") }
	if err := navigator.Focus(context.Background(), key.Hostname, key.PID, key.StartedAt); err == nil {
		t.Fatal("focus unexpectedly succeeded")
	}
	if len(manager.focused) != 0 {
		t.Fatalf("WM was focused after activation failure: %v", manager.focused)
	}
}

func TestObserveOnlyWMNeverAdvertisesOrAttemptsRemoteNavigation(t *testing.T) {
	navigator, key, ref, manager := remoteNavigator(t)
	if err := navigator.PaneState(context.Background(), ref.GUIPID, ref.WindowID, ref.PaneID, true); err != nil {
		t.Fatal(err)
	}
	if !navigator.View.Snapshot().Sessions[0].Focused {
		t.Fatal("test did not establish the focus overlay before Observe-only clear")
	}
	navigator.WM = wm.NewNone()
	navigator.View.Refresh()

	if navigator.RouteReady(key.Hostname, key.PID, key.StartedAt) {
		t.Fatal("observe-only WM advertised a remote route as ready")
	}
	if snapshot := navigator.View.Snapshot(); len(snapshot.Sessions) != 1 || snapshot.Sessions[0].Navigable {
		t.Fatalf("observe-only aggregate row = %+v, want observe-only", snapshot.Sessions)
	}
	activated := false
	navigator.ActivatePane = func(context.Context, string, int) error {
		activated = true
		return nil
	}
	if err := navigator.Focus(context.Background(), key.Hostname, key.PID, key.StartedAt); !errors.Is(err, ErrNavigateUnavailable) {
		t.Fatalf("Focus error = %v, want ErrNavigateUnavailable", err)
	}
	if activated {
		t.Fatal("observe-only focus reached the pane actuator")
	}
	if err := navigator.PaneState(context.Background(), ref.GUIPID, ref.WindowID, ref.PaneID, true); err != nil {
		t.Fatalf("observe-only pane state should be an acknowledged clear: %v", err)
	}
	// Restore route capability only to prove that the old focus overlay was
	// actually forgotten rather than merely hidden while navigation was absent.
	navigator.WM = manager
	navigator.View.Refresh()
	if navigator.View.Snapshot().Sessions[0].Focused {
		t.Fatal("observe-only pane state left a focus overlay that sprang back")
	}
}

func TestUnavailableNamedWMDoesNotAdvertiseOrAttemptRemoteNavigation(t *testing.T) {
	navigator, key, ref, manager := remoteNavigator(t)
	if err := navigator.PaneState(context.Background(), ref.GUIPID, ref.WindowID, ref.PaneID, true); err != nil {
		t.Fatal(err)
	}
	if !navigator.View.Snapshot().Sessions[0].Focused {
		t.Fatal("test did not establish focus before the WM became unavailable")
	}
	manager.unavailable = true
	navigator.View.Refresh()
	if navigator.RouteReady(key.Hostname, key.PID, key.StartedAt) {
		t.Fatal("unavailable WM advertised a remote route as ready")
	}
	activated := false
	navigator.ActivatePane = func(context.Context, string, int) error {
		activated = true
		return nil
	}
	if err := navigator.Focus(context.Background(), key.Hostname, key.PID, key.StartedAt); !errors.Is(err, ErrNavigateUnavailable) {
		t.Fatalf("Focus error = %v, want ErrNavigateUnavailable", err)
	}
	if activated {
		t.Fatal("unavailable WM focus reached the pane actuator")
	}
	if err := navigator.PaneState(context.Background(), ref.GUIPID, ref.WindowID, ref.PaneID, true); !errors.Is(err, ErrNavigateUnavailable) {
		t.Fatalf("PaneState error = %v, want ErrNavigateUnavailable", err)
	}
	manager.unavailable = false
	navigator.View.Refresh()
	if navigator.View.Snapshot().Sessions[0].Focused {
		t.Fatal("unavailable pane state left a focus overlay that sprang back")
	}
}

func TestRouteDropDuringLookupCausesZeroActuators(t *testing.T) {
	navigator, key, _, manager := remoteNavigator(t)
	navigator.Routes.Local.ListWindows = func(ctx context.Context) ([]wm.Window, error) {
		navigator.Registry.DropLiveHost("remote")
		return manager.Clients(ctx)
	}
	activations := 0
	navigator.ActivatePane = func(context.Context, string, int) error { activations++; return nil }
	if err := navigator.Focus(context.Background(), key.Hostname, key.PID, key.StartedAt); err == nil {
		t.Fatal("focus unexpectedly succeeded")
	}
	if activations != 0 || len(manager.focused) != 0 {
		t.Fatalf("acted on changed route: activations=%d focus=%v", activations, manager.focused)
	}
}

func TestDefinitivelyMissingPanePrunesOnlyItsStaleCandidate(t *testing.T) {
	navigator, key, _, manager := remoteNavigator(t)
	navigator.Routes.Local.LookupPane = func(context.Context, panebind.LocalPaneRef) (wezterm.Pane, error) {
		return wezterm.Pane{}, panebind.ErrPaneNotFound
	}
	activations := 0
	navigator.ActivatePane = func(context.Context, string, int) error { activations++; return nil }
	if err := navigator.Focus(context.Background(), key.Hostname, key.PID, key.StartedAt); !errors.Is(err, panebind.ErrPaneNotFound) {
		t.Fatalf("Focus error = %v, want ErrPaneNotFound", err)
	}
	if activations != 0 || len(manager.focused) != 0 {
		t.Fatalf("acted on missing pane: activations=%d focus=%v", activations, manager.focused)
	}
	if _, err := navigator.Registry.Resolve(key); !errors.Is(err, panebind.ErrSessionUnbound) {
		t.Fatalf("stale candidate remains: %v", err)
	}
	if got := navigator.View.Snapshot().Sessions[0].Navigable; got {
		t.Fatal("pruned route remains navigable")
	}
}

func TestTransientRouteErrorKeepsCandidateRetryable(t *testing.T) {
	navigator, key, _, _ := remoteNavigator(t)
	want := errors.New("wezterm temporarily unavailable")
	navigator.Routes.Local.LookupPane = func(context.Context, panebind.LocalPaneRef) (wezterm.Pane, error) {
		return wezterm.Pane{}, want
	}
	if err := navigator.Focus(context.Background(), key.Hostname, key.PID, key.StartedAt); !errors.Is(err, want) {
		t.Fatalf("Focus error = %v, want transient error", err)
	}
	if _, err := navigator.Registry.Resolve(key); err != nil {
		t.Fatalf("transient failure pruned candidate: %v", err)
	}
}

func TestMissingWindowMarkerKeepsCandidateForRetry(t *testing.T) {
	navigator, key, _, manager := remoteNavigator(t)
	navigator.Routes.Local.ListWindows = func(context.Context) ([]wm.Window, error) {
		return nil, nil
	}
	if err := navigator.Focus(context.Background(), key.Hostname, key.PID, key.StartedAt); !errors.Is(err, panebind.ErrWindowNotFound) {
		t.Fatalf("first Focus error = %v, want ErrWindowNotFound", err)
	}
	if _, err := navigator.Registry.Resolve(key); err != nil {
		t.Fatalf("marker propagation race pruned candidate: %v", err)
	}
	if !navigator.View.Snapshot().Sessions[0].Navigable {
		t.Fatal("marker propagation race made route unnavigable")
	}

	navigator.Routes.Local.ListWindows = manager.Clients
	navigator.ActivatePane = func(context.Context, string, int) error { return nil }
	if err := navigator.Focus(context.Background(), key.Hostname, key.PID, key.StartedAt); err != nil {
		t.Fatalf("Focus after marker appeared: %v", err)
	}
	if len(manager.focused) != 1 {
		t.Fatalf("WM focus calls = %v", manager.focused)
	}
}

func TestPaneMissingAfterActivationPrunesPriorCandidateWithoutRaisingWindow(t *testing.T) {
	navigator, key, _, manager := remoteNavigator(t)
	lookups := 0
	navigator.Routes.Local.LookupPane = func(context.Context, panebind.LocalPaneRef) (wezterm.Pane, error) {
		lookups++
		if lookups == 1 {
			return wezterm.Pane{
				MuxPID: 10, MuxSocket: "/run/wezterm/gui-sock-10", WindowID: 11, PaneID: 12,
			}, nil
		}
		return wezterm.Pane{}, panebind.ErrPaneNotFound
	}
	activations := 0
	navigator.ActivatePane = func(context.Context, string, int) error { activations++; return nil }
	if err := navigator.Focus(context.Background(), key.Hostname, key.PID, key.StartedAt); !errors.Is(err, panebind.ErrPaneNotFound) {
		t.Fatalf("Focus error = %v, want ErrPaneNotFound", err)
	}
	if lookups != 2 || activations != 1 || len(manager.focused) != 0 {
		t.Fatalf("lookups=%d activations=%d WM focus=%v", lookups, activations, manager.focused)
	}
	if _, err := navigator.Registry.Resolve(key); !errors.Is(err, panebind.ErrSessionUnbound) {
		t.Fatalf("post-activation stale candidate remains: %v", err)
	}
	if navigator.View.Snapshot().Sessions[0].Navigable {
		t.Fatal("post-activation stale route remains navigable")
	}
}

func TestPaneStateRequiresBothPaneRouteAndActiveWMWindow(t *testing.T) {
	navigator, _, ref, manager := remoteNavigator(t)
	if err := navigator.PaneState(context.Background(), ref.GUIPID, ref.WindowID, ref.PaneID, true); err != nil {
		t.Fatal(err)
	}
	if snapshot := navigator.View.Snapshot(); !snapshot.Sessions[0].Focused {
		t.Fatalf("valid local focus was not projected: %+v", snapshot.Sessions)
	}

	manager.active = "another-window"
	if err := navigator.PaneState(context.Background(), ref.GUIPID, ref.WindowID, ref.PaneID, true); err == nil {
		t.Fatal("WM mismatch unexpectedly accepted")
	}
	if snapshot := navigator.View.Snapshot(); snapshot.Sessions[0].Focused {
		t.Fatalf("WM-only mismatch left remote focused: %+v", snapshot.Sessions)
	}
}

func TestRebindClearsFocusAndCannotSpringBackAfterAmbiguityResolves(t *testing.T) {
	navigator, key, _, _ := remoteNavigator(t)
	if err := navigator.PaneState(context.Background(), 10, 11, 12, true); err != nil {
		t.Fatal(err)
	}
	if !navigator.View.Snapshot().Sessions[0].Focused {
		t.Fatal("valid pane-state did not establish focus")
	}
	payload, err := panebind.Encode(key)
	if err != nil {
		t.Fatal(err)
	}
	second := panebind.LocalPaneRef{GUIPID: 10, WindowID: 11, PaneID: 13}
	if err := navigator.BindPane(context.Background(), payload, second.GUIPID, second.WindowID, second.PaneID); err != nil {
		t.Fatal(err)
	}
	snapshot := navigator.View.Snapshot().Sessions[0]
	if snapshot.Navigable || snapshot.Focused {
		t.Fatalf("ambiguous rebind projected navigable/focused: %+v", snapshot)
	}

	navigator.Registry.UnbindPane(second)
	navigator.View.Refresh()
	snapshot = navigator.View.Snapshot().Sessions[0]
	if !snapshot.Navigable || snapshot.Focused {
		t.Fatalf("old focus sprang back after ambiguity resolved: %+v", snapshot)
	}
}

func TestSamePaneRebindClearsOldSessionFocusImmediately(t *testing.T) {
	navigator, oldKey, ref, _ := remoteNavigator(t)
	if err := navigator.PaneState(context.Background(), ref.GUIPID, ref.WindowID, ref.PaneID, true); err != nil {
		t.Fatal(err)
	}
	newKey := panebind.ExactSessionKey{Hostname: "remote", PID: 6, StartedAt: oldKey.StartedAt.Add(time.Second)}
	payload, err := panebind.Encode(newKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := navigator.BindPane(context.Background(), payload, ref.GUIPID, ref.WindowID, ref.PaneID); err != nil {
		t.Fatal(err)
	}
	snapshot := navigator.View.Snapshot().Sessions[0]
	if snapshot.Focused || snapshot.Navigable {
		t.Fatalf("old session retained focus/navigation after same-pane rebind: %+v", snapshot)
	}
}

func TestDelayedExactBindingReplayPreservesNewerPaneStateFocus(t *testing.T) {
	navigator, key, ref, _ := remoteNavigator(t)
	if err := navigator.PaneState(context.Background(), ref.GUIPID, ref.WindowID, ref.PaneID, true); err != nil {
		t.Fatal(err)
	}
	payload, err := panebind.Encode(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := navigator.BindPane(context.Background(), payload, ref.GUIPID, ref.WindowID, ref.PaneID); err != nil {
		t.Fatal(err)
	}
	snapshot := navigator.View.Snapshot().Sessions[0]
	if !snapshot.Focused || !snapshot.Navigable {
		t.Fatalf("idempotent replay cleared newer focus: %+v", snapshot)
	}
}

func TestSameKeyMovedRefClearsFocusUntilFreshPaneState(t *testing.T) {
	navigator, key, ref, _ := remoteNavigator(t)
	if err := navigator.PaneState(context.Background(), ref.GUIPID, ref.WindowID, ref.PaneID, true); err != nil {
		t.Fatal(err)
	}
	payload, err := panebind.Encode(key)
	if err != nil {
		t.Fatal(err)
	}
	moved := ref
	moved.WindowID++
	if err := navigator.BindPane(context.Background(), payload, moved.GUIPID, moved.WindowID, moved.PaneID); err != nil {
		t.Fatal(err)
	}
	snapshot := navigator.View.Snapshot().Sessions[0]
	if snapshot.Focused || !snapshot.Navigable {
		t.Fatalf("changed ref did not clear only focus: %+v", snapshot)
	}
}

func TestLocalBindingIsAcknowledgedWithoutEnteringRemoteRegistry(t *testing.T) {
	navigator, remoteKey, remoteRef, _ := remoteNavigator(t)
	if err := navigator.PaneState(context.Background(), remoteRef.GUIPID, remoteRef.WindowID, remoteRef.PaneID, true); err != nil {
		t.Fatal(err)
	}
	key := panebind.ExactSessionKey{Hostname: "local", PID: 99, StartedAt: time.Unix(99, 0)}
	payload, err := panebind.Encode(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := navigator.BindPane(context.Background(), payload, remoteRef.GUIPID, remoteRef.WindowID, remoteRef.PaneID); err != nil {
		t.Fatal(err)
	}
	if _, err := navigator.Registry.Resolve(key); !errors.Is(err, panebind.ErrSessionNotLive) {
		t.Fatalf("local binding entered remote registry: %v", err)
	}
	if _, err := navigator.Registry.Resolve(remoteKey); !errors.Is(err, panebind.ErrSessionUnbound) {
		t.Fatalf("local binding retained the pane's old remote route: %v", err)
	}
	snapshot := navigator.View.Snapshot().Sessions[0]
	if snapshot.Focused || snapshot.Navigable {
		t.Fatalf("local binding retained old remote focus/navigation: %+v", snapshot)
	}
	if err := navigator.PaneState(context.Background(), remoteRef.GUIPID, remoteRef.WindowID, remoteRef.PaneID, true); err != nil {
		t.Fatalf("unbound local pane-state should be an acknowledged clear: %v", err)
	}
}

func TestBoundCandidateWithoutFirstLiveSnapshotStillRetriesPaneState(t *testing.T) {
	navigator, oldKey, ref, _ := remoteNavigator(t)
	key := panebind.ExactSessionKey{Hostname: "remote", PID: 77, StartedAt: oldKey.StartedAt.Add(time.Second)}
	payload, err := panebind.Encode(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := navigator.BindPane(context.Background(), payload, ref.GUIPID, ref.WindowID, ref.PaneID); err != nil {
		t.Fatal(err)
	}
	if err := navigator.PaneState(context.Background(), ref.GUIPID, ref.WindowID, ref.PaneID, true); !errors.Is(err, panebind.ErrSessionNotLive) {
		t.Fatalf("pre-snapshot pane-state error = %v, want ErrSessionNotLive retry", err)
	}
}
