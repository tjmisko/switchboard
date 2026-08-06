package terminal

import (
	"context"
	"errors"
	"testing"
)

// Snapshot is the upgrade half of the optional fast-path: a backend that never
// grew one keeps working through Locate, unchanged. It reports that case as
// ErrNoBatchPath, which the caller must be able to tell apart from a failure —
// the two warrant opposite responses.
func TestSnapshotShouldReportNoBatchPathWhenTheBackendHasNone(t *testing.T) {
	single := &fakeLocator{name: "kitty", available: true, wantTTY: "/dev/pts/3", pane: &PaneRef{Backend: "kitty", TTY: "/dev/pts/3"}}

	got, err := Snapshot(context.Background(), single)
	if got != nil {
		t.Errorf("Snapshot = %+v, want nil for a Locator that is not a Snapshotter", got)
	}
	if !errors.Is(err, ErrNoBatchPath) {
		t.Errorf("Snapshot err = %v, want ErrNoBatchPath so the caller can fall back to Locate", err)
	}
}

func TestSnapshotShouldReturnThePaneSetWhenTheBackendProvidesOne(t *testing.T) {
	batch := newBatchFake("wezterm", map[string]PaneRef{
		"/dev/pts/9": {Backend: "wezterm", TTY: "/dev/pts/9"},
	})

	got, err := Snapshot(context.Background(), batch)
	if err != nil {
		t.Fatalf("Snapshot err = %v, want nil", err)
	}
	if len(got) != 1 || got["/dev/pts/9"].Backend != "wezterm" {
		t.Errorf("Snapshot = %+v, want the backend's pane set", got)
	}
}

// A failed enumeration must not be mistaken for an authoritative empty set —
// returning the empty map would blank every session's pane on one transient
// `wezterm cli list` failure. It must ALSO not be mistaken for a backend without
// a batch path: that is what put a per-session enumeration back under the store
// lock on every mux blip.
func TestSnapshotShouldReportAFailureAsDistinctFromNoBatchPath(t *testing.T) {
	broken := newBatchFake("wezterm", nil)
	broken.snapErr = errors.New("mux socket went away")

	got, err := Snapshot(context.Background(), broken)
	if got != nil {
		t.Errorf("Snapshot = %+v, want nil rather than an empty set that reads as 'no tty owns a pane'", got)
	}
	if err == nil {
		t.Fatal("Snapshot err = nil after a failed enumeration")
	}
	if errors.Is(err, ErrNoBatchPath) {
		t.Errorf("Snapshot err = %v, want a transient error; ErrNoBatchPath would send the caller to per-session Locate", err)
	}
}

func TestNoneSnapshotShouldReportAnEmptySetRatherThanAnError(t *testing.T) {
	got, err := Snapshot(context.Background(), NewNone())
	if err != nil {
		t.Fatalf("Snapshot(none) err = %v; owning nothing is a complete answer, not a failed one", err)
	}
	if got == nil {
		t.Fatal("Snapshot(none) = nil; owning nothing is a complete answer, not a failed one")
	}
	if len(got) != 0 {
		t.Errorf("Snapshot(none) = %+v, want empty", got)
	}
}
