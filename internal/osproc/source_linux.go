//go:build linux

package osproc

import (
	"context"
	"errors"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/tjmisko/switchboard/internal/proc"
)

func newSource() Source { return newLinuxSource() }

// newLinuxSource builds the concrete Linux source. Tests use it directly to
// reach Watched(), which is introspection the Source interface does not expose.
func newLinuxSource() *linuxSource {
	return &linuxSource{watched: make(map[Lifetime]context.CancelFunc), readBirth: proc.Birth}
}

// linuxSource reads process metadata from /proc and watches deaths with
// pidfd_open(2): one goroutine per watched pid polls its pidfd, and the kernel
// makes the fd readable when the process becomes a zombie — independent of how
// it died (Ctrl+C, /exit, kill -9, OOM, terminal hangup). The pidfd machinery
// is the former internal/procwatch, absorbed into the seam in Phase 1.1.
type linuxSource struct {
	mu      sync.Mutex
	watched map[Lifetime]context.CancelFunc
	// readBirth re-reads a pid's birth token after its pidfd is open. Tests
	// swap it to stage a pid that another lifetime took before the open.
	readBirth func(pid int) (string, error)
}

func (s *linuxSource) Enumerate() ([]Info, error) {
	pids, err := proc.AllPIDs()
	if err != nil {
		return nil, err
	}
	out := make([]Info, 0, len(pids))
	for _, pid := range pids {
		info, err := proc.Read(pid)
		if err != nil {
			continue // disappeared mid-scan — benign
		}
		out = append(out, FromProc(info))
	}
	return out, nil
}

func (s *linuxSource) Read(pid int) (Info, error) {
	info, err := proc.Read(pid)
	if errors.Is(err, proc.ErrGone) {
		return Info{PID: pid}, ErrGone
	}
	return FromProc(info), err
}

// AllPIDs lists the visible pids cheaply — one getdents over /proc, with no
// per-pid exe/cwd/tty reads. It is the fast-path the discovery scanner uses
// (via an optional-interface upgrade) to Read only pids it has not seen before,
// preserving the "enumerate cheaply, Read the unseen" hot path. It is NOT part
// of the neutral Source contract; a Source that omits it is driven from
// Enumerate instead.
func (s *linuxSource) AllPIDs() ([]int, error) { return proc.AllPIDs() }

// Watch starts polling the pidfd of one process lifetime. onDeath is called
// exactly once, from a background goroutine, when the kernel marks that
// lifetime dead. A duplicate Watch for the same lifetime returns nil without
// scheduling a second watcher.
//
// pidfd_open(2) opens whichever process holds the pid at that instant, which
// may be a replacement if the watched lifetime died after the scanner read
// it. So the birth token is re-read AFTER the open. A pid cannot leave a
// process and come back to it, so if the pid still carries the watched birth
// now, it carried it at the open too: the pidfd refers to the watched
// lifetime. Gone or another birth means the watched lifetime is already over,
// and onDeath fires at once; an unreadable token is refused as unverified.
//
// The watcher leaves the watched set BEFORE it calls onDeath, so a Watch made
// from inside the callback, or while it is still running, registers a fresh
// watcher instead of being swallowed as a duplicate of the dying one.
func (s *linuxSource) Watch(parent context.Context, lifetime Lifetime, onDeath func()) error {
	if !lifetime.Verified() {
		return ErrUnverifiedLifetime
	}
	s.mu.Lock()
	if _, dup := s.watched[lifetime]; dup {
		s.mu.Unlock()
		return nil
	}
	pidfd, err := unix.PidfdOpen(lifetime.PID, 0)
	if err != nil {
		s.mu.Unlock()
		if errors.Is(err, unix.ESRCH) {
			go onDeath() // already dead
			return nil
		}
		return err
	}
	birth, err := s.readBirth(lifetime.PID)
	if err != nil && !errors.Is(err, proc.ErrGone) {
		s.mu.Unlock()
		unix.Close(pidfd)
		return err
	}
	if err == nil && CompareBirth(birth, lifetime.Birth) == BirthUnverified {
		// The pid is live but its token unreadable now: which process the pidfd
		// holds is unproven, and reporting a death here could end a live
		// session. Refuse, and leave the lifetime to the liveness sweep.
		s.mu.Unlock()
		unix.Close(pidfd)
		return ErrUnverifiedLifetime
	}
	if err != nil || CompareBirth(birth, lifetime.Birth) == BirthDifferent {
		// Gone, or the pid now belongs to another lifetime: the watched one is
		// already dead.
		s.mu.Unlock()
		unix.Close(pidfd)
		go onDeath()
		return nil
	}
	ctx, cancel := context.WithCancel(parent)
	s.watched[lifetime] = cancel
	s.mu.Unlock()

	go func() {
		defer unix.Close(pidfd)
		died := pollUntilDeath(ctx, pidfd)
		// Only this goroutine removes its own entry, and Watch adds one only when
		// the slot is empty, so the slot is still ours to clear here.
		s.mu.Lock()
		delete(s.watched, lifetime)
		s.mu.Unlock()
		if died {
			onDeath()
		}
	}()
	return nil
}

// pollUntilDeath blocks until the kernel marks pidfd's process dead (true) or
// ctx is cancelled or poll fails outright (false).
func pollUntilDeath(ctx context.Context, pidfd int) bool {
	const tick = 1000 // ms — keeps Stop responsive without busy-looping
	// pidfd readability (POLLIN) signals exit. POLLERR/POLLHUP/POLLNVAL are
	// output-only conditions the kernel reports unconditionally; treating
	// them as death too closes the Phase-0 ⚠ gap (decisions.md #11) where a
	// POLLERR without POLLIN would otherwise spin the loop forever.
	const deathRevents = unix.POLLIN | unix.POLLERR | unix.POLLHUP | unix.POLLNVAL
	for {
		if ctx.Err() != nil {
			return false
		}
		pfd := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}}
		n, err := unix.Poll(pfd, tick)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return false
		}
		if n == 0 {
			continue // timeout — re-check ctx and loop
		}
		if pfd[0].Revents&deathRevents != 0 {
			return true
		}
	}
}

func (s *linuxSource) Stop(lifetime Lifetime) {
	s.mu.Lock()
	cancel, ok := s.watched[lifetime]
	s.mu.Unlock()
	if ok {
		cancel()
	}
}

// Watched returns the PIDs currently being watched. Test/introspection only.
func (s *linuxSource) Watched() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int, 0, len(s.watched))
	for lifetime := range s.watched {
		out = append(out, lifetime.PID)
	}
	return out
}
