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

func TestBroadcastSerializesPersistenceInGenerationOrder(t *testing.T) {
	store := New("unused")
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	persisted := make(chan int, 2)
	store.persistSnapshot = func(snap Snapshot) error {
		pid := snap.Sessions[0].PID
		if pid == 1 {
			close(firstEntered)
			<-releaseFirst
		}
		persisted <- pid
		return nil
	}

	older := Snapshot{Sessions: []Session{{PID: 1, CWD: "/old", StartedAt: time.Unix(1, 0)}}}
	newer := Snapshot{Sessions: []Session{{PID: 2, CWD: "/new", StartedAt: time.Unix(2, 0)}}}
	firstDone := make(chan struct{})
	go func() {
		_ = store.broadcast(older, 1)
		close(firstDone)
	}()
	<-firstEntered
	secondDone := make(chan struct{})
	go func() {
		_ = store.broadcast(newer, 2)
		close(secondDone)
	}()

	select {
	case <-secondDone:
		t.Fatal("newer generation persisted while the older publication was still in flight")
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseFirst)
	<-firstDone
	<-secondDone

	if first, second := <-persisted, <-persisted; first != 1 || second != 2 {
		t.Fatalf("persistence order = [%d %d], want [1 2]", first, second)
	}
}
