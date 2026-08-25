package panebind

import (
	"context"
	"errors"
	"testing"

	"github.com/tjmisko/switchboard/internal/wezterm"
	"github.com/tjmisko/switchboard/internal/wm"
)

func localResolver(pane wezterm.Pane, windows []wm.Window) LocalResolver {
	return LocalResolver{
		LookupPane:  func(_ context.Context, _ LocalPaneRef) (wezterm.Pane, error) { return pane, nil },
		ListWindows: func(context.Context) ([]wm.Window, error) { return windows, nil },
	}
}

func TestLocalPaneLookupClassifiesOnlyPeerMismatchAsDefinitiveMissing(t *testing.T) {
	got := localPaneLookupError(wezterm.ErrGUISocketPeer)
	if !errors.Is(got, ErrPaneNotFound) || !errors.Is(got, wezterm.ErrGUISocketPeer) {
		t.Fatalf("peer mismatch classification = %v", got)
	}
	temporary := errors.New("temporary")
	if got := localPaneLookupError(temporary); got != temporary || errors.Is(got, ErrPaneNotFound) {
		t.Fatalf("temporary error was classified as missing: %v", got)
	}
}

func TestLocalResolveUsesStablePaneIdentityAndCurrentWindow(t *testing.T) {
	stale := LocalPaneRef{GUIPID: 100, WindowID: 2, PaneID: 7}
	current := LocalPaneRef{GUIPID: 100, WindowID: 9, PaneID: 7}
	pane := wezterm.Pane{
		MuxPID: 100, MuxSocket: "/run/user/1000/wezterm/gui-sock-100", WindowID: 9, PaneID: 7,
	}
	windows := []wm.Window{
		{Address: "0xwrong-pid", PID: 999, Title: "wez [sbw:100:9]"},
		{Address: "0xincidental", PID: 100, Title: "[sbw:100:9] is not a suffix"},
		{Address: "0xright", PID: 100, Title: "project [sbw:100:9]"},
	}
	got, err := localResolver(pane, windows).Resolve(t.Context(), stale)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Ref != current || got.Pane != pane || got.Window.Address != "0xright" {
		t.Fatalf("route = %+v, want current ref %+v and exact window", got, current)
	}
}

func TestLocalResolveRejectsMismatchedPaneFacts(t *testing.T) {
	ref := LocalPaneRef{GUIPID: 100, WindowID: 2, PaneID: 7}
	tests := []struct {
		name string
		pane wezterm.Pane
	}{
		{"wrong gui pid", wezterm.Pane{MuxPID: 101, MuxSocket: "/r/gui-sock-101", WindowID: 2, PaneID: 7}},
		{"wrong pane", wezterm.Pane{MuxPID: 100, MuxSocket: "/r/gui-sock-100", WindowID: 2, PaneID: 8}},
		{"wrong socket", wezterm.Pane{MuxPID: 100, MuxSocket: "/r/gui-sock-999", WindowID: 2, PaneID: 7}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := localResolver(tt.pane, nil).Resolve(t.Context(), ref)
			if !errors.Is(err, ErrPaneNotFound) {
				t.Fatalf("error = %v, want pane not found", err)
			}
		})
	}
}

func TestLocalResolveFailsClosedForMissingOrDuplicateMarkedWindows(t *testing.T) {
	ref := LocalPaneRef{GUIPID: 100, WindowID: 2, PaneID: 7}
	pane := wezterm.Pane{MuxPID: 100, MuxSocket: "/r/gui-sock-100", WindowID: 2, PaneID: 7}
	missing := []wm.Window{{Address: "0x1", PID: 100, Title: "unmarked"}}
	if _, err := localResolver(pane, missing).Resolve(t.Context(), ref); !errors.Is(err, ErrWindowNotFound) {
		t.Fatalf("missing error = %v", err)
	}
	duplicate := []wm.Window{
		{Address: "0x1", PID: 100, Title: "one [sbw:100:2]"},
		{Address: "0x2", PID: 100, Title: "two [sbw:100:2]"},
	}
	if _, err := localResolver(pane, duplicate).Resolve(t.Context(), ref); !errors.Is(err, ErrWindowAmbiguous) {
		t.Fatalf("duplicate error = %v", err)
	}
}

func TestRouteResolverRefreshesMovedWindowInRegistry(t *testing.T) {
	registry := NewRegistry()
	key := exact("h", 1, "2026-08-24T20:00:00Z")
	stale := LocalPaneRef{GUIPID: 100, WindowID: 2, PaneID: 7}
	current := LocalPaneRef{GUIPID: 100, WindowID: 9, PaneID: 7}
	if err := registry.ReplaceLive("h", []ExactSessionKey{key}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Bind(key, stale); err != nil {
		t.Fatal(err)
	}
	pane := wezterm.Pane{MuxPID: 100, MuxSocket: "/r/gui-sock-100", WindowID: 9, PaneID: 7}
	resolver := RouteResolver{
		Registry: registry,
		Local:    localResolver(pane, []wm.Window{{Address: "0x9", PID: 100, Title: "p [sbw:100:9]"}}),
	}
	got, err := resolver.Resolve(t.Context(), key)
	if err != nil || got.Ref != current {
		t.Fatalf("Resolve = (%+v,%v)", got, err)
	}
	if stored, err := registry.Resolve(key); err != nil || stored != current {
		t.Fatalf("registry route = (%+v,%v), want refreshed", stored, err)
	}
}

func TestRouteResolverDoesNotOverwriteConcurrentRebind(t *testing.T) {
	registry := NewRegistry()
	first := exact("h", 1, "2026-08-24T20:00:00Z")
	second := exact("h", 2, "2026-08-24T20:00:01Z")
	ref := LocalPaneRef{GUIPID: 100, WindowID: 2, PaneID: 7}
	if err := registry.ReplaceLive("h", []ExactSessionKey{first, second}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Bind(first, ref); err != nil {
		t.Fatal(err)
	}
	resolver := RouteResolver{
		Registry: registry,
		Local: LocalResolver{
			LookupPane: func(context.Context, LocalPaneRef) (wezterm.Pane, error) {
				if err := registry.Bind(second, ref); err != nil {
					t.Fatal(err)
				}
				return wezterm.Pane{MuxPID: 100, MuxSocket: "/r/gui-sock-100", WindowID: 2, PaneID: 7}, nil
			},
			ListWindows: func(context.Context) ([]wm.Window, error) {
				return []wm.Window{{Address: "0x2", PID: 100, Title: "p [sbw:100:2]"}}, nil
			},
		},
	}
	if _, err := resolver.Resolve(t.Context(), first); !errors.Is(err, ErrRouteChanged) {
		t.Fatalf("error = %v, want route changed", err)
	}
	if got, _, _ := registry.SessionForPane(ref); !got.Equal(second) {
		t.Fatalf("concurrent rebind overwritten: %+v", got)
	}
}

func TestRevalidateAfterActivationRequiresSameStablePane(t *testing.T) {
	registry := NewRegistry()
	key := exact("h", 1, "2026-08-24T20:00:00Z")
	first := LocalPaneRef{GUIPID: 100, WindowID: 2, PaneID: 7}
	second := LocalPaneRef{GUIPID: 100, WindowID: 3, PaneID: 8}
	if err := registry.ReplaceLive("h", []ExactSessionKey{key}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Bind(key, first); err != nil {
		t.Fatal(err)
	}
	current := first
	resolver := RouteResolver{
		Registry: registry,
		Local: LocalResolver{
			LookupPane: func(context.Context, LocalPaneRef) (wezterm.Pane, error) {
				return wezterm.Pane{MuxPID: current.GUIPID, MuxSocket: "/r/gui-sock-100", WindowID: current.WindowID, PaneID: current.PaneID}, nil
			},
			ListWindows: func(context.Context) ([]wm.Window, error) {
				return []wm.Window{{Address: "0x", PID: 100, Title: "p " + WindowMarker(current)}}, nil
			},
		},
	}
	prior, err := resolver.Resolve(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	registry.UnbindPane(first)
	current = second
	if err := registry.Bind(key, second); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.RevalidateAfterActivation(t.Context(), key, prior); !errors.Is(err, ErrRouteChanged) {
		t.Fatalf("error = %v, want route changed", err)
	}
}
