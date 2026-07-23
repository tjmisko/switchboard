package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/osproc"
	"github.com/tjmisko/switchboard/internal/state"
)

// A running catalog of session-LIFECYCLE hazards, each pinned as a regression.
// Where timing_hazards_test.go covers "what color is this LIVE chip", this file
// covers "is this session still alive at all" — the question whose wrong answer
// produces a ghost lane: a session that ended long ago whose final status
// interval the reader stretches all the way to `now`, inflating every
// duration-derived number for the day.
//
// The defect class (docs/session-lifecycle-hazards.md): session_end is what
// bounds a lane, and its original sole writer was the pidfd death-watch — which
// lives in daemon memory and therefore does NOT survive a restart or SIGKILL.
// Every row below is a way that watch fails to report a death, plus the
// contrast rows that must NOT be mistaken for one.
//
// Each row models: a tracked session + what the OS says about its pid → the
// sweep runs → did the lane close, and was it closed exactly once?

// procState is what the fake osproc.Source reports for a pid.
type procState int

const (
	procAlive      procState = iota // a live claude process — the session is running
	procGone                        // ErrGone — the ordinary observed death
	procRecycled                    // the pid exists but is no longer an agent (kernel reuse)
	procUnreadable                  // a transient / unsupported-backend read failure: liveness UNKNOWN
)

// fakeProcSource is an osproc.Source whose per-pid liveness the test drives.
// Pids absent from st read as procAlive.
type fakeProcSource struct{ st map[int]procState }

func (f fakeProcSource) Read(pid int) (osproc.Info, error) {
	switch f.st[pid] {
	case procGone:
		return osproc.Info{PID: pid}, osproc.ErrGone
	case procRecycled:
		// A real process, but somebody else's: the kernel handed our pid to bash.
		return osproc.Info{PID: pid, Comm: "bash", Exe: "/usr/bin/bash"}, nil
	case procUnreadable:
		return osproc.Info{PID: pid}, osproc.ErrUnsupported
	default:
		// Comm "claude" with a masked exe is a valid claude snapshot (see IsClaude).
		return osproc.Info{PID: pid, Comm: "claude"}, nil
	}
}

func (f fakeProcSource) Enumerate() ([]osproc.Info, error)        { return nil, nil }
func (f fakeProcSource) Watch(context.Context, int, func()) error { return nil }
func (f fakeProcSource) Stop(int)                                 {}

// trackedSession is one session in the store map, as the daemon holds it.
func trackedSession(pid int, sid string) *state.Session {
	return &state.Session{PID: pid, Agent: "claude", CWD: "/home/u/proj",
		Claude: &state.AgentInfo{SessionID: sid}}
}

func TestSessionLifecycleHazards(t *testing.T) {
	const pid = 4242

	rows := []struct {
		id      string
		why     string
		state   procState
		wantEnd bool // a session_end was recorded (the lane closed)
	}{
		{
			id:      "L1-restart-orphans-watch",
			why:     "daemon restart/SIGKILL wiped the in-memory watch; the process then died unobserved",
			state:   procGone,
			wantEnd: true,
		},
		{
			id:      "L3-watch-registration-failed",
			why:     "procSrc.Watch errored at discovery, so this session never had a death-watch at all",
			state:   procGone,
			wantEnd: true,
		},
		{
			id:      "L4-live-idle-not-a-ghost",
			why:     "a genuinely live session sitting idle for hours must never be ended; the sweep keys on death, not inactivity",
			state:   procAlive,
			wantEnd: false,
		},
		{
			id:      "L4b-recycled-pid-is-a-death",
			why:     "the pid resolves, but to a non-agent process — our session is definitively over",
			state:   procRecycled,
			wantEnd: true,
		},
		{
			id:      "L4c-unreadable-is-not-a-death",
			why:     "a transient/unsupported read proves nothing; fabricating an end here would split a running session into two lanes",
			state:   procUnreadable,
			wantEnd: false,
		},
	}

	for _, row := range rows {
		t.Run(row.id, func(t *testing.T) {
			histDir := t.TempDir()
			sink := history.NewSink(history.Config{Enabled: true, Detail: history.DetailFull, Dir: histDir})
			src := fakeProcSource{st: map[int]procState{pid: row.state}}

			m := map[int]*state.Session{pid: trackedSession(pid, "sid-1")}
			var forgotten []int
			sweepDeadSessions(m, src, sink, func(p int) { forgotten = append(forgotten, p) }, time.Now())
			sink.Close()

			ends := eventsOfType(readEvents(t, histDir), history.EventSessionEnd)
			if got := len(ends) == 1; got != row.wantEnd {
				t.Fatalf("%s: got %d session_end events, wantEnd=%v (%s)", row.id, len(ends), row.wantEnd, row.why)
			}

			// A closed lane is also dropped from the map and forgotten by the scanner
			// (so a recycled pid is re-discovered); a live one is left entirely alone.
			_, stillTracked := m[pid]
			if stillTracked == row.wantEnd {
				t.Errorf("%s: session tracked=%v after sweep, want tracked=%v", row.id, stillTracked, !row.wantEnd)
			}
			if row.wantEnd {
				if len(forgotten) != 1 || forgotten[0] != pid {
					t.Errorf("%s: forgot %v, want the scanner to forget pid %d", row.id, forgotten, pid)
				}
				if ends[0].SessionID != "sid-1" || ends[0].PID != pid {
					t.Errorf("%s: session_end = %+v, want it to carry sid-1/pid %d", row.id, ends[0], pid)
				}
			} else if len(forgotten) != 0 {
				t.Errorf("%s: forgot %v, want nothing forgotten for a live session", row.id, forgotten)
			}
		})
	}
}

// L2: a session that died while the daemon was DOWN has no watch of ours that
// could ever have fired. The startup stale-drop is the only thing that will see
// it, so it must record the session_end rather than silently deleting — which is
// what left the 2026-07-22 ghosts open.
func TestDropStaleSessionsRecordsSessionEnd(t *testing.T) {
	const deadPID, livePID = 111, 222

	histDir := t.TempDir()
	sink := history.NewSink(history.Config{Enabled: true, Detail: history.DetailFull, Dir: histDir})
	store := state.New(filepath.Join(t.TempDir(), "state.json"))
	store.Apply(func(m map[int]*state.Session) {
		m[deadPID] = trackedSession(deadPID, "sid-dead")
		m[livePID] = trackedSession(livePID, "sid-live")
	})

	src := fakeProcSource{st: map[int]procState{deadPID: procGone, livePID: procAlive}}
	var forgotten []int
	dropStaleSessions(store, src, sink, func(p int) { forgotten = append(forgotten, p) })
	sink.Close()

	ends := eventsOfType(readEvents(t, histDir), history.EventSessionEnd)
	if len(ends) != 1 {
		t.Fatalf("got %d session_end events, want exactly one (for the pid that died while we were down)", len(ends))
	}
	if ends[0].SessionID != "sid-dead" || ends[0].PID != deadPID {
		t.Errorf("session_end = %+v, want it to close sid-dead/pid %d", ends[0], deadPID)
	}
	if len(forgotten) != 1 || forgotten[0] != deadPID {
		t.Errorf("forgot %v, want just the dead pid %d", forgotten, deadPID)
	}

	store.Apply(func(m map[int]*state.Session) {
		if _, ok := m[deadPID]; ok {
			t.Errorf("dead pid %d still tracked after stale-drop", deadPID)
		}
		if _, ok := m[livePID]; !ok {
			t.Errorf("live pid %d was dropped; a survivor must be kept and re-watched", livePID)
		}
	})
}

// L5: the pidfd watch and the liveness sweep can both observe the same death.
// Store-map membership is the dedup — whichever fires first removes the session,
// so the other records nothing. One death must never produce two session_ends
// (which would close the lane twice and double-count the gap between them).
func TestEndSessionEmitsExactlyOncePerDeath(t *testing.T) {
	const pid = 909

	histDir := t.TempDir()
	sink := history.NewSink(history.Config{Enabled: true, Detail: history.DetailFull, Dir: histDir})
	src := fakeProcSource{st: map[int]procState{pid: procGone}}
	m := map[int]*state.Session{pid: trackedSession(pid, "sid-1")}
	now := time.Now()

	// The sweep notices first...
	sweepDeadSessions(m, src, sink, func(int) {}, now)
	// ...then the orphaned-but-late pidfd callback fires for the same death.
	if endSession(m, pid, sink, func(int) {}, now) {
		t.Error("second endSession reported closing the lane again; it must be a no-op")
	}
	// ...and a later tick sweeps again for good measure.
	sweepDeadSessions(m, src, sink, func(int) {}, now)
	sink.Close()

	if ends := eventsOfType(readEvents(t, histDir), history.EventSessionEnd); len(ends) != 1 {
		t.Fatalf("got %d session_end events for one death, want exactly 1", len(ends))
	}
}

// L6: the ghost itself, at the reader. A lane with no session_end is stretched to
// the caller's end bound (`now` for a live day) no matter how long ago the
// session really stopped — this is what rendered three dead sessions as 4½-hour
// bars on 2026-07-22. The session_end the fix now emits is what bounds it.
func TestGhostLaneIsBoundedBySessionEnd(t *testing.T) {
	const pid = 3407477
	base := time.Date(2026, 7, 22, 10, 14, 50, 0, time.Local)
	death := base.Add(18 * time.Minute) // last real activity ~10:32
	now := base.Add(5 * time.Hour)      // the dashboard's "now" — 15:08

	events := []history.Event{
		{Ts: base, Type: history.EventSessionStart, PID: pid, Agent: "claude"},
		{Ts: base.Add(13 * time.Second), Type: history.EventTransition, PID: pid, SessionID: "4a9af989", To: "working"},
		{Ts: base.Add(17 * time.Minute), Type: history.EventTransition, PID: pid, SessionID: "4a9af989", From: "working", To: "dormant"},
	}

	// Before the fix: nothing closes the lane, so it runs to `now`.
	ghost := history.BuildSwimlanes(events, now)
	if len(ghost) != 1 {
		t.Fatalf("got %d lanes, want 1", len(ghost))
	}
	if !ghost[0].End.Equal(now) {
		t.Fatalf("unbounded lane ends at %v, want it stretched to now (%v) — the ghost this test pins", ghost[0].End, now)
	}
	if d := ghost[0].End.Sub(death); d < 4*time.Hour {
		t.Fatalf("ghost lane only overshoots the death by %v; the scenario is not reproducing", d)
	}

	// After the fix: the sweep/stale-drop records a session_end at the death, and
	// the lane closes there instead of at `now`.
	fixed := history.BuildSwimlanes(append(events, history.Event{
		Ts: death, Type: history.EventSessionEnd, PID: pid, SessionID: "4a9af989", Agent: "claude",
	}), now)
	if len(fixed) != 1 {
		t.Fatalf("got %d lanes, want 1", len(fixed))
	}
	if !fixed[0].End.Equal(death) {
		t.Errorf("lane ends at %v, want it bounded at the death (%v)", fixed[0].End, death)
	}
	if last := fixed[0].Intervals[len(fixed[0].Intervals)-1]; last.End.After(death) {
		t.Errorf("final interval runs to %v, past the death at %v — still a ghost", last.End, death)
	}
}
