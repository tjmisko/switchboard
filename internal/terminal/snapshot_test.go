package terminal

import (
	"context"
	"errors"
	"testing"
)

// SnapshotOrNil is the upgrade half of the optional fast-path: a backend that
// never grew one keeps working through Locate, unchanged.
func TestSnapshotOrNilShouldReturnNilWhenTheBackendHasNoBatchPath(t *testing.T) {
	single := &fakeLocator{name: "kitty", available: true, wantTTY: "/dev/pts/3", pane: &PaneRef{Backend: "kitty", TTY: "/dev/pts/3"}}

	if got := SnapshotOrNil(context.Background(), single); got != nil {
		t.Errorf("SnapshotOrNil = %+v, want nil for a Locator that is not a Snapshotter", got)
	}
}

func TestSnapshotOrNilShouldReturnThePaneSetWhenTheBackendProvidesOne(t *testing.T) {
	batch := newBatchFake("wezterm", map[string]PaneRef{
		"/dev/pts/9": {Backend: "wezterm", TTY: "/dev/pts/9"},
	})

	got := SnapshotOrNil(context.Background(), batch)
	if len(got) != 1 || got["/dev/pts/9"].Backend != "wezterm" {
		t.Errorf("SnapshotOrNil = %+v, want the backend's pane set", got)
	}
}

// A failed enumeration must degrade to the single path, not be mistaken for an
// authoritative empty set — returning the empty map would blank every session's
// pane on one transient `wezterm cli list` failure.
func TestSnapshotOrNilShouldReturnNilWhenTheSnapshotFails(t *testing.T) {
	broken := newBatchFake("wezterm", nil)
	broken.snapErr = errors.New("mux socket went away")

	if got := SnapshotOrNil(context.Background(), broken); got != nil {
		t.Errorf("SnapshotOrNil = %+v, want nil so the caller falls back to Locate", got)
	}
}

func TestNoneSnapshotShouldReportAnEmptySetRatherThanAnError(t *testing.T) {
	got := SnapshotOrNil(context.Background(), NewNone())
	if got == nil {
		t.Fatal("SnapshotOrNil(none) = nil; owning nothing is a complete answer, not a failed one")
	}
	if len(got) != 0 {
		t.Errorf("SnapshotOrNil(none) = %+v, want empty", got)
	}
}
