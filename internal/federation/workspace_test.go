package federation

import (
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/panebind"
	"github.com/tjmisko/switchboard/internal/wm"
)

const workspaceHost = "buildbox"

var workspaceStart = time.Unix(1_756_000_000, 0).UTC()

// boundIndex returns an index whose one remote session is live and bound to
// pane, plus that session's key.
func boundIndex(t *testing.T, pane panebind.LocalPaneRef) (*WorkspaceIndex, panebind.ExactSessionKey) {
	t.Helper()
	registry := panebind.NewRegistry()
	key := panebind.ExactSessionKey{Hostname: workspaceHost, PID: 42, StartedAt: workspaceStart}
	if err := registry.ReplaceLive(workspaceHost, []panebind.ExactSessionKey{key}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Bind(key, pane); err != nil {
		t.Fatal(err)
	}
	return NewWorkspaceIndex(registry), key
}

func markedWindow(pane panebind.LocalPaneRef, workspace int) wm.Window {
	return wm.Window{
		Address:     "0xdead",
		PID:         pane.GUIPID,
		Title:       "claude — /work " + panebind.WindowMarker(pane),
		Workspace:   "3",
		WorkspaceID: workspace,
	}
}

func TestWorkspaceIndexResolvesTheLocalWindowShowingARemoteSession(t *testing.T) {
	pane := panebind.LocalPaneRef{GUIPID: 3344, WindowID: 14, PaneID: 9}
	index, key := boundIndex(t, pane)
	index.ObserveWindows([]wm.Window{
		{Address: "0xbeef", PID: pane.GUIPID, Title: "btop " + panebind.WindowMarker(panebind.LocalPaneRef{GUIPID: pane.GUIPID, WindowID: 1}), WorkspaceID: 1},
		markedWindow(pane, 3),
	})

	if got, ok := index.Workspace(key.Hostname, key.PID, key.StartedAt); !ok || got != 3 {
		t.Fatalf("Workspace = (%d,%t), want (3,true)", got, ok)
	}
}

func TestWorkspaceIndexReportsUnresolvedWhenTheRouteIsNotNavigable(t *testing.T) {
	pane := panebind.LocalPaneRef{GUIPID: 3344, WindowID: 14, PaneID: 9}
	index, key := boundIndex(t, pane)
	index.ObserveWindows([]wm.Window{markedWindow(pane, 3)})

	// A session with no binding at all, and one whose host disconnected, both
	// have no local window to be ordered by.
	if _, ok := index.Workspace(key.Hostname, 999, key.StartedAt); ok {
		t.Fatal("unbound session resolved a workspace")
	}
	index.registry.DropLiveHost(workspaceHost)
	if _, ok := index.Workspace(key.Hostname, key.PID, key.StartedAt); ok {
		t.Fatal("dropped host resolved a workspace")
	}
}

func TestWorkspaceIndexFailsClosedOnAmbiguousAndUnstampedWindows(t *testing.T) {
	pane := panebind.LocalPaneRef{GUIPID: 3344, WindowID: 14, PaneID: 9}
	index, key := boundIndex(t, pane)

	// Two windows carrying one marker: guessing between them would put the chip
	// in an arbitrary place and move it on the next enumeration.
	index.ObserveWindows([]wm.Window{markedWindow(pane, 3), markedWindow(pane, 6)})
	if got, ok := index.Workspace(key.Hostname, key.PID, key.StartedAt); ok {
		t.Fatalf("ambiguous marker resolved workspace %d", got)
	}

	// Workspace 0 is "not reported", not a workspace.
	index.ObserveWindows([]wm.Window{markedWindow(pane, 0)})
	if got, ok := index.Workspace(key.Hostname, key.PID, key.StartedAt); ok {
		t.Fatalf("unset workspace resolved as %d", got)
	}

	// Same window, same GUI, wrong pane: the marker is the whole join.
	index.ObserveWindows([]wm.Window{markedWindow(panebind.LocalPaneRef{GUIPID: pane.GUIPID, WindowID: 15}, 3)})
	if got, ok := index.Workspace(key.Hostname, key.PID, key.StartedAt); ok {
		t.Fatalf("another window's marker resolved workspace %d", got)
	}
}

func TestWorkspaceIndexKeepsLastEnumerationWhenTheWMQueryFails(t *testing.T) {
	pane := panebind.LocalPaneRef{GUIPID: 3344, WindowID: 14, PaneID: 9}
	index, key := boundIndex(t, pane)
	index.ObserveWindows([]wm.Window{markedWindow(pane, 3)})

	// nil is a failed query. Dropping the cache would send every remote chip to
	// the end of the bar and bring it back a tick later.
	index.ObserveWindows(nil)
	if got, ok := index.Workspace(key.Hostname, key.PID, key.StartedAt); !ok || got != 3 {
		t.Fatalf("after failed query Workspace = (%d,%t), want (3,true)", got, ok)
	}

	// An empty enumeration is a real answer: the window is gone.
	index.ObserveWindows([]wm.Window{})
	if got, ok := index.Workspace(key.Hostname, key.PID, key.StartedAt); ok {
		t.Fatalf("empty enumeration resolved workspace %d", got)
	}
}
