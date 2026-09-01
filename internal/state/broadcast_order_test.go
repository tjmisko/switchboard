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
	updates, cancel := store.Subscribe()
	defer cancel()
	unlocked := make(chan uint64, 2)
	store.afterBroadcastUnlock = func(gen uint64) { unlocked <- gen }
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

	// Disk generation 1 is still blocked, but generation 2 must already be the
	// live frame: persistence ordering must not enter the semantic-delivery path.
	waitForUnlockedGeneration(t, unlocked, 2)
	if got := (<-updates).Snapshot.Sessions[0].PID; got != 2 {
		t.Fatalf("live frame PID = %d, want newest 2", got)
	}
	if !store.broadcastMu.TryLock() {
		close(releaseFirst)
		<-firstDone
		<-secondDone
		t.Fatal("publication sequencer is still held across disk I/O")
	}
	store.broadcastMu.Unlock()
	close(releaseFirst)
	<-firstDone
	<-secondDone

	if first, second := <-persisted, <-persisted; first != 1 || second != 2 {
		t.Fatalf("persistence order = [%d %d], want [1 2]", first, second)
	}
}

func TestPersistenceCoalescesQueuedFullReplacementsToNewest(t *testing.T) {
	store := New("unused")
	unlocked := make(chan uint64, 3)
	store.afterBroadcastUnlock = func(gen uint64) { unlocked <- gen }
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	persisted := make(chan int, 3)
	store.persistSnapshot = func(snap Snapshot) error {
		pid := snap.Sessions[0].PID
		if pid == 1 {
			close(firstEntered)
			<-releaseFirst
		}
		persisted <- pid
		return nil
	}

	done := make([]chan struct{}, 3)
	for i := range done {
		done[i] = make(chan struct{})
	}
	go func() { _ = store.broadcast(snapshotWithPID(1), 1); close(done[0]) }()
	<-firstEntered
	go func() { _ = store.broadcast(snapshotWithPID(2), 2); close(done[1]) }()
	waitForUnlockedGeneration(t, unlocked, 2)
	go func() { _ = store.broadcast(snapshotWithPID(3), 3); close(done[2]) }()
	waitForUnlockedGeneration(t, unlocked, 3)
	close(releaseFirst)
	for _, ch := range done {
		<-ch
	}

	if len(persisted) != 2 {
		t.Fatalf("physical persistence writes = %d, want older active + newest queued", len(persisted))
	}
	if first, latest := <-persisted, <-persisted; first != 1 || latest != 3 {
		t.Fatalf("persistence order = [%d %d], want [1 3]", first, latest)
	}
}

func snapshotWithPID(pid int) Snapshot {
	return Snapshot{Sessions: []Session{{PID: pid, StartedAt: time.Unix(int64(pid), 0)}}}
}

func waitForUnlockedGeneration(t *testing.T, unlocked <-chan uint64, want uint64) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case gen := <-unlocked:
			if gen == want {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for unlocked generation %d", want)
		}
	}
}
