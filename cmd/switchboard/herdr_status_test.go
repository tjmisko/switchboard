package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/herdr"
	"github.com/tjmisko/switchboard/internal/history"
	claudeprovider "github.com/tjmisko/switchboard/internal/provider/claude"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/statustune"
	"github.com/tjmisko/switchboard/internal/terminal"
)

// fakeHerdrSource is a programmable herdr status source.
type fakeHerdrSource struct {
	mu       sync.Mutex
	statuses map[herdr.PaneKey]herdr.PaneStatus
	active   map[string]string // focused pane by socket
	truth    string            // the pane herdr's own focus is on, for Resync
	servers  []string
	changes  chan herdr.PaneKey
}

func newFakeHerdrSource() *fakeHerdrSource {
	return &fakeHerdrSource{
		statuses: make(map[herdr.PaneKey]herdr.PaneStatus),
		active:   make(map[string]string),
		changes:  make(chan herdr.PaneKey, 64),
	}
}

func (f *fakeHerdrSource) SetServers(_ context.Context, sockets []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.servers = sockets
}

func (f *fakeHerdrSource) Status(key herdr.PaneKey) (herdr.PaneStatus, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	status, ok := f.statuses[key]
	return status, ok
}

func (f *fakeHerdrSource) ActivePane(socket string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.active[socket]
}

func (f *fakeHerdrSource) Changes() <-chan herdr.PaneKey { return f.changes }

func (f *fakeHerdrSource) Claim(socket, paneID string) bool {
	f.focus(socket, paneID)
	return true
}

// Resync restores the focused pane herdr itself reports (truth), as a re-list
// does, and signals the server's panes when it moved.
func (f *fakeHerdrSource) Resync(_ context.Context, socket string) error {
	f.mu.Lock()
	truth := f.truth
	moved := f.active[socket] != truth
	f.mu.Unlock()
	if moved {
		f.focus(socket, truth)
	}
	return nil
}

// focus moves the server's focused pane and signals every pane on it, as the
// watcher does.
func (f *fakeHerdrSource) focus(socket, paneID string) {
	f.mu.Lock()
	f.active[socket] = paneID
	var keys []herdr.PaneKey
	for key := range f.statuses {
		if key.Socket == socket {
			keys = append(keys, key)
		}
	}
	f.mu.Unlock()
	for _, key := range keys {
		f.changes <- key
	}
}

func (f *fakeHerdrSource) set(key herdr.PaneKey, agent, status string, since time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses[key] = herdr.PaneStatus{Agent: agent, Status: status, Since: since}
}

func (f *fakeHerdrSource) drop(key herdr.PaneKey) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.statuses, key)
}

const testHerdrSock = "/home/u/.config/herdr/herdr.sock"

var (
	herdrKey = herdr.PaneKey{Socket: testHerdrSock, PaneID: "w1:p1"}
	herdrRef = terminal.PaneRef{Backend: "herdr", Handle: "w1:p1", MuxSocket: testHerdrSock, TTY: "/dev/pts/7"}
	herdrT0  = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
)

// graphClaudeSession is a Claude session in a herdr pane whose provider graph,
// observed at herdrT0 and fresh for a day, says idle.
func graphClaudeSession(t *testing.T) *state.Session {
	t.Helper()
	sess := &state.Session{PID: 700, StartedAt: herdrT0.Add(-time.Hour), Agent: state.AgentKindClaude, TTY: "/dev/pts/7", CWD: "/repo"}
	graph, err := state.ProjectAgentGraph(agentgraph.Observation{
		Provider: agentgraph.ProviderClaude, RootID: "sess-1", Source: agentgraph.SourceClaudeTranscript,
		ObservedAt: herdrT0, FreshUntil: herdrT0.Add(24 * time.Hour),
		Nodes: []agentgraph.Node{{ID: "sess-1", Runtime: agentgraph.RuntimeIdle, UpdatedAt: herdrT0}},
	}, nil, herdrT0)
	if err != nil {
		t.Fatal(err)
	}
	sess.SetAgentGraph(graph, herdrT0)
	if sess.Claude.Status != state.StatusIdle {
		t.Fatalf("graph session published %q, want idle", sess.Claude.Status)
	}
	return sess
}

func newTestSink(t *testing.T) (*history.Sink, string) {
	t.Helper()
	dir := t.TempDir()
	return history.NewSink(history.Config{Enabled: true, Detail: history.DetailFull, Dir: dir}), dir
}

func transitionsIn(t *testing.T, sink *history.Sink, dir string) []history.Event {
	t.Helper()
	sink.Close()
	var out []history.Event
	for _, ev := range readEvents(t, dir) {
		if ev.Type == history.EventTransition {
			out = append(out, ev)
		}
	}
	return out
}

func TestApplyHerdrPaneShouldPublishHerdrsStatusWhenItsPaneReportsOne(t *testing.T) {
	sess := graphClaudeSession(t)
	source := newFakeHerdrSource()
	source.set(herdrKey, "claude", herdr.StatusBlocked, herdrT0.Add(time.Minute))
	sink, dir := newTestSink(t)

	ref := herdrRef
	applyHerdrPane(sess, &ref, source, sink, herdrT0.Add(2*time.Minute))
	if sess.Claude.Status != state.StatusPermission {
		t.Fatalf("status = %q, want permission from herdr's blocked", sess.Claude.Status)
	}
	if !sess.Claude.StatusSince.Equal(herdrT0.Add(time.Minute)) {
		t.Fatalf("StatusSince = %v, want when herdr's status began", sess.Claude.StatusSince)
	}
	if sess.Herdr == nil || sess.Herdr.PaneID != "w1:p1" || sess.Herdr.Agent != "claude" || !sess.Herdr.Live {
		t.Fatalf("herdr block = %+v, want the live pane", sess.Herdr)
	}
	evs := transitionsIn(t, sink, dir)
	if len(evs) != 1 || evs[0].From != state.StatusIdle || evs[0].To != state.StatusPermission || evs[0].Rule != statustune.RuleHerdrAuthority {
		t.Fatalf("transitions = %+v, want one idle→permission by herdr_authority", evs)
	}
}

func TestApplyHerdrPaneShouldRecordNothingWhenHerdrAgreesWithTheProvider(t *testing.T) {
	sess := graphClaudeSession(t)
	source := newFakeHerdrSource()
	source.set(herdrKey, "claude", herdr.StatusIdle, herdrT0)
	sink, dir := newTestSink(t)

	ref := herdrRef
	applyHerdrPane(sess, &ref, source, sink, herdrT0.Add(time.Minute))
	applyHerdrPane(sess, &ref, source, sink, herdrT0.Add(2*time.Minute))
	if evs := transitionsIn(t, sink, dir); len(evs) != 0 {
		t.Fatalf("transitions = %+v, want none", evs)
	}
}

func TestApplyHerdrPaneShouldReturnTheStatusToTheProviderWhenTheServerStopsCounting(t *testing.T) {
	sess := graphClaudeSession(t)
	source := newFakeHerdrSource()
	source.set(herdrKey, "claude", herdr.StatusWorking, herdrT0)
	sink, dir := newTestSink(t)
	ref := herdrRef
	applyHerdrPane(sess, &ref, source, sink, herdrT0.Add(time.Minute))

	source.drop(herdrKey)
	applyHerdrPane(sess, &ref, source, sink, herdrT0.Add(2*time.Minute))
	if sess.Claude.Status != state.StatusIdle || sess.Herdr.Live {
		t.Fatalf("status = %q live=%v, want the provider's idle and herdr withdrawn", sess.Claude.Status, sess.Herdr.Live)
	}
	evs := transitionsIn(t, sink, dir)
	if len(evs) != 2 || evs[1].From != state.StatusWorking || evs[1].To != state.StatusIdle || evs[1].Rule != statustune.RuleHerdrReleased {
		t.Fatalf("transitions = %+v, want working→idle by herdr_released last", evs)
	}
}

// herdr gives a pane a new id when it moves to another workspace; the tick's
// enumeration carries the new one.
func TestApplyHerdrPaneShouldFollowThePaneWhenTheEnumerationGivesItANewID(t *testing.T) {
	sess := graphClaudeSession(t)
	source := newFakeHerdrSource()
	source.set(herdrKey, "claude", herdr.StatusIdle, herdrT0)
	moved := herdr.PaneKey{Socket: testHerdrSock, PaneID: "w2:p4"}
	source.set(moved, "claude", herdr.StatusWorking, herdrT0)
	ref := herdrRef
	applyHerdrPane(sess, &ref, source, nil, herdrT0)

	ref.Handle = "w2:p4"
	applyHerdrPane(sess, &ref, source, nil, herdrT0.Add(time.Minute))
	if sess.Herdr.PaneID != "w2:p4" || sess.Claude.Status != state.StatusWorking {
		t.Fatalf("herdr=%+v status=%q, want the moved pane's working", sess.Herdr, sess.Claude.Status)
	}
}

func TestApplyHerdrPaneShouldKeepTheLastPaneWhenTheTTYIsMissingFromAnEnumeration(t *testing.T) {
	sess := graphClaudeSession(t)
	source := newFakeHerdrSource()
	source.set(herdrKey, "claude", herdr.StatusWorking, herdrT0)
	ref := herdrRef
	applyHerdrPane(sess, &ref, source, nil, herdrT0)

	applyHerdrPane(sess, nil, source, nil, herdrT0.Add(time.Minute))
	if sess.Herdr.PaneID != "w1:p1" || !sess.Herdr.Live || sess.Claude.Status != state.StatusWorking {
		t.Fatalf("herdr=%+v status=%q, want the pane and herdr's working kept", sess.Herdr, sess.Claude.Status)
	}
}

func TestApplyHerdrPaneShouldIgnoreASessionOutsideHerdr(t *testing.T) {
	sess := graphClaudeSession(t)
	wez := terminal.PaneRef{Backend: "wezterm", Mux: 9, PaneID: 3, TTY: "/dev/pts/7"}
	applyHerdrPane(sess, &wez, newFakeHerdrSource(), nil, herdrT0)
	applyHerdrPane(sess, nil, newFakeHerdrSource(), nil, herdrT0)
	if sess.Herdr != nil || sess.Claude.Status != state.StatusIdle {
		t.Fatalf("herdr=%+v status=%q, want no herdr block and the provider's status", sess.Herdr, sess.Claude.Status)
	}
}

func TestHerdrSocketsShouldListEachHerdrServerOnceWhenPanesShareIt(t *testing.T) {
	work := "/home/u/.config/herdr/sessions/work/herdr.sock"
	got := herdrSockets(map[string]terminal.PaneRef{
		"/dev/pts/7": herdrRef,
		"/dev/pts/8": {Backend: "herdr", Handle: "w1:p2", MuxSocket: testHerdrSock},
		"/dev/pts/9": {Backend: "herdr", Handle: "w1:p1", MuxSocket: work},
		"/dev/pts/3": {Backend: "wezterm", MuxSocket: "/run/wezterm/gui-sock-1"},
	})
	if len(got) != 2 || got[0] != testHerdrSock || got[1] != work {
		t.Fatalf("herdrSockets = %v, want [%s %s]", got, testHerdrSock, work)
	}
}

func TestRunHerdrStatusShouldApplyASignalledPaneToItsSession(t *testing.T) {
	store := state.New("")
	store.Apply(func(m map[int]*state.Session) {
		sess := graphClaudeSession(t)
		sess.Herdr = &state.HerdrInfo{PaneID: "w1:p1", Socket: testHerdrSock}
		m[sess.PID] = sess
	})
	source := newFakeHerdrSource()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runHerdrStatus(ctx, store, source, nil, nil)

	source.set(herdrKey, "claude", herdr.StatusWorking, time.Now())
	source.changes <- herdrKey
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s := store.Snapshot().Sessions[0]; s.Claude.Status == state.StatusWorking {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("status = %q, want working after the signal", store.Snapshot().Sessions[0].Claude.Status)
}

// Codex attention must remain visible even when screen detection reports work.
func TestCodexAttentionShouldOverrideHerdrWorkingAndRecordTheDisplayedEdge(t *testing.T) {
	store := state.New("")
	started := time.Now().Add(-time.Hour)
	ref := seedCoordinatorSession(store, 4077, started, state.AgentKindCodex, "root", "/codex")
	sink, dir := newTestSink(t)
	coordinator := newAgentCoordinator(store, sink, nil, newFakeCodexCoordinatorObserver())
	coordinator.refreshTrackedRoots()
	defer coordinator.Close()

	now := time.Now().Add(-3 * time.Second)
	observe := func(runtime agentgraph.RuntimeState, attention agentgraph.AttentionState, at time.Time) {
		t.Helper()
		observation := agentgraph.Observation{
			Provider: agentgraph.ProviderCodex, RootID: "root", Source: agentgraph.SourceCodexAppServer,
			ObservedAt: at, FreshUntil: at.Add(time.Hour), Complete: true,
			Nodes: []agentgraph.Node{{ID: "root", Runtime: runtime, Attention: attention, UpdatedAt: at}},
		}
		if !coordinator.applyObservation(ref, coordinator.begin(ref.Key()), observation, claudeprovider.Compatibility{}, at) {
			t.Fatal("observation did not land")
		}
	}
	observe(agentgraph.RuntimeIdle, agentgraph.AttentionNone, now)

	source := newFakeHerdrSource()
	source.set(herdr.PaneKey{Socket: testHerdrSock, PaneID: "w1:p1"}, "codex", herdr.StatusWorking, now)
	store.Apply(func(m map[int]*state.Session) {
		pane := herdrRef
		applyHerdrPane(m[4077], &pane, source, sink, now.Add(time.Second))
	})

	observe(agentgraph.RuntimeActive, agentgraph.AttentionApproval, now.Add(2*time.Second))
	sess := store.Snapshot().Sessions[0]
	if sess.Codex.Status != state.StatusPermission {
		t.Fatalf("status = %q, want Codex approval attention to override herdr's working", sess.Codex.Status)
	}
	if sess.AgentGraph.Summary.Status != state.StatusPermission {
		t.Fatalf("graph summary = %q, want the provider's own permission recorded", sess.AgentGraph.Summary.Status)
	}
	evs := transitionsIn(t, sink, dir)
	found := false
	for _, ev := range evs {
		if ev.To == state.StatusPermission {
			found = true
		}
	}
	if !found {
		t.Fatalf("transitions = %+v, want the displayed approval edge recorded", evs)
	}
}
