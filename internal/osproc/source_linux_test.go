//go:build linux

package osproc

import (
	"context"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/testsupport"
)

// §9 death semantics, exercised against real short-lived children. These were
// internal/procwatch's tests; the pidfd machinery moved into the linux Source
// in Phase 1.1, so they now drive *linuxSource directly.
//
// Two behaviors are documented gaps, not assertions (see docs/decisions.md):
//   - EINTR mid-poll must not fire onDeath (the loop continues). It cannot be
//     triggered deterministically from a test, so it is covered by inspection.
//   - The Phase-0 ⚠ POLLERR-without-POLLIN spin (decisions.md #11) is now FIXED:
//     POLLERR/POLLHUP/POLLNVAL are treated as death. Hard to trigger from a
//     test (pidfd delivers POLLIN on exit), so it is covered by inspection.

// watching reports whether s currently watches pid.
func watching(s *linuxSource, pid int) bool {
	for _, p := range s.Watched() {
		if p == pid {
			return true
		}
	}
	return false
}

// waitWatchedEmpty waits (briefly) for the watched set to drain — the proxy for
// "the per-pid goroutine returned and cleaned up", i.e. no leak. The poll tick
// is 1s, so allow generous slack.
func waitWatchedEmpty(t *testing.T, s *linuxSource) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(s.Watched()) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("watched set did not drain (goroutine leak?): %v", s.Watched())
}

// onDeath fires exactly once when a watched child is killed, and the source
// cleans itself up afterward.
func TestWatchFiresOnDeathExactlyOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	child := testsupport.SpawnSleep(t, 60*time.Second)

	fired := make(chan struct{}, 4)
	s := newLinuxSource()
	if err := s.Watch(ctx, child.PID, func() { fired <- struct{}{} }); err != nil {
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
	waitWatchedEmpty(t, s)
}

// A duplicate Watch for the same pid is a no-op: no second fd, no second
// goroutine, only the first callback ever fires.
func TestDuplicateWatchIsNoOp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	child := testsupport.SpawnSleep(t, 60*time.Second)

	fired := make(chan struct{}, 8)
	s := newLinuxSource()
	if err := s.Watch(ctx, child.PID, func() { fired <- struct{}{} }); err != nil {
		t.Fatalf("first Watch: %v", err)
	}
	if err := s.Watch(ctx, child.PID, func() { fired <- struct{}{} }); err != nil {
		t.Fatalf("duplicate Watch returned error: %v", err)
	}
	if n := len(s.Watched()); n != 1 {
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
	s := newLinuxSource()
	if err := s.Watch(ctx, child.PID, func() { fired <- struct{}{} }); err != nil {
		t.Fatalf("Watch: %v", err)
	}

	s.Stop(child.PID)

	select {
	case <-fired:
		t.Fatal("onDeath fired on Stop — must only fire on actual death")
	case <-time.After(300 * time.Millisecond):
	}
	waitWatchedEmpty(t, s)
}

// Watching an already-dead pid (PidfdOpen → ESRCH) fires onDeath immediately
// and never enters the watched set. DeadPID is well above pid_max, so there is
// no risk of the kernel having recycled it onto a live process.
func TestWatchAlreadyDeadFiresImmediately(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fired := make(chan struct{}, 4)
	s := newLinuxSource()
	if err := s.Watch(ctx, testsupport.DeadPID(), func() { fired <- struct{}{} }); err != nil {
		t.Fatalf("Watch(dead): %v", err)
	}

	select {
	case <-fired:
	case <-time.After(3 * time.Second):
		t.Fatal("onDeath did not fire for an already-dead pid")
	}
	if watching(s, testsupport.DeadPID()) {
		t.Error("ESRCH pid must not be in the watched set")
	}
}
