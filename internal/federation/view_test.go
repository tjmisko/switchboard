package federation

import (
	"context"
	"sync"
	"testing"
	"time"

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
	var last state.Snapshot
	for len(updates) > 0 {
		last = <-updates
	}
	if len(last.Sessions) != 1 || last.Sessions[0].PID != 9 {
		t.Fatalf("last queued replacement = %+v", last.Sessions)
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
	case snapshot := <-updates:
		if len(snapshot.Sessions) != 1 || snapshot.Sessions[0].PID != 7 {
			t.Fatalf("initial publication = %+v", snapshot.Sessions)
		}
	case <-time.After(time.Second):
		t.Fatal("quiet pre-subscription state was never published")
	}
}
