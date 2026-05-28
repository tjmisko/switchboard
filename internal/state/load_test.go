package state_test

import (
	"path/filepath"
	"testing"

	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/testsupport"
)

// §4.5 Load — hydrates the store from a valid on-disk mirror, keyed by PID.
// Drives Load from the frozen golden fixture via the harness's Golden reader,
// proving the public contract round-trips back into a live store.
func TestLoadHydratesFromGolden(t *testing.T) {
	golden := testsupport.Golden(t, filepath.Join("testdata", "state.golden.json"))

	path := filepath.Join(t.TempDir(), "state.json")
	testsupport.WriteFile(t, path, string(golden))

	store := state.New(path)
	if err := store.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	snap := store.Snapshot()
	if len(snap.Sessions) != 2 {
		t.Fatalf("hydrated %d sessions, want 2", len(snap.Sessions))
	}

	byPID := map[int]state.Session{}
	for _, s := range snap.Sessions {
		byPID[s.PID] = s
	}
	if _, ok := byPID[4821]; !ok {
		t.Errorf("session 4821 not hydrated")
	}
	if s, ok := byPID[5102]; !ok {
		t.Errorf("session 5102 not hydrated")
	} else if s.Wezterm != nil {
		t.Errorf("minimal session 5102 should have nil Wezterm, got %+v", s.Wezterm)
	}
	if !byPID[4821].Focused {
		t.Errorf("session 4821 should be focused per golden")
	}
}

// §4.5 Load — no-op (nil) on empty path and on a missing file.
func TestLoadNoOpOnEmptyPathAndMissingFile(t *testing.T) {
	if err := state.New("").Load(); err != nil {
		t.Errorf("Load with empty path = %v, want nil", err)
	}

	missing := filepath.Join(t.TempDir(), "does-not-exist.json")
	store := state.New(missing)
	if err := store.Load(); err != nil {
		t.Errorf("Load of missing file = %v, want nil", err)
	}
	if n := len(store.Snapshot().Sessions); n != 0 {
		t.Errorf("missing-file Load hydrated %d sessions, want 0", n)
	}
}

// §4.5 ⚠ characterization: a corrupt mirror returns an error and hydrates
// nothing — previously-persisted sessions are not restored (the daemon logs and
// rebuilds from the live scan).
func TestLoadCorruptReturnsErrorAndHydratesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	testsupport.WriteFile(t, path, "{ this is not valid json ]")

	store := state.New(path)
	if err := store.Load(); err == nil {
		t.Error("Load of corrupt JSON = nil, want error")
	}
	if n := len(store.Snapshot().Sessions); n != 0 {
		t.Errorf("corrupt-file Load hydrated %d sessions, want 0", n)
	}
}
