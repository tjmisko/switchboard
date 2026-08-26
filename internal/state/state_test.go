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

func TestSortChipOrder_injectedWorkspaceOverridesTheSessionsOwnWindow(t *testing.T) {
	// The federated view's case: rows 2 and 4 are remote, so they carry no
	// window of their own, and the key says where each is DISPLAYED here.
	sessions := []Session{
		{PID: 1, StartedAt: ts(10), Hyprland: ws(2)},
		{PID: 2, StartedAt: ts(20)},
		{PID: 3, StartedAt: ts(30), Hyprland: ws(5)},
		{PID: 4, StartedAt: ts(40)},
	}
	SortChipOrder(sessions, func(s Session) (int, bool) {
		if s.PID == 2 {
			return 3, true
		}
		return 0, false // unknown: fall back to the session's own window
	})
	got := make([]int, len(sessions))
	for i := range sessions {
		got[i] = sessions[i].PID
	}
	want := []int{1, 2, 3, 4} // ws 2, injected ws 3, ws 5, then unplaced
	if !equalInts(got, want) {
		t.Fatalf("injected workspace order: got %v want %v", got, want)
	}
}

func TestSortChipOrder_injectedWorkspaceWinsOverAStaleWindow(t *testing.T) {
	// A remote row may still carry its own desktop's workspace; the injected
	// key is the one that means something on the machine drawing the bar.
	sessions := []Session{
		{PID: 1, StartedAt: ts(10), Hyprland: ws(1)},
		{PID: 2, StartedAt: ts(20), Hyprland: ws(9)},
	}
	SortChipOrder(sessions, func(s Session) (int, bool) {
		if s.PID == 2 {
			return -1, true
		}
		return 0, false
	})
	if sessions[0].PID != 2 {
		t.Fatalf("injected key ignored: got %d first", sessions[0].PID)
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
