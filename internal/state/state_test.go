package state

import (
	"testing"
	"time"
)

func ts(sec int) time.Time {
	return time.Date(2026, 1, 1, 0, 0, sec, 0, time.UTC)
}

// order returns the session PIDs in snapshot order, which is the order chips
// are rendered left-to-right on the bottom bar.
func order(snap Snapshot) []int {
	pids := make([]int, len(snap.Sessions))
	for i, s := range snap.Sessions {
		pids[i] = s.PID
	}
	return pids
}

func seed(store *Store, sessions ...*Session) {
	store.Apply(func(m map[int]*Session) {
		for _, s := range sessions {
			m[s.PID] = s
		}
	})
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func ws(id int) *HyprlandInfo {
	return &HyprlandInfo{WorkspaceID: id, Workspace: "ws"}
}

func TestSnapshotOrder_followsWorkspaceIDWhenResolved(t *testing.T) {
	store := New("")
	// Insert out of workspace order, and with start times that contradict
	// workspace order, to prove workspace ID is the primary key.
	seed(store,
		&Session{PID: 1, StartedAt: ts(10), Hyprland: ws(3)},
		&Session{PID: 2, StartedAt: ts(20), Hyprland: ws(1)},
		&Session{PID: 3, StartedAt: ts(30), Hyprland: ws(2)},
	)
	got := order(store.Snapshot())
	want := []int{2, 3, 1} // ws 1, 2, 3
	if !equalInts(got, want) {
		t.Fatalf("order by workspace id: got %v want %v", got, want)
	}
}

func TestSnapshotOrder_tieBreaksByStartedAtWithinWorkspace(t *testing.T) {
	store := New("")
	seed(store,
		&Session{PID: 1, StartedAt: ts(30), Hyprland: ws(2)},
		&Session{PID: 2, StartedAt: ts(10), Hyprland: ws(2)},
		&Session{PID: 3, StartedAt: ts(20), Hyprland: ws(2)},
	)
	got := order(store.Snapshot())
	want := []int{2, 3, 1} // same workspace -> oldest first
	if !equalInts(got, want) {
		t.Fatalf("tie-break by StartedAt: got %v want %v", got, want)
	}
}

func TestSnapshotOrder_unresolvedWorkspaceGoesLastByStartedAt(t *testing.T) {
	store := New("")
	seed(store,
		&Session{PID: 1, StartedAt: ts(5), Hyprland: ws(2)},
		&Session{PID: 2, StartedAt: ts(40)},                                            // no hyprland at all
		&Session{PID: 3, StartedAt: ts(10), Hyprland: ws(1)},                           // resolved
		&Session{PID: 4, StartedAt: ts(15), Hyprland: &HyprlandInfo{Address: "0xabc"}}, // hyprland but no workspace id
	)
	got := order(store.Snapshot())
	// Resolved first by ws id (3 -> ws1, 1 -> ws2), then unresolved by StartedAt (4 @15, 2 @40).
	want := []int{3, 1, 4, 2}
	if !equalInts(got, want) {
		t.Fatalf("unresolved last: got %v want %v", got, want)
	}
}

func TestSnapshotOrder_specialWorkspaceSortsByID(t *testing.T) {
	store := New("")
	// Special workspaces use negative IDs; they should sort before positive ones.
	seed(store,
		&Session{PID: 1, StartedAt: ts(10), Hyprland: ws(1)},
		&Session{PID: 2, StartedAt: ts(20), Hyprland: ws(-99)},
	)
	got := order(store.Snapshot())
	want := []int{2, 1}
	if !equalInts(got, want) {
		t.Fatalf("special workspace order: got %v want %v", got, want)
	}
}
