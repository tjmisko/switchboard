package state

import (
	"testing"
	"time"
)

func TestBroadcastRejectsLateOlderGeneration(t *testing.T) {
	store := New("")
	updates, cancel := store.Subscribe()
	defer cancel()
	newer := Snapshot{Sessions: []Session{{PID: 2, CWD: "/new", StartedAt: time.Unix(2, 0)}}}
	older := Snapshot{Sessions: []Session{{PID: 1, CWD: "/old", StartedAt: time.Unix(1, 0)}}}
	store.broadcast(newer, 2)
	store.broadcast(older, 1)
	if len(updates) != 1 {
		t.Fatalf("queued updates = %d, want only newest generation", len(updates))
	}
	if got := (<-updates).Snapshot.Sessions[0].PID; got != 2 {
		t.Fatalf("queued pid = %d, want 2", got)
	}
}
