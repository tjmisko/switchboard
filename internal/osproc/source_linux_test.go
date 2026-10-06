//go:build linux

package osproc

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/tjmisko/switchboard/internal/proc"
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

// liveLifetime reads pid's current lifetime through s, failing the test when it
// has no birth token: every Watch below needs a verified lifetime.
func liveLifetime(t *testing.T, s *linuxSource, pid int) Lifetime {
	t.Helper()
	info, err := s.Read(pid)
	if err != nil {
		t.Fatalf("Read(%d): %v", pid, err)
	}
	if !info.Lifetime().Verified() {
		t.Fatalf("Read(%d) has no birth token", pid)
	}
	return info.Lifetime()
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
	if err := s.Watch(ctx, liveLifetime(t, s, child.PID), func() { fired <- struct{}{} }); err != nil {
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
	if err := s.Watch(ctx, liveLifetime(t, s, child.PID), func() { fired <- struct{}{} }); err != nil {
		t.Fatalf("first Watch: %v", err)
	}
	if err := s.Watch(ctx, liveLifetime(t, s, child.PID), func() { fired <- struct{}{} }); err != nil {
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
	if err := s.Watch(ctx, liveLifetime(t, s, child.PID), func() { fired <- struct{}{} }); err != nil {
		t.Fatalf("Watch: %v", err)
	}

	s.Stop(liveLifetime(t, s, child.PID))

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
	if err := s.Watch(ctx, Lifetime{PID: testsupport.DeadPID(), Birth: "boot:1"}, func() { fired <- struct{}{} }); err != nil {
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

// requirePidfd skips when the kernel has no pidfd_open(2) (pre-5.3, or blocked
// by a seccomp profile): there is no watcher to exercise.
func requirePidfd(t *testing.T) {
	t.Helper()
	fd, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		t.Skipf("pidfd_open unavailable: %v", err)
	}
	unix.Close(fd)
}

// A pid that is watched again while its previous watcher's onDeath is still
// running must get a watcher of its own. The daemon's death callback waits on
// the store lock, and the scanner can re-appear the same pid in that window;
// if the dying watcher still held the slot, the re-registration was swallowed
// as a duplicate and the new lifetime was never watched.
func TestWatchAgainDuringDeathCallback(t *testing.T) {
	rows := []struct {
		name string
		// rewatchInside re-registers from inside onDeath; otherwise the test
		// goroutine re-registers while onDeath is blocked.
		rewatchInside bool
	}{
		{name: "should register a new watcher when the same pid is watched again from within its death callback", rewatchInside: true},
		{name: "should register a new watcher when the same pid is watched again while its death callback is still running", rewatchInside: false},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			requirePidfd(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			child := testsupport.SpawnSleep(t, 60*time.Second)
			s := newLinuxSource()
			lifetime := liveLifetime(t, s, child.PID)

			second := make(chan struct{}, 4)
			rewatchErr := make(chan error, 1)
			firstEntered := make(chan struct{})
			releaseFirst := make(chan struct{})
			onFirstDeath := func() {
				if row.rewatchInside {
					rewatchErr <- s.Watch(ctx, lifetime, func() { second <- struct{}{} })
					return
				}
				close(firstEntered)
				<-releaseFirst
			}
			if err := s.Watch(ctx, lifetime, onFirstDeath); err != nil {
				t.Fatalf("Watch: %v", err)
			}

			child.Kill(t)

			if !row.rewatchInside {
				select {
				case <-firstEntered:
				case <-time.After(3 * time.Second):
					t.Fatal("first onDeath did not fire within 3s of kill")
				}
				rewatchErr <- s.Watch(ctx, lifetime, func() { second <- struct{}{} })
				close(releaseFirst)
			}

			select {
			case err := <-rewatchErr:
				if err != nil {
					t.Fatalf("re-Watch: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("first onDeath did not fire within 3s of kill")
			}
			// The pid is dead (a zombie or reaped), so a registered watcher reports
			// it at once; a swallowed one never does.
			select {
			case <-second:
			case <-time.After(3 * time.Second):
				t.Fatal("re-Watch registered nothing: the dying watcher swallowed it as a duplicate")
			}
			select {
			case <-second:
				t.Fatal("re-Watch's onDeath fired more than once")
			case <-time.After(300 * time.Millisecond):
			}
			waitWatchedEmpty(t, s)
		})
	}
}

// #97: Watch is keyed by lifetime and proves its pidfd refers to that
// lifetime by re-reading the birth token after the open. Each row stages,
// with a live child and no timing race, one shape of a pid that changed hands
// before or around the open.
func TestWatchShouldBeKeyedByLifetimeAndRecheckedAfterOpen(t *testing.T) {
	requirePidfd(t)

	t.Run("should report the watched lifetime dead at once when its pid already belongs to another", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		child := testsupport.SpawnSleep(t, 60*time.Second)
		s := newLinuxSource()
		stale := Lifetime{PID: child.PID, Birth: "boot:1"} // the lifetime the scanner read, since replaced by child

		fired := make(chan struct{}, 4)
		if err := s.Watch(ctx, stale, func() { fired <- struct{}{} }); err != nil {
			t.Fatalf("Watch: %v", err)
		}
		select {
		case <-fired:
		case <-time.After(3 * time.Second):
			t.Fatal("onDeath did not fire: the watcher is watching the replacement")
		}
		if watching(s, child.PID) {
			t.Error("the replacement's pid is watched on the stale lifetime's behalf")
		}
	})

	t.Run("should report the watched lifetime dead when its pid is gone by the re-read", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		child := testsupport.SpawnSleep(t, 60*time.Second)
		s := newLinuxSource()
		lifetime := liveLifetime(t, s, child.PID)
		s.readBirth = func(int) (string, error) { return "", proc.ErrGone }

		fired := make(chan struct{}, 4)
		if err := s.Watch(ctx, lifetime, func() { fired <- struct{}{} }); err != nil {
			t.Fatalf("Watch: %v", err)
		}
		select {
		case <-fired:
		case <-time.After(3 * time.Second):
			t.Fatal("onDeath did not fire for a lifetime gone at the re-read")
		}
	})

	t.Run("should refuse without firing when the re-read cannot verify the lifetime", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		child := testsupport.SpawnSleep(t, 60*time.Second)
		s := newLinuxSource()
		lifetime := liveLifetime(t, s, child.PID)
		s.readBirth = func(int) (string, error) { return "", nil }

		fired := make(chan struct{}, 4)
		if err := s.Watch(ctx, lifetime, func() { fired <- struct{}{} }); !errors.Is(err, ErrUnverifiedLifetime) {
			t.Fatalf("Watch err = %v, want ErrUnverifiedLifetime", err)
		}
		select {
		case <-fired:
			t.Fatal("onDeath fired for a live process whose token was merely unreadable")
		case <-time.After(300 * time.Millisecond):
		}
	})

	t.Run("should refuse an unverified lifetime when it has no birth token", func(t *testing.T) {
		s := newLinuxSource()
		child := testsupport.SpawnSleep(t, 60*time.Second)
		err := s.Watch(context.Background(), Lifetime{PID: child.PID}, func() { t.Error("onDeath fired for a refused watch") })
		if !errors.Is(err, ErrUnverifiedLifetime) {
			t.Fatalf("Watch err = %v, want ErrUnverifiedLifetime", err)
		}
		if watching(s, child.PID) {
			t.Error("an unverified lifetime entered the watched set")
		}
	})

	t.Run("should keep a lifetime's watcher when Stop names another lifetime of the pid", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		child := testsupport.SpawnSleep(t, 60*time.Second)
		s := newLinuxSource()
		lifetime := liveLifetime(t, s, child.PID)

		fired := make(chan struct{}, 4)
		if err := s.Watch(ctx, lifetime, func() { fired <- struct{}{} }); err != nil {
			t.Fatalf("Watch: %v", err)
		}
		s.Stop(Lifetime{PID: child.PID, Birth: "boot:1"})
		child.Kill(t)
		select {
		case <-fired:
		case <-time.After(3 * time.Second):
			t.Fatal("onDeath did not fire: a Stop for another lifetime cancelled this one")
		}
	})
}
