package state_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/state"
)

// §4.1 Apply — mutates, broadcasts, and persists. We observe both side effects:
// a subscriber receives the post-mutation snapshot and the on-disk mirror
// reflects the same session set.
func TestApplyBroadcastsAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := state.New(path)

	ch, cancel := store.Subscribe()
	defer cancel()

	store.Apply(func(m map[int]*state.Session) {
		m[42] = &state.Session{PID: 42, StartedAt: time.Unix(1000, 0)}
	})

	select {
	case snap := <-ch:
		if len(snap.Sessions) != 1 || snap.Sessions[0].PID != 42 {
			t.Fatalf("broadcast snapshot = %+v, want one session PID 42", snap.Sessions)
		}
	case <-time.After(time.Second):
		t.Fatal("Apply did not broadcast to subscriber")
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("persisted file not written: %v", err)
	}
	var snap state.Snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		t.Fatalf("persisted file corrupt: %v", err)
	}
	if len(snap.Sessions) != 1 || snap.Sessions[0].PID != 42 {
		t.Errorf("persisted sessions = %+v, want one PID 42", snap.Sessions)
	}
}

// §4.2 Snapshot — sorts ascending by StartedAt.
func TestSnapshotSortsByStartedAt(t *testing.T) {
	store := state.New("")
	store.Apply(func(m map[int]*state.Session) {
		m[1] = &state.Session{PID: 1, StartedAt: time.Unix(300, 0)}
		m[2] = &state.Session{PID: 2, StartedAt: time.Unix(100, 0)}
		m[3] = &state.Session{PID: 3, StartedAt: time.Unix(200, 0)}
	})

	snap := store.Snapshot()
	gotPIDs := []int{snap.Sessions[0].PID, snap.Sessions[1].PID, snap.Sessions[2].PID}
	want := []int{2, 3, 1} // by StartedAt 100, 200, 300
	for i := range want {
		if gotPIDs[i] != want[i] {
			t.Errorf("sorted PIDs = %v, want %v", gotPIDs, want)
			break
		}
	}
}

// §4.2 — equal StartedAt is broken by an ascending-PID tie-break, so order is
// deterministic across snapshots (fixed in 0.9; was previously unspecified —
// see docs/decisions.md). The positional focus selector relies on this.
func TestSnapshotEqualStartedAtSortsByPID(t *testing.T) {
	same := time.Unix(500, 0)
	store := state.New("")
	store.Apply(func(m map[int]*state.Session) {
		m[20] = &state.Session{PID: 20, StartedAt: same}
		m[10] = &state.Session{PID: 10, StartedAt: same}
		m[30] = &state.Session{PID: 30, StartedAt: same}
	})

	snap := store.Snapshot()
	got := []int{snap.Sessions[0].PID, snap.Sessions[1].PID, snap.Sessions[2].PID}
	want := []int{10, 20, 30}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("equal-StartedAt order = %v, want ascending PID %v", got, want)
		}
	}
}

// §4.3 Subscribe — a lagging subscriber's cap-4 buffer drops snapshots rather
// than blocking Apply. If broadcast blocked, this test would hang.
func TestSubscribeDropsWithoutBlocking(t *testing.T) {
	store := state.New("")
	ch, cancel := store.Subscribe()
	defer cancel()

	// Never drain ch. Ten applies must all return; the buffer caps at 4.
	for i := 0; i < 10; i++ {
		store.Apply(func(m map[int]*state.Session) {
			m[1] = &state.Session{PID: 1, StartedAt: time.Unix(int64(i), 0)}
		})
	}
	if n := len(ch); n > 4 {
		t.Errorf("buffered snapshots = %d, want <= 4 (cap)", n)
	}
}

// §4.3 Subscribe — cancel closes the channel.
func TestSubscribeCancelClosesChannel(t *testing.T) {
	store := state.New("")
	ch, cancel := store.Subscribe()
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("channel should be closed after cancel")
		}
	case <-time.After(time.Second):
		t.Error("read from cancelled channel blocked")
	}
}

// §4.4 persist — no-op on empty path (no file, no panic), and a successful
// persist leaves only state.json with no .state-*.json temp litter.
func TestPersistEmptyPathIsNoOp(t *testing.T) {
	store := state.New("")
	// Must not panic or error visibly.
	store.Apply(func(m map[int]*state.Session) { m[1] = &state.Session{PID: 1} })
}

func TestPersistLeavesNoTempLitter(t *testing.T) {
	dir := t.TempDir()
	store := state.New(filepath.Join(dir, "state.json"))
	store.Apply(func(m map[int]*state.Session) { m[1] = &state.Session{PID: 1} })

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "state.json" {
			t.Errorf("unexpected file %q in state dir (temp litter?)", e.Name())
		}
	}
}
