// Package discovery scans /proc for claude processes. We poll once a second
// rather than subscribing to netlink CN_PROC because /proc scans are cheap
// (~200-500 procfs entries, kernel-side memory) and avoid needing
// CAP_NET_ADMIN. Latency is bounded by the tick interval.
package discovery

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/tjmisko/switchboard/internal/proc"
)

// IsClaude returns true if the given /proc snapshot looks like a Claude Code
// process. We match on comm == "claude" AND exe under ~/.local/share/claude/
// (handles both the released binary and dev builds installed elsewhere). The
// exe check is cheap insurance against name collisions.
func IsClaude(p proc.Info) bool {
	if p.Comm != "claude" {
		return false
	}
	if p.Exe == "" {
		return true // benefit of the doubt — kernel masked exe (rare)
	}
	return strings.Contains(p.Exe, "/claude/")
}

type Scanner struct {
	mu   sync.Mutex
	seen map[int]struct{}
}

func New() *Scanner {
	return &Scanner{seen: make(map[int]struct{})}
}

// Run polls /proc every interval and invokes onAppeared for any new claude
// PID. Returns when ctx is cancelled. Death is *not* reported here — that's
// the procwatch package's job, fed by pidfds.
func (s *Scanner) Run(ctx context.Context, interval time.Duration, onAppeared func(proc.Info)) error {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	s.scanOnce(onAppeared)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			s.scanOnce(onAppeared)
		}
	}
}

// Forget drops a PID from the seen set so the next scan can re-fire if the
// kernel ever recycled the same PID for a fresh claude process. Call this
// from procwatch's death callback.
func (s *Scanner) Forget(pid int) {
	s.mu.Lock()
	delete(s.seen, pid)
	s.mu.Unlock()
}

func (s *Scanner) scanOnce(onAppeared func(proc.Info)) {
	pids, err := proc.AllPIDs()
	if err != nil {
		return
	}
	for _, pid := range pids {
		s.mu.Lock()
		_, known := s.seen[pid]
		s.mu.Unlock()
		if known {
			continue
		}
		info, err := proc.Read(pid)
		if err != nil {
			continue
		}
		if !IsClaude(info) {
			continue
		}
		s.mu.Lock()
		s.seen[pid] = struct{}{}
		s.mu.Unlock()
		onAppeared(info)
	}
}
