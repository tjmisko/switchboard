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
	return &linuxSource{watched: make(map[int]context.CancelFunc)}
}

// linuxSource reads process metadata from /proc and watches deaths with
// pidfd_open(2): one goroutine per watched pid polls its pidfd, and the kernel
// makes the fd readable when the process becomes a zombie — independent of how
// it died (Ctrl+C, /exit, kill -9, OOM, terminal hangup). The pidfd machinery
// is the former internal/procwatch, absorbed into the seam in Phase 1.1.
type linuxSource struct {
	mu      sync.Mutex
	watched map[int]context.CancelFunc
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
		out = append(out, fromProc(info))
	}
	return out, nil
}

func (s *linuxSource) Read(pid int) (Info, error) {
	info, err := proc.Read(pid)
	if errors.Is(err, proc.ErrGone) {
		return Info{PID: pid}, ErrGone
	}
	return fromProc(info), err
}

func fromProc(p proc.Info) Info {
	return Info{PID: p.PID, PPID: p.PPID, Comm: p.Comm, Exe: p.Exe, CWD: p.CWD, TTY: p.TTY}
}

// Watch starts polling pid's pidfd. onDeath is called exactly once, from a
// background goroutine, when the kernel marks the process dead. A duplicate
// Watch for the same pid returns nil without scheduling a second watcher.
func (s *linuxSource) Watch(parent context.Context, pid int, onDeath func()) error {
	s.mu.Lock()
	if _, dup := s.watched[pid]; dup {
		s.mu.Unlock()
		return nil
	}
	pidfd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		s.mu.Unlock()
		if errors.Is(err, unix.ESRCH) {
			go onDeath() // already dead
			return nil
		}
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	s.watched[pid] = cancel
	s.mu.Unlock()

	go func() {
		defer unix.Close(pidfd)
		defer func() {
			s.mu.Lock()
			delete(s.watched, pid)
			s.mu.Unlock()
		}()
		const tick = 1000 // ms — keeps Stop responsive without busy-looping
		// pidfd readability (POLLIN) signals exit. POLLERR/POLLHUP/POLLNVAL are
		// output-only conditions the kernel reports unconditionally; treating
		// them as death too closes the Phase-0 ⚠ gap (decisions.md #11) where a
		// POLLERR without POLLIN would otherwise spin the loop forever.
		const deathRevents = unix.POLLIN | unix.POLLERR | unix.POLLHUP | unix.POLLNVAL
		for {
			if ctx.Err() != nil {
				return
			}
			pfd := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}}
			n, err := unix.Poll(pfd, tick)
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if err != nil {
				return
			}
			if n == 0 {
				continue // timeout — re-check ctx and loop
			}
			if pfd[0].Revents&deathRevents != 0 {
				onDeath()
				return
			}
		}
	}()
	return nil
}

func (s *linuxSource) Stop(pid int) {
	s.mu.Lock()
	cancel, ok := s.watched[pid]
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
	for pid := range s.watched {
		out = append(out, pid)
	}
	return out
}
