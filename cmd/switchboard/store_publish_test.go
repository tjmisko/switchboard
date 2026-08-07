package main

import (
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/state"
)

// writerDelay stands in for a genuinely slow Apply. The measured one on this box
// is the fanout Observer's first-sight seed: 1.81 s of archive decode per
// newly-seen session, paid again every time a session appears. 200 ms is enough
// to be unmistakable against scheduler noise and short enough that a red run of
// this test costs nothing.
const writerDelay = 200 * time.Millisecond

// TestShouldNotBlockAReaderWhileAWriterHoldsTheLock is the whole point of
// publish-and-swap, stated as the thing a user feels.
//
// The readers measureWorstReaderWait hammers with are not hypothetical: they are
// every waybar subscriber, every hook RPC from a live session, and
// `switchboard-ctl focus` — the chip click. Before this change all of them took
// the same RWMutex the writer holds, so a slow Apply froze the entire read side
// for its full duration. After it, Snapshot() is an atomic pointer load and a
// slow write can only delay the next WRITE.
//
// Verified failing against the pre-change tree, where it reported a reader
// blocked for ~200 ms — the injected delay in full, which is the production
// symptom reproduced in miniature.
func TestShouldNotBlockAReaderWhileAWriterHoldsTheLock(t *testing.T) {
	store := state.New("")
	store.Apply(func(m map[int]*state.Session) {
		m[9100] = &state.Session{PID: 9100, TTY: "/dev/pts/1", CWD: "/home/test", StartedAt: time.Now()}
	})

	got := measureWorstReaderWait(store, func() {
		store.Apply(func(m map[int]*state.Session) {
			time.Sleep(writerDelay)
			m[9100].Focused = true
		})
	})

	// A fraction of the injected delay rather than a wall-clock constant, so a
	// loaded box cannot turn a pass into a fail: the claim is "nothing like the
	// write's duration", not "under N microseconds".
	limit := writerDelay / 4
	if got > limit {
		t.Errorf("a store reader blocked for %v while an Apply held the lock for %v; want under %v — "+
			"readers are taking the writer's lock again", got, writerDelay, limit)
	}

	// The write still landed. A reader that never blocks is worthless if it is
	// reading a snapshot the writer never published.
	if !store.Snapshot().Sessions[0].Focused {
		t.Error("the slow Apply's mutation is not visible to a reader after it returned")
	}
}
