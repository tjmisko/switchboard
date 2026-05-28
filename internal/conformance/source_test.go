//go:build linux

package conformance_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/conformance"
	"github.com/tjmisko/switchboard/internal/osproc"
	"github.com/tjmisko/switchboard/internal/proc"
	"github.com/tjmisko/switchboard/internal/testsupport"
)

// osprocAdapter wraps the Phase-1 internal/osproc Source behind the neutral
// conformance.Source interface (the only difference is the neutral struct type).
// The sway/x11/tmux/macOS backends adopt this same contract in later phases.
type osprocAdapter struct{ s osproc.Source }

func toNeutral(p osproc.Info) conformance.ProcInfo {
	return conformance.ProcInfo{PID: p.PID, PPID: p.PPID, Comm: p.Comm, Exe: p.Exe, CWD: p.CWD, TTY: p.TTY}
}

func (a osprocAdapter) Enumerate() ([]conformance.ProcInfo, error) {
	infos, err := a.s.Enumerate()
	if err != nil {
		return nil, err
	}
	out := make([]conformance.ProcInfo, 0, len(infos))
	for _, info := range infos {
		out = append(out, toNeutral(info))
	}
	return out, nil
}

func (a osprocAdapter) Read(pid int) (conformance.ProcInfo, error) {
	info, err := a.s.Read(pid)
	return toNeutral(info), err
}

func (a osprocAdapter) Watch(ctx context.Context, pid int, onDeath func()) error {
	return a.s.Watch(ctx, pid, onDeath)
}

func (a osprocAdapter) Stop(pid int) { a.s.Stop(pid) }

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
		Source: osprocAdapter{s: osproc.New()},
		IsGone: func(err error) bool { return errors.Is(err, osproc.ErrGone) },
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
