package federation

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/state"
)

type fakeRemote struct {
	mu    sync.RWMutex
	data  map[string]state.Snapshot
	subs  map[chan map[string]state.Snapshot]struct{}
	ready chan struct{}
	once  sync.Once
}

func newFakeRemote() *fakeRemote {
	return &fakeRemote{
		data: make(map[string]state.Snapshot), subs: make(map[chan map[string]state.Snapshot]struct{}),
		ready: make(chan struct{}),
	}
}

func (r *fakeRemote) Snapshot() map[string]state.Snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]state.Snapshot, len(r.data))
	for host, snapshot := range r.data {
		out[host] = snapshot
	}
	return out
}

func (r *fakeRemote) Subscribe() (<-chan map[string]state.Snapshot, func()) {
	ch := make(chan map[string]state.Snapshot, 4)
	r.mu.Lock()
	r.subs[ch] = struct{}{}
	r.mu.Unlock()
	r.once.Do(func() { close(r.ready) })
	return ch, func() {
		r.mu.Lock()
		if _, ok := r.subs[ch]; ok {
			delete(r.subs, ch)
			close(ch)
		}
		r.mu.Unlock()
	}
}

func (r *fakeRemote) replace(data map[string]state.Snapshot) {
	r.mu.Lock()
	r.data = data
	for ch := range r.subs {
		ch <- data
	}
	r.mu.Unlock()
}

func testSession(pid int, startedAt time.Time) state.Session {
	return state.Session{PID: pid, StartedAt: startedAt, CWD: "/work", TTY: "/dev/pts/1"}
}

func TestViewNamespacesAndDropsRemoteSnapshots(t *testing.T) {
	local := state.New("")
	localStart := time.Unix(10, 0)
	remoteStart := time.Unix(20, 0)
	local.Apply(func(sessions map[int]*state.Session) {
		session := testSession(42, localStart)
		sessions[42] = &session
	})
	remote := newFakeRemote()
	remoteSession := testSession(42, remoteStart)
	remoteSession.Wezterm = &state.WeztermInfo{MuxPID: 100, PaneID: 2}
	remoteSession.Hyprland = &state.HyprlandInfo{Address: "remote-window"}
	remote.replace(map[string]state.Snapshot{
		"zeta":  {Sessions: []state.Session{remoteSession}},
		"alpha": {Sessions: []state.Session{testSession(7, remoteStart)}},
		// A remote source may not impersonate the local namespace.
		"local": {Sessions: []state.Session{testSession(99, remoteStart)}},
	})
	view, err := NewView(local, "local", remote)
	if err != nil {
		t.Fatal(err)
	}
	view.SetRouteReady(func(host string, pid int, _ time.Time) bool { return host == "zeta" && pid == 42 })

	snapshot := view.Snapshot()
	if len(snapshot.Sessions) != 3 {
		t.Fatalf("sessions = %+v", snapshot.Sessions)
	}
	got := []string{snapshot.Sessions[0].Hostname, snapshot.Sessions[1].Hostname, snapshot.Sessions[2].Hostname}
	want := []string{"local", "alpha", "zeta"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("hosts = %v, want %v", got, want)
		}
	}
	if snapshot.Sessions[2].Wezterm != nil || snapshot.Sessions[2].Hyprland != nil {
		t.Fatalf("remote desktop refs leaked into aggregate: %+v", snapshot.Sessions[2])
	}
	if !snapshot.Sessions[0].Navigable || snapshot.Sessions[1].Navigable || !snapshot.Sessions[2].Navigable {
		t.Fatalf("route-ready projection = %+v", snapshot.Sessions)
	}
	if snapshot.Sessions[0].Remote || !snapshot.Sessions[1].Remote || !snapshot.Sessions[2].Remote {
		t.Fatalf("local/remote origin projection = %+v", snapshot.Sessions)
	}

	remote.replace(map[string]state.Snapshot{})
	snapshot = view.Snapshot()
	if len(snapshot.Sessions) != 1 || snapshot.Sessions[0].Hostname != "local" {
		t.Fatalf("after disconnect = %+v", snapshot.Sessions)
	}
}

func TestViewRemoteFocusRequiresExactProcessLifetime(t *testing.T) {
	local := state.New("")
	local.Apply(func(sessions map[int]*state.Session) {
		session := testSession(1, time.Unix(1, 0))
		session.Focused = true
		sessions[1] = &session
	})
	startedAt := time.Unix(2, 0)
	remote := newFakeRemote()
	remote.replace(map[string]state.Snapshot{"remote": {Sessions: []state.Session{testSession(1, startedAt)}}})
	view, _ := NewView(local, "local", remote)
	view.SetRouteReady(func(string, int, time.Time) bool { return true })

	view.SetRemoteFocus("remote", 1, startedAt.Add(time.Second))
	snapshot := view.Snapshot()
	if !snapshot.Sessions[0].Focused || snapshot.Sessions[1].Focused {
		t.Fatalf("stale binding marked a row focused: %+v", snapshot.Sessions)
	}

	view.SetRemoteFocus("remote", 1, startedAt)
	snapshot = view.Snapshot()
	if snapshot.Sessions[0].Focused || !snapshot.Sessions[1].Focused {
		t.Fatalf("exact remote focus not projected: %+v", snapshot.Sessions)
	}

	// The same identity may reappear after a remote daemon restart because its
	// StartedAt is a discovery lifetime, not a kernel birth certificate. The
	// disconnect edge must forget the old focus observation explicitly.
	view.DropRemoteHost("remote")
	snapshot = view.Snapshot()
	if !snapshot.Sessions[0].Focused || snapshot.Sessions[1].Focused {
		t.Fatalf("disconnect did not clear focus overlay: %+v", snapshot.Sessions)
	}
}

func TestViewRunPublishesSourceReplacement(t *testing.T) {
	local := state.New("")
	remote := newFakeRemote()
	view, _ := NewView(local, "local", remote)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go view.Run(ctx)
	<-remote.ready

	updates, unsubscribe := view.Subscribe()
	defer unsubscribe()
	remote.replace(map[string]state.Snapshot{"remote": {Sessions: []state.Session{testSession(8, time.Unix(8, 0))}}})
	select {
	case <-updates:
		// Subscribe values are wakeups, not ordered revisions. Run's initial
		// publication can already be queued here, so re-read the authoritative
		// aggregate after the notification instead of trusting its payload.
		snapshot := view.Snapshot()
		if len(snapshot.Sessions) != 1 || snapshot.Sessions[0].Hostname != "remote" {
			t.Fatalf("snapshot = %+v", snapshot)
		}
	case <-time.After(time.Second):
		t.Fatal("no aggregate update")
	}
}

func TestViewSaturationKeepsFinalReplacement(t *testing.T) {
	local := state.New("")
	remote := newFakeRemote()
	view, _ := NewView(local, "local", remote)
	updates, unsubscribe := view.Subscribe()
	defer unsubscribe()

	for pid := 1; pid <= 9; pid++ {
		remote.replace(map[string]state.Snapshot{
			"remote": {Sessions: []state.Session{testSession(pid, time.Unix(int64(pid), 0))}},
		})
		view.publish()
	}
	if len(updates) == 0 {
		t.Fatal("saturated subscriber lost its wakeup")
	}
	frame := view.CurrentFrame()
	if len(frame.Snapshot.Sessions) != 1 || frame.Snapshot.Sessions[0].PID != 9 {
		t.Fatalf("current frame = %+v", frame.Snapshot.Sessions)
	}
}

func TestLateUnfocusCannotClearNewerWindow(t *testing.T) {
	remote := newFakeRemote()
	startedAt := time.Unix(2, 0)
	remote.replace(map[string]state.Snapshot{"remote": {Sessions: []state.Session{testSession(1, startedAt)}}})
	view, _ := NewView(state.New(""), "local", remote)
	view.SetRouteReady(func(string, int, time.Time) bool { return true })

	view.SetRemoteFocusFrom("gui:old-window:pane", "remote", 1, startedAt)
	view.SetRemoteFocusFrom("gui:new-window:pane", "remote", 1, startedAt)
	view.ClearRemoteFocusFrom("gui:old-window:pane")
	if !view.Snapshot().Sessions[0].Focused {
		t.Fatal("late old-window unfocus cleared the newer focus observation")
	}
	view.ClearRemoteFocusFrom("gui:new-window:pane")
	if view.Snapshot().Sessions[0].Focused {
		t.Fatal("matching unfocus did not clear the focus observation")
	}
}

func TestRunReadyPublishesQuietStateThatPredatedSubscription(t *testing.T) {
	remote := newFakeRemote()
	remote.replace(map[string]state.Snapshot{
		"remote": {Sessions: []state.Session{testSession(7, time.Unix(7, 0))}},
	})
	view, _ := NewView(state.New(""), "local", remote)
	updates, unsubscribe := view.Subscribe()
	defer unsubscribe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	go view.RunReady(ctx, ready)
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("view never became ready")
	}
	select {
	case <-updates:
		frame := view.CurrentFrame()
		if len(frame.Snapshot.Sessions) != 1 || frame.Snapshot.Sessions[0].PID != 7 {
			t.Fatalf("initial publication = %+v", frame.Snapshot.Sessions)
		}
		if len(frame.JSON) == 0 {
			t.Fatal("initial publication did not cache its shared encoding")
		}
	case <-time.After(time.Second):
		t.Fatal("quiet pre-subscription state was never published")
	}
}

func TestViewOrdersRemoteRowsByTheLocalWorkspaceDisplayingThem(t *testing.T) {
	local := state.New("")
	local.Apply(func(sessions map[int]*state.Session) {
		near := testSession(11, time.Unix(10, 0))
		near.Hyprland = &state.HyprlandInfo{Address: "0xaa", Workspace: "2", WorkspaceID: 2}
		far := testSession(12, time.Unix(20, 0))
		far.Hyprland = &state.HyprlandInfo{Address: "0xbb", Workspace: "5", WorkspaceID: 5}
		sessions[11], sessions[12] = &near, &far
	})
	startedAt := time.Unix(30, 0)
	remote := newFakeRemote()
	// The remote row arrives stamped with ITS desktop's workspace 9, which says
	// nothing about this machine; the SSH pane showing it lives in a local
	// window on workspace 3.
	displayed := testSession(70, startedAt)
	displayed.Hyprland = &state.HyprlandInfo{Address: "0xremote", Workspace: "9", WorkspaceID: 9}
	unbound := testSession(71, startedAt)
	remote.replace(map[string]state.Snapshot{
		"buildbox": {Sessions: []state.Session{displayed, unbound}},
	})
	view, err := NewView(local, "local", remote)
	if err != nil {
		t.Fatal(err)
	}
	view.SetRouteReady(func(string, int, time.Time) bool { return true })
	view.SetRouteWorkspace(func(host string, pid int, started time.Time) (int, bool) {
		if host == "buildbox" && pid == 70 && started.Equal(startedAt) {
			return 3, true
		}
		return 0, false
	})

	got := view.Snapshot().Sessions
	// Workspace order across both origins; the remote row whose local window is
	// unknown keeps its old place at the end.
	want := []int{11, 70, 12, 71}
	if len(got) != len(want) {
		t.Fatalf("sessions = %+v", got)
	}
	for i, pid := range want {
		if got[i].PID != pid {
			t.Fatalf("chip order = %v, want %v", pids(got), want)
		}
	}
	if got[1].Hyprland != nil {
		t.Fatalf("remote desktop window leaked into the aggregate: %+v", got[1].Hyprland)
	}
	// Having stripped the remote coordinates, the aggregate must still publish
	// the workspace a reader could act on — otherwise a renderer has nothing to
	// report but a dash.
	if got[1].LocalWorkspace != 3 {
		t.Errorf("displayed remote row LocalWorkspace = %d, want 3", got[1].LocalWorkspace)
	}
	if got[3].LocalWorkspace != 0 {
		t.Errorf("unbound remote row LocalWorkspace = %d, want 0 (unresolved)", got[3].LocalWorkspace)
	}
	// Local rows keep carrying their own Hyprland block and must not be
	// stamped with a federation-only field.
	for _, s := range []state.Session{got[0], got[2]} {
		if s.LocalWorkspace != 0 {
			t.Errorf("local row %d got LocalWorkspace = %d, want 0", s.PID, s.LocalWorkspace)
		}
	}
}

// Pointer identity is the only assertion that distinguishes one shared encode
// from equal bytes independently encoded for each subscriber.
func TestViewEncodesTheAggregateOncePerPublishRegardlessOfSubscriberCount(t *testing.T) {
	remote := newFakeRemote()
	remote.replace(map[string]state.Snapshot{
		"remote": {Sessions: []state.Session{testSession(7, time.Unix(7, 0))}},
	})
	view, _ := NewView(state.New(""), "local", remote)
	const subscribers = 10
	updates := make([]<-chan struct{}, 0, subscribers)
	for i := 0; i < subscribers; i++ {
		ch, cancel := view.Subscribe()
		defer cancel()
		updates = append(updates, ch)
	}

	view.publish()
	var shared []byte
	for i, ch := range updates {
		if len(ch) == 0 {
			t.Fatalf("subscriber %d received no wakeup", i)
		}
		<-ch
		frame := view.CurrentFrame()
		if len(frame.JSON) == 0 {
			t.Fatalf("subscriber %d read no shared encoding", i)
		}
		if i == 0 {
			shared = frame.JSON
		} else if &frame.JSON[0] != &shared[0] {
			t.Fatalf("subscriber %d read a separately encoded frame", i)
		}
	}
}

func TestViewDoesNotFanOutARemoteFrameThatDiffersOnlyBySpinnerOrObservedAt(t *testing.T) {
	at := time.Date(2026, 8, 28, 12, 0, 1, 0, time.UTC)
	remote := newFakeRemote()
	session := testSession(7, at.Add(-time.Hour))
	session.Wezterm = &state.WeztermInfo{WindowTitle: "⠹ codex"}
	session.AgentGraph = &state.AgentGraph{
		RootID: "root", ObservedAt: at, FreshUntil: at.Add(time.Second), Complete: true,
		Summary: state.AgentGraphSummary{Runtime: agentgraph.RuntimeIdle, Status: state.StatusIdle},
		Nodes:   []state.AgentNode{{ID: "root", Runtime: agentgraph.RuntimeIdle, UpdatedAt: at}},
	}
	remote.replace(map[string]state.Snapshot{"remote": {Sessions: []state.Session{session}}})
	view, _ := NewView(state.New(""), "local", remote)
	updates, cancel := view.Subscribe()
	defer cancel()
	view.publish()
	<-updates

	// Remote terminal metadata is discarded by the aggregate projection, while
	// SnapshotChangeKey deliberately ignores graph observation clocks and sees
	// both freshness instants through the same five-second wire ceiling.
	spun := session
	spun.Wezterm = &state.WeztermInfo{WindowTitle: "⠸ codex"}
	graph := *session.AgentGraph
	graph.ObservedAt = at.Add(time.Second)
	graph.FreshUntil = at.Add(2 * time.Second)
	graph.Nodes = append([]state.AgentNode(nil), session.AgentGraph.Nodes...)
	graph.Nodes[0].UpdatedAt = at.Add(time.Second)
	spun.AgentGraph = &graph
	remote.replace(map[string]state.Snapshot{"remote": {Sessions: []state.Session{spun}}})
	view.publish()
	if len(updates) != 0 {
		t.Fatal("advisory remote clocks or discarded spinner metadata triggered fanout")
	}

	graph.Summary.Status = state.StatusWorking
	spun.AgentGraph = &graph
	remote.replace(map[string]state.Snapshot{"remote": {Sessions: []state.Session{spun}}})
	view.publish()
	if len(updates) == 0 {
		t.Fatal("observable graph status change was suppressed")
	}
}

func TestViewStillDeliversTheFinalFrameOnRemoteDisconnect(t *testing.T) {
	remote := newFakeRemote()
	view, _ := NewView(state.New(""), "local", remote)
	updates, cancel := view.Subscribe()
	defer cancel()
	for pid := 1; pid <= 9; pid++ {
		remote.replace(map[string]state.Snapshot{
			"remote": {Sessions: []state.Session{testSession(pid, time.Unix(int64(pid), 0))}},
		})
		view.publish()
	}
	remote.replace(map[string]state.Snapshot{})
	view.publish()
	if len(updates) == 0 {
		t.Fatal("saturated subscriber lost its wakeup")
	}
	if got := view.CurrentFrame().Snapshot.Sessions; len(got) != 0 {
		t.Fatalf("final disconnect frame retained sessions: %+v", got)
	}
}

func TestViewCurrentFrameNeverPrecedesAnEarlierRead(t *testing.T) {
	remote := newFakeRemote()
	view, _ := NewView(state.New(""), "local", remote)
	updates, cancel := view.Subscribe()
	defer cancel()
	for pid := 1; pid <= 2; pid++ {
		remote.replace(map[string]state.Snapshot{
			"remote": {Sessions: []state.Session{testSession(pid, time.Unix(int64(pid), 0))}},
		})
		view.publish()
	}
	if got := view.CurrentFrame().Snapshot.Sessions[0].PID; got != 2 {
		t.Fatalf("initial current frame PID = %d, want 2", got)
	}
	for len(updates) > 0 {
		<-updates
		if got := view.CurrentFrame().Snapshot.Sessions[0].PID; got != 2 {
			t.Fatalf("queued wakeup moved current frame backward to PID %d", got)
		}
	}
}

func TestViewCurrentFrameEncodingMatchesItsSnapshot(t *testing.T) {
	remote := newFakeRemote()
	remote.replace(map[string]state.Snapshot{
		"remote": {Sessions: []state.Session{testSession(9, time.Unix(9, 0))}},
	})
	view, _ := NewView(state.New(""), "local", remote)
	view.publish()
	frame := view.CurrentFrame()
	want, err := json.Marshal(frame.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(frame.JSON, want) {
		t.Fatalf("shared encoding does not match its snapshot\n got: %s\nwant: %s", frame.JSON, want)
	}
}

func pids(sessions []state.Session) []int {
	out := make([]int, len(sessions))
	for i := range sessions {
		out[i] = sessions[i].PID
	}
	return out
}
