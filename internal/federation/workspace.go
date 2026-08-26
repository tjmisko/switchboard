package federation

import (
	"strings"
	"sync"
	"time"

	"github.com/tjmisko/switchboard/internal/panebind"
	"github.com/tjmisko/switchboard/internal/wm"
)

// WorkspaceIndex answers the one question the aggregate needs in order to place
// a remote chip: which LOCAL workspace holds the terminal window displaying
// that remote session's SSH pane.
//
// A remote row arrives carrying the workspace it occupies on its own desktop,
// and federation.View drops that value on purpose — chip 3 on another machine
// says nothing about where the user looks on this one. But the session IS
// visible here, inside the exact local WezTerm window panebind already resolved
// for navigation, and that window sits on a local workspace like any other. So
// the remote row is ordered by where the user actually sees it, not appended
// after every local chip.
//
// The join is the same one mapping uses for local sessions: the WezTerm
// integration's [sbw:<gui-pid>:<window-id>] title marker against the WM client
// list, failing closed on ambiguity.
//
// It performs NO I/O. The window enumeration is handed to it by the reconcile
// tick that already fetched it (mapping.Resolver.Enumerate), because Workspace
// is called on the publish path — once per remote row per snapshot — and a fork
// per lookup there is exactly the cost the batched reconcile exists to remove.
type WorkspaceIndex struct {
	registry *panebind.Registry

	mu      sync.RWMutex
	clients []wm.Window
}

func NewWorkspaceIndex(registry *panebind.Registry) *WorkspaceIndex {
	return &WorkspaceIndex{registry: registry}
}

// ObserveWindows adopts a reconcile tick's WM client enumeration.
//
// A nil slice means the WM query FAILED, and it is ignored rather than stored:
// clearing the cache would drop every remote chip to the end of the bar and
// bounce it back on the next tick. This mirrors mapping.ReconcileFrom, where a
// missing observation never blanks a resolved mapping. An empty non-nil slice
// is a real answer (no windows) and does replace the cache.
func (w *WorkspaceIndex) ObserveWindows(clients []wm.Window) {
	if w == nil || clients == nil {
		return
	}
	// Copied because the caller fans its enumeration across every session in the
	// tick and this reference outlives that tick.
	adopted := append([]wm.Window(nil), clients...)
	w.mu.Lock()
	w.clients = adopted
	w.mu.Unlock()
}

// Workspace returns the local workspace ID displaying an exact remote session,
// and whether it resolved. Unresolved is the ordinary state — no binding yet,
// no window manager, a pane that moved since the last enumeration — and the
// caller orders such a row exactly as it did before, by start time at the end
// of the bar.
func (w *WorkspaceIndex) Workspace(host string, pid int, startedAt time.Time) (int, bool) {
	if w == nil || w.registry == nil {
		return 0, false
	}
	key := panebind.ExactSessionKey{Hostname: host, PID: pid, StartedAt: startedAt}
	// Resolve is the liveness/ambiguity gate: a stale candidate or a session
	// bound to two panes has no single window to be ordered by.
	ref, err := w.registry.Resolve(key)
	if err != nil {
		return 0, false
	}
	marker := panebind.WindowMarker(ref)

	w.mu.RLock()
	clients := w.clients
	w.mu.RUnlock()

	workspace, matches := 0, 0
	for _, client := range clients {
		if client.PID != ref.GUIPID || !strings.HasSuffix(strings.TrimSpace(client.Title), marker) {
			continue
		}
		workspace = client.WorkspaceID
		matches++
	}
	// Workspace ID 0 is unset, not a workspace (Hyprland numbers are positive,
	// or negative for special workspaces) — the same rule state applies to a
	// session's own window.
	if matches != 1 || workspace == 0 {
		return 0, false
	}
	return workspace, true
}
