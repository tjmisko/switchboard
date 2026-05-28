//go:build linux

package conformance_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/conformance"
	"github.com/tjmisko/switchboard/internal/proc"
	"github.com/tjmisko/switchboard/internal/procwatch"
	"github.com/tjmisko/switchboard/internal/testsupport"
)

// osprocSource is the thin adapter wrapping the existing Linux /proc + pidfd
// concretes behind the neutral conformance.Source interface. Phase 1's
// internal/osproc linux backend replaces it and reuses RunSourceContract.
type osprocSource struct{ w *procwatch.Watcher }

func newOsprocSource() *osprocSource { return &osprocSource{w: procwatch.New()} }

func toNeutral(p proc.Info) conformance.ProcInfo {
	return conformance.ProcInfo{PID: p.PID, PPID: p.PPID, Comm: p.Comm, Exe: p.Exe, CWD: p.CWD, TTY: p.TTY}
}

func (s *osprocSource) Enumerate() ([]conformance.ProcInfo, error) {
	pids, err := proc.AllPIDs()
	if err != nil {
		return nil, err
	}
	out := make([]conformance.ProcInfo, 0, len(pids))
	for _, pid := range pids {
		info, err := proc.Read(pid)
		if err != nil {
			continue
		}
		out = append(out, toNeutral(info))
	}
	return out, nil
}

func (s *osprocSource) Read(pid int) (conformance.ProcInfo, error) {
	info, err := proc.Read(pid)
	return toNeutral(info), err
}

func (s *osprocSource) Watch(ctx context.Context, pid int, onDeath func()) error {
	return s.w.Watch(ctx, pid, onDeath)
}

func (s *osprocSource) Stop(pid int) { s.w.Stop(pid) }

// childRegistry tracks spawned testsupport children by pid so the neutral
// KillChild hook can kill AND reap (reaping is what makes a dead pid read as
// gone rather than lingering as a zombie in /proc).
type childRegistry struct {
	mu sync.Mutex
	m  map[int]*testsupport.Child
}

func (r *childRegistry) add(c *testsupport.Child) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[c.PID] = c
	return c.PID
}

func (r *childRegistry) kill(t *testing.T, pid int) {
	r.mu.Lock()
	c := r.m[pid]
	r.mu.Unlock()
	if c == nil {
		t.Fatalf("KillChild: unknown pid %d", pid)
	}
	c.Kill(t)
}

func TestOsprocSourceConformance(t *testing.T) {
	reg := &childRegistry{m: map[int]*testsupport.Child{}}

	conformance.RunSourceContract(t, conformance.SourceFixture{
		Source: newOsprocSource(),
		IsGone: func(err error) bool { return errors.Is(err, proc.ErrGone) },
		SpawnTTYChild: func(t *testing.T) int {
			return reg.add(testsupport.SpawnTTYChild(t, 60*time.Second))
		},
		SpawnBareChild: func(t *testing.T) int {
			return reg.add(testsupport.SpawnSleep(t, 60*time.Second))
		},
		KillChild: reg.kill,
		MaskedExePID: func() (int, bool) {
			// pid 2 is kthreadd on Linux: comm is readable but exe/cwd are masked.
			if _, err := proc.Read(2); err != nil {
				return 0, false
			}
			return 2, true
		},
	})
}
