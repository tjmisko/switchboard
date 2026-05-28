package procwatch_test

import (
	"context"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/procwatch"
	"github.com/tjmisko/switchboard/internal/testsupport"
)

// §9 procwatch death semantics, exercised against real short-lived children.
//
// Two behaviors are documented gaps, not assertions (see docs/decisions.md):
//   - EINTR mid-poll must not fire onDeath (the loop continues). It cannot be
//     triggered deterministically from a test, so it is covered by inspection.
//   - ⚠ a POLLERR revent without POLLIN can spin without ever firing onDeath.
//     Known gap, intentionally unfixed in Phase 0.

// watching reports whether w currently watches pid.
func watching(w *procwatch.Watcher, pid int) bool {
	for _, p := range w.Watched() {
		if p == pid {
			return true
		}
	}
	return false
}

// waitWatchedEmpty waits (briefly) for the watched set to drain — the proxy for
// "the per-pid goroutine returned and cleaned up", i.e. no leak. The poll tick
// is 1s, so allow generous slack.
func waitWatchedEmpty(t *testing.T, w *procwatch.Watcher) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(w.Watched()) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("watched set did not drain (goroutine leak?): %v", w.Watched())
}

// onDeath fires exactly once when a watched child is killed, and the watcher
// cleans itself up afterward.
func TestWatchFiresOnDeathExactlyOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	child := testsupport.SpawnSleep(t, 60*time.Second)

	fired := make(chan struct{}, 4)
	w := procwatch.New()
	if err := w.Watch(ctx, child.PID, func() { fired <- struct{}{} }); err != nil {
		t.Fatalf("Watch: %v", err)
	}

	child.Kill(t)

	select {
	case <-fired:
	case <-time.After(3 * time.Second):
		t.Fatal("onDeath did not fire within 3s of kill")
	}
	select {
	case <-fired:
		t.Fatal("onDeath fired more than once")
	case <-time.After(300 * time.Millisecond):
	}

	// §9 Watched() excludes the exited PID; the goroutine cleaned up (no leak).
	waitWatchedEmpty(t, w)
}

// A duplicate Watch for the same pid is a no-op: no second fd, no second
// goroutine, only the first callback ever fires.
func TestDuplicateWatchIsNoOp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	child := testsupport.SpawnSleep(t, 60*time.Second)

	fired := make(chan struct{}, 8)
	w := procwatch.New()
	if err := w.Watch(ctx, child.PID, func() { fired <- struct{}{} }); err != nil {
		t.Fatalf("first Watch: %v", err)
	}
	if err := w.Watch(ctx, child.PID, func() { fired <- struct{}{} }); err != nil {
		t.Fatalf("duplicate Watch returned error: %v", err)
	}
	if n := len(w.Watched()); n != 1 {
		t.Fatalf("Watched() = %d, want 1 (duplicate must not add a watcher)", n)
	}

	child.Kill(t)

	select {
	case <-fired:
	case <-time.After(3 * time.Second):
		t.Fatal("onDeath did not fire")
	}
	select {
	case <-fired:
		t.Fatal("onDeath fired twice — duplicate Watch scheduled a second watcher")
	case <-time.After(300 * time.Millisecond):
	}
}

// Stop cancels the watcher without firing onDeath (the process is still alive).
func TestStopCancelsWithoutFiringOnDeath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	child := testsupport.SpawnSleep(t, 60*time.Second)

	fired := make(chan struct{}, 4)
	w := procwatch.New()
	if err := w.Watch(ctx, child.PID, func() { fired <- struct{}{} }); err != nil {
		t.Fatalf("Watch: %v", err)
	}

	w.Stop(child.PID)

	select {
	case <-fired:
		t.Fatal("onDeath fired on Stop — must only fire on actual death")
	case <-time.After(300 * time.Millisecond):
	}
	waitWatchedEmpty(t, w)
}

// Watching an already-dead pid (PidfdOpen → ESRCH) fires onDeath immediately
// and never enters the watched set. DeadPID is well above pid_max, so there is
// no risk of the kernel having recycled it onto a live process.
func TestWatchAlreadyDeadFiresImmediately(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fired := make(chan struct{}, 4)
	w := procwatch.New()
	if err := w.Watch(ctx, testsupport.DeadPID(), func() { fired <- struct{}{} }); err != nil {
		t.Fatalf("Watch(dead): %v", err)
	}

	select {
	case <-fired:
	case <-time.After(3 * time.Second):
		t.Fatal("onDeath did not fire for an already-dead pid")
	}
	if watching(w, testsupport.DeadPID()) {
		t.Error("ESRCH pid must not be in the watched set")
	}
}
