// Package procwatch turns a PID into a one-shot "is dead yet?" signal using
// pidfd_open(2). One goroutine per watched PID polls its pidfd; POLLIN fires
// when the kernel makes the process a zombie, which happens regardless of how
// it died (Ctrl+C, /exit, kill -9, OOM, terminal hangup).
package procwatch

import (
	"context"
	"errors"
	"sync"

	"golang.org/x/sys/unix"
)

type Watcher struct {
	mu      sync.Mutex
	watched map[int]context.CancelFunc
}

func New() *Watcher {
	return &Watcher{watched: make(map[int]context.CancelFunc)}
}

// Watch starts polling pid's pidfd. onDeath is called exactly once, from a
// background goroutine, when the kernel marks the process dead.
//
// Calling Watch twice for the same pid is a no-op (second call returns nil
// without scheduling another watcher).
func (w *Watcher) Watch(parent context.Context, pid int, onDeath func()) error {
	w.mu.Lock()
	if _, dup := w.watched[pid]; dup {
		w.mu.Unlock()
		return nil
	}
	pidfd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		w.mu.Unlock()
		if errors.Is(err, unix.ESRCH) {
			go onDeath()
			return nil
		}
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	w.watched[pid] = cancel
	w.mu.Unlock()

	go func() {
		defer unix.Close(pidfd)
		defer func() {
			w.mu.Lock()
			delete(w.watched, pid)
			w.mu.Unlock()
		}()
		const tick = 1000 // ms — keeps Stop responsive without busy-looping
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
			if pfd[0].Revents&unix.POLLIN != 0 {
				onDeath()
				return
			}
		}
	}()
	return nil
}

// Stop cancels the watcher for pid if one exists.
func (w *Watcher) Stop(pid int) {
	w.mu.Lock()
	cancel, ok := w.watched[pid]
	w.mu.Unlock()
	if ok {
		cancel()
	}
}

// Watched returns the PIDs we are currently watching.
func (w *Watcher) Watched() []int {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]int, 0, len(w.watched))
	for pid := range w.watched {
		out = append(out, pid)
	}
	return out
}
