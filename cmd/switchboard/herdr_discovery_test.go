package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/herdr"
	"github.com/tjmisko/switchboard/internal/osproc"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/terminal"
)

// fakeHerdrPanes is the herdr backend's pane snapshot, or its error.
type fakeHerdrPanes struct {
	panes map[string]terminal.PaneRef
	err   error
}

func (f *fakeHerdrPanes) Snapshot(context.Context) (map[string]terminal.PaneRef, error) {
	return f.panes, f.err
}

// ttyProcs is an osproc.Source that knows each pid's tty; unknown pids are gone.
type ttyProcs struct {
	mu   sync.Mutex
	ttys map[int]string
}

func (p *ttyProcs) Read(pid int) (osproc.Info, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	tty, ok := p.ttys[pid]
	if !ok {
		return osproc.Info{PID: pid}, osproc.ErrGone
	}
	return osproc.Info{PID: pid, Comm: "node", TTY: tty, CWD: "/repo"}, nil
}
func (p *ttyProcs) Enumerate() ([]osproc.Info, error)        { return nil, nil }
func (p *ttyProcs) Watch(context.Context, int, func()) error { return nil }
func (p *ttyProcs) Stop(int)                                 {}

// processInfoCaller answers pane.process_info from a pane → (pgid, pids) table
// and counts its calls.
type processInfoCaller struct {
	mu     sync.Mutex
	groups map[string][]int // pane id → foreground pids, the first being the group leader
	calls  int
}

func (c *processInfoCaller) call(_ context.Context, _, method string, params, result any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if method != "pane.process_info" {
		return errors.New("unexpected " + method)
	}
	c.calls++
	pids := c.groups[params.(map[string]string)["pane_id"]]
	if len(pids) == 0 {
		return errors.New("not_found")
	}
	var procs []map[string]int
	for _, pid := range pids {
		procs = append(procs, map[string]int{"pid": pid})
	}
	raw, _ := json.Marshal(map[string]any{"process_info": map[string]any{
		"foreground_process_group_id": pids[0], "foreground_processes": procs,
	}})
	return json.Unmarshal(raw, result)
}

type discoveryFixture struct {
	panes  *fakeHerdrPanes
	status *fakeHerdrSource
	caller *processInfoCaller
	procs  *ttyProcs
	d      *herdrDiscovery
}

func newDiscoveryFixture() *discoveryFixture {
	f := &discoveryFixture{
		panes:  &fakeHerdrPanes{panes: map[string]terminal.PaneRef{}},
		status: newFakeHerdrSource(),
		caller: &processInfoCaller{groups: map[string][]int{}},
		procs:  &ttyProcs{ttys: map[int]string{}},
	}
	f.d = newHerdrDiscovery(f.panes, f.status, f.caller.call, f.procs)
	return f
}

// pane registers a herdr pane on tty running agent as the given foreground pids.
func (f *discoveryFixture) pane(paneID, tty, agent string, pids ...int) herdr.PaneKey {
	f.panes.panes[tty] = terminal.PaneRef{Backend: "herdr", Handle: paneID, MuxSocket: testHerdrSock, TTY: tty}
	key := herdr.PaneKey{Socket: testHerdrSock, PaneID: paneID}
	f.status.set(key, agent, herdr.StatusIdle, herdrT0)
	f.caller.groups[paneID] = pids
	for _, pid := range pids {
		f.procs.ttys[pid] = tty
	}
	return key
}

func TestHerdrDiscoveryShouldFindAnAgentTheScannerCannotClassify(t *testing.T) {
	f := newDiscoveryFixture()
	f.pane("w1:p1", "/dev/pts/7", "pi", 901, 902)

	got, ok := f.d.tick(context.Background())
	if !ok || len(got) != 1 {
		t.Fatalf("tick = %+v, %v; want one candidate", got, ok)
	}
	if got[0].info.PID != 901 || got[0].agent != "pi" || got[0].pane.Handle != "w1:p1" {
		t.Fatalf("candidate = %+v, want pi as pid 901 (the group leader) in w1:p1", got[0])
	}
	if len(f.status.servers) != 1 || f.status.servers[0] != testHerdrSock {
		t.Fatalf("followed servers = %v, want the pane's server", f.status.servers)
	}
}

func TestHerdrDiscoveryShouldLeaveClaudeAndCodexToTheScanner(t *testing.T) {
	f := newDiscoveryFixture()
	f.pane("w1:p1", "/dev/pts/7", "claude", 901)
	f.pane("w1:p2", "/dev/pts/8", "codex", 902)

	if got, _ := f.d.tick(context.Background()); len(got) != 0 {
		t.Fatalf("tick = %+v, want no candidates", got)
	}
}

func TestHerdrDiscoveryShouldSkipAPaneWithNoAgent(t *testing.T) {
	f := newDiscoveryFixture()
	f.pane("w1:p1", "/dev/pts/7", "", 901)
	if got, _ := f.d.tick(context.Background()); len(got) != 0 {
		t.Fatalf("tick = %+v, want no candidates for a plain shell", got)
	}
}

func TestHerdrDiscoveryShouldSkipAPaneWhoseServerIsNotFollowed(t *testing.T) {
	f := newDiscoveryFixture()
	key := f.pane("w1:p1", "/dev/pts/7", "pi", 901)
	f.status.drop(key)
	if got, _ := f.d.tick(context.Background()); len(got) != 0 {
		t.Fatalf("tick = %+v, want none without a live status", got)
	}
}

// Agents run tools in the foreground; the pid found first must stay the
// session's while it lives on the pane.
func TestHerdrDiscoveryShouldKeepTheAgentPIDWhenAToolTakesTheForeground(t *testing.T) {
	f := newDiscoveryFixture()
	f.pane("w1:p1", "/dev/pts/7", "pi", 901)
	f.d.tick(context.Background())

	f.caller.groups["w1:p1"] = []int{955} // a tool now leads the foreground group
	f.procs.ttys[955] = "/dev/pts/7"
	got, _ := f.d.tick(context.Background())
	if len(got) != 1 || got[0].info.PID != 901 {
		t.Fatalf("tick = %+v, want pid 901 kept", got)
	}
	if f.caller.calls != 1 {
		t.Fatalf("process_info calls = %d, want 1 (cached)", f.caller.calls)
	}
}

func TestHerdrDiscoveryShouldLookAgainWhenTheCachedProcessExits(t *testing.T) {
	f := newDiscoveryFixture()
	f.pane("w1:p1", "/dev/pts/7", "pi", 901)
	f.d.tick(context.Background())

	delete(f.procs.ttys, 901)
	f.caller.groups["w1:p1"] = []int{960}
	f.procs.ttys[960] = "/dev/pts/7"
	got, _ := f.d.tick(context.Background())
	if len(got) != 1 || got[0].info.PID != 960 {
		t.Fatalf("tick = %+v, want the new agent pid 960", got)
	}
}

// process_info can name a pid that exits, or is reused elsewhere, before it is
// read.
func TestHerdrDiscoveryShouldRejectAProcessNotOnThePanesTTY(t *testing.T) {
	f := newDiscoveryFixture()
	f.pane("w1:p1", "/dev/pts/7", "pi", 901)
	f.procs.ttys[901] = "/dev/pts/12"
	if got, _ := f.d.tick(context.Background()); len(got) != 0 {
		t.Fatalf("tick = %+v, want none for a process on another tty", got)
	}
}

func TestHerdrDiscoveryShouldKeepTheFollowedServersWhenTheEnumerationFails(t *testing.T) {
	f := newDiscoveryFixture()
	f.pane("w1:p1", "/dev/pts/7", "pi", 901)
	f.d.tick(context.Background())

	f.panes.err = errors.New("herdr: sock_diag failed")
	if _, ok := f.d.tick(context.Background()); ok {
		t.Fatal("tick ok = true, want false on an enumeration error")
	}
	if len(f.status.servers) != 1 {
		t.Fatalf("followed servers = %v, want the set kept", f.status.servers)
	}
}

// runDiscoveryTicks runs the loop for ticks iterations and returns every pid
// it announced, in order.
func runDiscoveryTicks(t *testing.T, f *discoveryFixture, store *state.Store, ticks int, between func(tick int)) []int {
	t.Helper()
	var appeared []int
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tick := 0
	d := f.d
	// Drive the loop one tick at a time through a stepping snapshotter.
	stepper := &steppedPanes{inner: f.panes, before: func() {
		if tick == ticks {
			cancel()
			return
		}
		if between != nil {
			between(tick)
		}
		tick++
	}}
	d.panes = stepper
	runHerdrDiscovery(ctx, store, d, time.Millisecond, func(info osproc.Info, agent string, pane *terminal.PaneRef) {
		appeared = append(appeared, info.PID)
	})
	return appeared
}

// steppedPanes runs a hook before each snapshot so a test can change the world
// between ticks and stop the loop.
type steppedPanes struct {
	inner  *fakeHerdrPanes
	before func()
}

func (s *steppedPanes) Snapshot(ctx context.Context) (map[string]terminal.PaneRef, error) {
	s.before()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return s.inner.Snapshot(ctx)
}

func TestRunHerdrDiscoveryShouldAnnounceEachAgentOnceWhenTicksRepeat(t *testing.T) {
	f := newDiscoveryFixture()
	f.pane("w1:p1", "/dev/pts/7", "pi", 901)
	store := state.New("")
	got := runDiscoveryTicks(t, f, store, 3, nil)
	if len(got) != 1 || got[0] != 901 {
		t.Fatalf("appeared = %v, want pi announced once", got)
	}
}

// A session hydrated from state.json is announced again so this daemon run
// registers its death watch.
func TestRunHerdrDiscoveryShouldReannounceAHydratedAgentOnce(t *testing.T) {
	f := newDiscoveryFixture()
	f.pane("w1:p1", "/dev/pts/7", "pi", 901)
	store := state.New("")
	store.Apply(func(m map[int]*state.Session) { m[901] = &state.Session{PID: 901, Agent: "pi", TTY: "/dev/pts/7"} })
	if got := runDiscoveryTicks(t, f, store, 2, nil); len(got) != 1 || got[0] != 901 {
		t.Fatalf("appeared = %v, want the hydrated pi re-announced once", got)
	}
}

func TestRunHerdrDiscoveryShouldLeaveAPIDTrackedAsAnotherAgent(t *testing.T) {
	f := newDiscoveryFixture()
	f.pane("w1:p1", "/dev/pts/7", "pi", 901)
	store := state.New("")
	store.Apply(func(m map[int]*state.Session) { m[901] = &state.Session{PID: 901, Agent: state.AgentKindClaude} })
	if got := runDiscoveryTicks(t, f, store, 2, nil); len(got) != 0 {
		t.Fatalf("appeared = %v, want nothing over a tracked claude", got)
	}
}

func TestRunHerdrDiscoveryShouldAnnounceAgainWhenAPIDReturnsAsANewAgent(t *testing.T) {
	f := newDiscoveryFixture()
	key := f.pane("w1:p1", "/dev/pts/7", "pi", 901)
	store := state.New("")
	got := runDiscoveryTicks(t, f, store, 3, func(tick int) {
		if tick == 1 {
			f.status.set(key, "opencode", herdr.StatusIdle, herdrT0)
		}
	})
	if len(got) != 2 {
		t.Fatalf("appeared = %v, want pi then opencode", got)
	}
}

func TestProcessIsSessionShouldKeepAHerdrAgentWhileItRunsOnItsTTY(t *testing.T) {
	sess := &state.Session{PID: 901, Agent: "pi", TTY: "/dev/pts/7", Herdr: &state.HerdrInfo{PaneID: "w1:p1", Socket: testHerdrSock}}
	if !processIsSession(osproc.Info{PID: 901, Comm: "node", TTY: "/dev/pts/7"}, sess) {
		t.Fatal("a pi process on its own tty read as not the session")
	}
	if processIsSession(osproc.Info{PID: 901, Comm: "bash", TTY: "/dev/pts/12"}, sess) {
		t.Fatal("a reused pid on another tty read as the session")
	}
	if processIsSession(osproc.Info{PID: 901, Comm: "node"}, &state.Session{PID: 901, Agent: "pi"}) {
		t.Fatal("a session with no tty cannot be vouched for by one")
	}
}

func TestProcessIsSessionShouldStillClassifyClaudeAndCodexRoots(t *testing.T) {
	claude := &state.Session{PID: 4242, Agent: state.AgentKindClaude, TTY: "/dev/pts/7"}
	if processIsSession(osproc.Info{PID: 4242, Comm: "bash", TTY: "/dev/pts/7"}, claude) {
		t.Fatal("a bash process on claude's tty read as the claude session")
	}
	if !processIsSession(osproc.Info{PID: 4242, Comm: "claude"}, claude) {
		t.Fatal("a claude process read as not the claude session")
	}
}

func TestSweepShouldEndAHerdrAgentWhenItsPIDMovesToAnotherTTY(t *testing.T) {
	sink, dir := newTestSink(t)
	m := map[int]*state.Session{901: {PID: 901, Agent: "pi", TTY: "/dev/pts/7"}}
	procs := &ttyProcs{ttys: map[int]string{901: "/dev/pts/12"}}
	sweepDeadSessions(m, procs, sink, func(int) {}, herdrT0)
	if _, ok := m[901]; ok {
		t.Fatal("session kept for a pid now on another tty")
	}
	sink.Close()
	ended := false
	for _, ev := range readEvents(t, dir) {
		ended = ended || ev.Type == "session_end"
	}
	if !ended {
		t.Fatal("no session_end recorded")
	}
}

// An agent herdr alone observes gets its status as a one-node graph, and
// renderers read it through the graph fallback.
func TestApplyHerdrPaneShouldPublishAHerdrOnlyAgentsStatusThroughItsGraph(t *testing.T) {
	sess := &state.Session{PID: 901, Agent: "pi", TTY: "/dev/pts/7"}
	source := newFakeHerdrSource()
	source.set(herdrKey, "pi", herdr.StatusBlocked, herdrT0)
	sink, dir := newTestSink(t)
	ref := herdrRef

	applyHerdrPane(sess, &ref, source, sink, herdrT0.Add(time.Second))
	if sess.AgentGraph == nil || sess.AgentGraph.Summary.Status != state.StatusPermission {
		t.Fatalf("graph = %+v, want a herdr graph reading permission", sess.AgentGraph)
	}
	if sess.Claude != nil || sess.Codex != nil {
		t.Fatal("a pi session grew a claude/codex enrichment block")
	}
	if got := enrichmentID(sess); got == "" || got != sess.AgentGraph.RootID {
		t.Fatalf("enrichmentID = %q, want the graph root id", got)
	}
	evs := transitionsIn(t, sink, dir)
	if len(evs) != 1 || evs[0].To != state.StatusPermission || evs[0].Agent != "pi" || evs[0].SessionID == "" {
		t.Fatalf("transitions = %+v, want one pi edge to permission with a session id", evs)
	}
}

// Once a Pi hook binds the session, its history and its graph are keyed by
// Pi's own session id; an empty block still falls back to the herdr root.
func TestEnrichmentIDShouldBePiSessionIDWhenAPiHookHasBoundTheSession(t *testing.T) {
	sess := &state.Session{PID: 901, Agent: "pi", TTY: "/dev/pts/7"}
	source := newFakeHerdrSource()
	source.set(herdrKey, "pi", herdr.StatusWorking, herdrT0)
	sink, _ := newTestSink(t)
	ref := herdrRef
	applyHerdrPane(sess, &ref, source, sink, herdrT0.Add(time.Second))
	herdrRoot := sess.AgentGraph.RootID

	sess.AgentBlock(state.AgentKindPi)
	if got := enrichmentID(sess); got != herdrRoot {
		t.Fatalf("unbound enrichmentID = %q, want the herdr root %q", got, herdrRoot)
	}
	sess.Pi.SessionID = "0199a1b2-pi"
	source.set(herdrKey, "pi", herdr.StatusIdle, herdrT0.Add(2*time.Second))
	applyHerdrPane(sess, &ref, source, sink, herdrT0.Add(3*time.Second))
	if got := enrichmentID(sess); got != "0199a1b2-pi" || sess.AgentGraph.RootID != "0199a1b2-pi" {
		t.Fatalf("bound enrichmentID = %q, root = %q, want Pi's session id for both", got, sess.AgentGraph.RootID)
	}
}
