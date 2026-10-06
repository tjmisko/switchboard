package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/herdr"
	"github.com/tjmisko/switchboard/internal/osproc"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/terminal"
)

// infoProcs is an osproc.Source that returns a programmable snapshot per pid;
// unknown pids are gone.
type infoProcs struct {
	mu    sync.Mutex
	infos map[int]osproc.Info
}

func (p *infoProcs) set(info osproc.Info) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.infos[info.PID] = info
}

func (p *infoProcs) kill(pid int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.infos, pid)
}

func (p *infoProcs) Read(pid int) (osproc.Info, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	info, ok := p.infos[pid]
	if !ok {
		return osproc.Info{PID: pid}, osproc.ErrGone
	}
	return info, nil
}
func (p *infoProcs) Enumerate() ([]osproc.Info, error)                    { return nil, nil }
func (p *infoProcs) Watch(context.Context, osproc.Lifetime, func()) error { return nil }
func (p *infoProcs) Stop(osproc.Lifetime)                                 {}

// interactivePi is a TUI Pi as the scanner reads it (see discovery.IsPi).
func interactivePi(pid int, tty string) osproc.Info {
	return osproc.Info{PID: pid, Comm: "pi", Exe: "/usr/bin/node-22", CWD: "/repo", TTY: tty, StdinTTY: true,
		Args: []string{"pi"}, Birth: testBirth(pid)}
}

// discoveredPi is the session appear builds from a Pi before admitRoot.
func discoveredPi(pid int, tty string) state.Session {
	return state.Session{PID: pid, Agent: state.AgentKindPi, CWD: "/repo", TTY: tty, Birth: testBirth(pid)}
}

func TestAdmitRootShouldMergeScannerAndHerdrDiscoveryOfOnePiWhicheverArrivesFirst(t *testing.T) {
	const pid, tty = 901, "/dev/pts/7"
	pane := terminal.PaneRef{Backend: "herdr", Handle: "w1:p1", MuxSocket: testHerdrSock, TTY: tty}
	scanner := func(m map[int]*state.Session, source herdrStatusSource, now time.Time) {
		admitRoot(m, discoveredPi(pid, tty), nil, source, nil, nil, now)
	}
	herdrFind := func(m map[int]*state.Session, source herdrStatusSource, now time.Time) {
		p := pane
		admitRoot(m, discoveredPi(pid, tty), &p, source, nil, nil, now)
	}
	for _, tc := range []struct {
		name          string
		first, second func(map[int]*state.Session, herdrStatusSource, time.Time)
	}{
		{"scanner first", scanner, herdrFind},
		{"herdr first", herdrFind, scanner},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := newFakeHerdrSource()
			source.set(herdr.PaneKey{Socket: testHerdrSock, PaneID: "w1:p1"}, "pi", herdr.StatusWorking, herdrT0)
			m := map[int]*state.Session{}
			tc.first(m, source, herdrT0)
			startedAt := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
			m[pid].StartedAt = startedAt
			tc.second(m, source, herdrT0.Add(time.Second))

			if len(m) != 1 {
				t.Fatalf("sessions = %d, want one", len(m))
			}
			sess := m[pid]
			if sess.Agent != state.AgentKindPi {
				t.Fatalf("agent = %q, want pi", sess.Agent)
			}
			if sess.Herdr == nil || sess.Herdr.PaneID != "w1:p1" || sess.Herdr.Socket != testHerdrSock {
				t.Fatalf("herdr block = %+v, want pane w1:p1 kept", sess.Herdr)
			}
			if !sess.StartedAt.Equal(startedAt) {
				t.Fatalf("StartedAt = %v, want the first discovery's %v", sess.StartedAt, startedAt)
			}
		})
	}
}

// herdr discovery announces a pid the scanner already tracks as Pi, so herdr
// can attach its pane to that one session rather than skip it.
func TestRunHerdrDiscoveryShouldAnnounceAPiTheScannerAlreadyTracks(t *testing.T) {
	f := newDiscoveryFixture()
	f.pane("w1:p1", "/dev/pts/7", "pi", 901)
	store := state.New("")
	store.Apply(func(m map[int]*state.Session) {
		admitRoot(m, discoveredPi(901, "/dev/pts/7"), nil, f.status, nil, nil, herdrT0)
	})
	if got := runDiscoveryTicks(t, f, store, 2, nil); len(got) != 1 || got[0] != 901 {
		t.Fatalf("appeared = %v, want the scanner's pi announced once for its pane", got)
	}
}

func TestSweepShouldKeepTrackingAPiOutsideHerdrThroughDeath(t *testing.T) {
	const pid, tty = 902, "/dev/pts/9"
	for _, tc := range []struct {
		name string
		die  func(*infoProcs)
	}{
		{"process exits", func(p *infoProcs) { p.kill(pid) }},
		{"pid reused on the same tty", func(p *infoProcs) {
			p.set(osproc.Info{PID: pid, Comm: "bash", Exe: "/usr/bin/bash", TTY: tty, StdinTTY: true})
		}},
		{"pid reused by a json-mode pi", func(p *infoProcs) {
			child := interactivePi(pid, tty)
			child.StdinTTY = false
			p.set(child)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink, dir := newTestSink(t)
			procs := &infoProcs{infos: map[int]osproc.Info{pid: interactivePi(pid, tty)}}
			m := map[int]*state.Session{}
			admitRoot(m, discoveredPi(pid, tty), nil, newFakeHerdrSource(), sink, nil, herdrT0)
			if m[pid].Herdr != nil {
				t.Fatal("a scanner-only pi got a herdr block")
			}

			for tick := range 3 {
				sweepDeadSessions(m, procs, sink, func(osproc.Lifetime) {}, herdrT0.Add(time.Duration(tick)*time.Second))
				if _, ok := m[pid]; !ok {
					t.Fatalf("tick %d: live pi dropped", tick)
				}
			}

			tc.die(procs)
			forgot := 0
			sweepDeadSessions(m, procs, sink, func(osproc.Lifetime) { forgot++ }, herdrT0.Add(5*time.Second))
			if _, ok := m[pid]; ok {
				t.Fatal("pi kept after its process stopped being one")
			}
			if forgot != 1 {
				t.Fatalf("forget called %d times, want 1", forgot)
			}
			sink.Close()
			ended := 0
			for _, ev := range readEvents(t, dir) {
				if ev.Type == "session_end" && ev.PID == pid {
					ended++
				}
			}
			if ended != 1 {
				t.Fatalf("session_end events = %d, want 1", ended)
			}
		})
	}
}

func TestProcessIsSessionShouldJudgeAPiByItsClassifierAndAHerdrPiAlsoByItsTTY(t *testing.T) {
	const tty = "/dev/pts/7"
	scannerOnly := &state.Session{PID: 901, Agent: state.AgentKindPi, TTY: tty}
	inHerdr := &state.Session{PID: 901, Agent: state.AgentKindPi, TTY: tty,
		Herdr: &state.HerdrInfo{PaneID: "w1:p1", Socket: testHerdrSock}}
	notPiOnTTY := osproc.Info{PID: 901, Comm: "node", TTY: tty}

	if !processIsSession(interactivePi(901, tty), scannerOnly) {
		t.Fatal("an interactive pi read as not its scanner-found session")
	}
	if !processIsSession(interactivePi(901, "/dev/pts/12"), scannerOnly) {
		t.Fatal("an interactive pi read as not its session after the tty changed")
	}
	if processIsSession(notPiOnTTY, scannerOnly) {
		t.Fatal("a non-pi process on the tty vouched for a scanner-found pi")
	}
	if !processIsSession(notPiOnTTY, inHerdr) {
		t.Fatal("a herdr pi on its pane's tty read as not its session")
	}
}
