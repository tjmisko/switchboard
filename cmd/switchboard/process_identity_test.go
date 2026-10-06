package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/osproc"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/terminal"
	"github.com/tjmisko/switchboard/internal/transcript"
)

// Process lifetime identity (#97). Every test here stages a pid reuse on
// demand through a fake process source or a direct store edit: the same pid,
// and often the same tty, executable and even StartedAt, under a new birth
// token. None of them races a real process.

var identityT0 = time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)

// identityClaude is a tracked Claude root with the status, provider binding,
// display name and graph a reuse must not hand on.
func identityClaude(t *testing.T, pid int, birth string, startedAt time.Time) *state.Session {
	t.Helper()
	graph, err := state.ProjectAgentGraph(agentgraph.Observation{
		Provider: agentgraph.ProviderClaude, RootID: "sid-old", Source: agentgraph.SourceClaudeTranscript,
		ObservedAt: identityT0, FreshUntil: identityT0.Add(time.Hour),
		Nodes: []agentgraph.Node{{ID: "sid-old", Runtime: agentgraph.RuntimeActive, UpdatedAt: identityT0}},
	}, nil, identityT0)
	if err != nil {
		t.Fatal(err)
	}
	return &state.Session{
		PID: pid, Agent: state.AgentKindClaude, CWD: "/repo", TTY: "/dev/pts/4", StartedAt: startedAt, Birth: birth,
		Claude:      &state.AgentInfo{SessionID: "sid-old", Status: state.StatusPermission},
		DisplayName: &state.DisplayName{Value: "old name", ConversationID: "sid-old"},
		AgentGraph:  graph,
	}
}

// discoveredClaude is what appear builds for a Claude process before
// admitRoot: the resolver's fresh StartedAt and the live birth token.
func discoveredClaude(pid int, birth string, startedAt time.Time) state.Session {
	return state.Session{PID: pid, Agent: state.AgentKindClaude, CWD: "/repo", TTY: "/dev/pts/4", StartedAt: startedAt, Birth: birth}
}

func TestAdmitRootShouldInheritOnlyTheSameProcessLifetime(t *testing.T) {
	const pid = 4400
	rediscovered := identityT0.Add(time.Hour)

	t.Run("should keep the display start and provider state when the same lifetime is re-announced", func(t *testing.T) {
		sink, dir := newTestSink(t)
		m := map[int]*state.Session{pid: identityClaude(t, pid, "boot:1", identityT0)}
		got := admitRoot(m, discoveredClaude(pid, "boot:1", rediscovered), nil, newFakeHerdrSource(), sink, nil, rediscovered)
		if !got.StartedAt.Equal(identityT0) || got.Claude == nil || got.Claude.SessionID != "sid-old" || got.AgentGraph == nil || got.DisplayName == nil {
			t.Fatalf("admitted = %+v, want the same lifetime's start, binding, graph and name kept", got)
		}
		sink.Close()
		if ends := eventsOfType(readEvents(t, dir), history.EventSessionEnd); len(ends) != 0 {
			t.Fatalf("session_end events = %+v, want none for a re-announced lifetime", ends)
		}
	})

	t.Run("should inherit nothing and close the old lane when the pid is reused by the same agent on the same tty", func(t *testing.T) {
		sink, dir := newTestSink(t)
		m := map[int]*state.Session{pid: identityClaude(t, pid, "boot:1", identityT0)}
		var forgotten []osproc.Lifetime
		got := admitRoot(m, discoveredClaude(pid, "boot:2", rediscovered), nil, newFakeHerdrSource(), sink,
			func(l osproc.Lifetime) { forgotten = append(forgotten, l) }, rediscovered)
		if !got.StartedAt.Equal(rediscovered) {
			t.Errorf("StartedAt = %v, want the new lifetime's own %v", got.StartedAt, rediscovered)
		}
		if got.Claude != nil || got.AgentGraph != nil || got.DisplayName != nil {
			t.Errorf("admitted = %+v, want no status, binding, graph or name from the previous lifetime", got)
		}
		if status := got.ExplainStatus(rediscovered).Status; status == state.StatusPermission {
			t.Errorf("published status = %q, want the previous lifetime's red not inherited", status)
		}
		if m[pid] != got || m[pid].Birth != "boot:2" {
			t.Errorf("m[%d] = %+v, want the new lifetime stored", pid, m[pid])
		}
		if len(forgotten) != 1 || forgotten[0] != (osproc.Lifetime{PID: pid, Birth: "boot:1"}) {
			t.Errorf("forgot %v, want the old lifetime forgotten once", forgotten)
		}
		sink.Close()
		ends := eventsOfType(readEvents(t, dir), history.EventSessionEnd)
		if len(ends) != 1 || ends[0].SessionID != "sid-old" {
			t.Fatalf("session_end events = %+v, want the old lifetime's lane closed once", ends)
		}
	})

	t.Run("should inherit nothing and close no lane when the new lifetime is unverified", func(t *testing.T) {
		sink, dir := newTestSink(t)
		m := map[int]*state.Session{pid: identityClaude(t, pid, "boot:1", identityT0)}
		got := admitRoot(m, discoveredClaude(pid, "", rediscovered), nil, newFakeHerdrSource(), sink, nil, rediscovered)
		if !got.StartedAt.Equal(rediscovered) || got.Claude != nil || got.AgentGraph != nil || got.DisplayName != nil {
			t.Errorf("admitted = %+v, want an unverified lifetime to inherit nothing", got)
		}
		sink.Close()
		if ends := eventsOfType(readEvents(t, dir), history.EventSessionEnd); len(ends) != 0 {
			t.Fatalf("session_end events = %+v, want none: an unverified token proves no death", ends)
		}
	})

	t.Run("should inherit nothing when the prior lifetime is unverified", func(t *testing.T) {
		m := map[int]*state.Session{pid: identityClaude(t, pid, "", identityT0)}
		got := admitRoot(m, discoveredClaude(pid, "boot:1", rediscovered), nil, newFakeHerdrSource(), nil, nil, rediscovered)
		if !got.StartedAt.Equal(rediscovered) || got.Claude != nil || got.AgentGraph != nil {
			t.Errorf("admitted = %+v, want nothing inherited from an unverified prior", got)
		}
	})

	t.Run("should not hand a herdr pane to a pi that reuses the pid on the same tty", func(t *testing.T) {
		const piPID, tty = 4401, "/dev/pts/7"
		source := newFakeHerdrSource()
		pane := terminal.PaneRef{Backend: "herdr", Handle: "w1:p1", MuxSocket: testHerdrSock, TTY: tty}
		m := map[int]*state.Session{}
		admitRoot(m, discoveredPi(piPID, tty), &pane, source, nil, nil, identityT0)
		if m[piPID].Herdr == nil {
			t.Fatal("setup: herdr discovery attached no pane")
		}
		reused := discoveredPi(piPID, tty)
		reused.Birth = "boot-test:reused"
		got := admitRoot(m, reused, nil, source, nil, nil, rediscovered)
		if got.Herdr != nil {
			t.Fatalf("herdr block = %+v, want none: the pane belonged to the previous pi", got.Herdr)
		}
	})
}

// A daemon restart restores display state and authority only once the live
// birth token proves the pid still holds the persisted lifetime.
func TestRestartShouldRevalidateTheProcessLifetimeBeforeRestoringAuthority(t *testing.T) {
	const pid = 4404
	rediscovered := identityT0.Add(2 * time.Hour)
	for _, tc := range []struct {
		name            string
		persistedBirth  string
		live            osproc.Info
		wantRestored    bool
		wantSessionEnds int
	}{
		{
			name:           "should keep the display start and provider state when the live token matches",
			persistedBirth: "boot:1",
			live:           osproc.Info{PID: pid, Comm: "claude", TTY: "/dev/pts/4", Birth: "boot:1"},
			wantRestored:   true,
		},
		{
			name:            "should close the lane and restore nothing when the pid was reused by the same agent on the same tty",
			persistedBirth:  "boot:1",
			live:            osproc.Info{PID: pid, Comm: "claude", TTY: "/dev/pts/4", Birth: "boot:2"},
			wantSessionEnds: 1,
		},
		{
			name:           "should restore nothing and close no lane when the mirror predates birth tokens",
			persistedBirth: "",
			live:           osproc.Info{PID: pid, Comm: "claude", TTY: "/dev/pts/4", Birth: "boot:1"},
		},
		{
			name:           "should restore nothing and close no lane when the live token is unavailable",
			persistedBirth: "boot:1",
			live:           osproc.Info{PID: pid, Comm: "claude", TTY: "/dev/pts/4"},
		},
		{
			name:            "should close the lane when an unverified pid no longer runs the agent",
			persistedBirth:  "",
			live:            osproc.Info{PID: pid, Comm: "bash", TTY: "/dev/pts/4"},
			wantSessionEnds: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			before := state.New(path)
			before.Apply(func(m map[int]*state.Session) { m[pid] = identityClaude(t, pid, tc.persistedBirth, identityT0) })

			store := state.New(path)
			if err := store.Load(); err != nil {
				t.Fatalf("Load: %v", err)
			}
			sink, dir := newTestSink(t)
			procs := &infoProcs{infos: map[int]osproc.Info{pid: tc.live}}
			dropStaleSessions(store, procs, sink, nil, transcript.DefaultTailBytes)

			restored := len(store.Snapshot().Sessions) == 1
			if restored != tc.wantRestored {
				t.Fatalf("restored = %v after the stale drop, want %v", restored, tc.wantRestored)
			}
			// The scanner then re-announces the live process.
			store.Apply(func(m map[int]*state.Session) {
				admitRoot(m, discoveredClaude(pid, tc.live.Birth, rediscovered), nil, newFakeHerdrSource(), sink, nil, rediscovered)
			})
			got := store.Snapshot().Sessions[0]
			if tc.wantRestored {
				if !got.StartedAt.Equal(identityT0) || got.Claude == nil || got.Claude.SessionID != "sid-old" || got.DisplayName == nil {
					t.Errorf("session = %+v, want the persisted start time, binding and name restored", got)
				}
			} else if !got.StartedAt.Equal(rediscovered) || got.Claude != nil || got.AgentGraph != nil || got.DisplayName != nil {
				t.Errorf("session = %+v, want a fresh lifetime with no restored authority", got)
			}
			sink.Close()
			if ends := eventsOfType(readEvents(t, dir), history.EventSessionEnd); len(ends) != tc.wantSessionEnds {
				t.Errorf("session_end events = %d, want %d", len(ends), tc.wantSessionEnds)
			}
		})
	}
}

// The liveness sweep is the only death check an unverified session gets, so it
// keeps the classifier rule birth tokens replaced; a verified session is ended
// by a token mismatch whatever the process looks like.
func TestSweepShouldJudgeLifetimeByBirthTokenAndFallBackWhenUnverified(t *testing.T) {
	const pid = 4405
	for _, tc := range []struct {
		name    string
		birth   string
		live    osproc.Info
		wantEnd bool
	}{
		{"should end a verified session when its pid holds another lifetime of the same agent on the same tty", "boot:1",
			osproc.Info{PID: pid, Comm: "claude", TTY: "/dev/pts/4", Birth: "boot:2"}, true},
		{"should keep a verified session while its lifetime runs", "boot:1",
			osproc.Info{PID: pid, Comm: "claude", TTY: "/dev/pts/4", Birth: "boot:1"}, false},
		{"should keep an unverified session while the classifier vouches for its pid", "",
			osproc.Info{PID: pid, Comm: "claude", TTY: "/dev/pts/4", Birth: "boot:2"}, false},
		{"should end an unverified session when its pid no longer runs the agent", "",
			osproc.Info{PID: pid, Comm: "bash", TTY: "/dev/pts/4"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := history.NewSink(history.Config{})
			m := map[int]*state.Session{pid: identityClaude(t, pid, tc.birth, identityT0)}
			sweepDeadSessions(m, &infoProcs{infos: map[int]osproc.Info{pid: tc.live}}, sink, nil, identityT0)
			if _, kept := m[pid]; kept == tc.wantEnd {
				t.Fatalf("kept = %v, want ended = %v", kept, tc.wantEnd)
			}
		})
	}
}
