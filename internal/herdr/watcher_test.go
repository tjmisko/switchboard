package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeServer speaks herdr's socket protocol as observed live (0.9.1): one
// request per connection, subscription acks as subscription_started, status
// events dotted and lifecycle events underscored.
type fakeServer struct {
	t      *testing.T
	socket string
	ln     net.Listener

	mu         sync.Mutex
	panes      []Pane
	subs       []net.Conn
	subscribed chan []string // pane ids of each accepted subscription
	lists      int
}

func newFakeServer(t *testing.T, panes ...Pane) *fakeServer {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "herdr.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeServer{t: t, socket: socket, ln: ln, panes: panes, subscribed: make(chan []string, 16)}
	go s.serve()
	t.Cleanup(func() { s.close() })
	return s
}

func (s *fakeServer) close() {
	_ = s.ln.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.subs {
		_ = c.Close()
	}
}

func (s *fakeServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *fakeServer) handle(conn net.Conn) {
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		_ = conn.Close()
		return
	}
	var req struct {
		ID     string `json:"id"`
		Method string `json:"method"`
		Params struct {
			Subscriptions []map[string]string `json:"subscriptions"`
		} `json:"params"`
	}
	_ = json.Unmarshal(line, &req)
	switch req.Method {
	case "pane.list":
		s.mu.Lock()
		s.lists++
		body, _ := json.Marshal(map[string]any{"id": req.ID, "result": map[string]any{"type": "pane_list", "panes": s.panes}})
		s.mu.Unlock()
		_, _ = conn.Write(append(body, '\n'))
		_ = conn.Close()
	case "events.subscribe":
		var ids []string
		s.mu.Lock()
		known := make(map[string]bool)
		for _, p := range s.panes {
			known[p.PaneID] = true
		}
		for _, sub := range req.Params.Subscriptions {
			if id, ok := sub["pane_id"]; ok {
				if !known[id] {
					s.mu.Unlock()
					_, _ = conn.Write([]byte(`{"id":"` + req.ID + `:sub:0:probe","error":{"code":"pane_not_found","message":"pane ` + id + ` not found"}}` + "\n"))
					_ = conn.Close()
					return
				}
				ids = append(ids, id)
			}
		}
		s.subs = append(s.subs, conn)
		s.mu.Unlock()
		_, _ = conn.Write([]byte(`{"id":"` + req.ID + `","result":{"type":"subscription_started"}}` + "\n"))
		s.subscribed <- ids
	default:
		_ = conn.Close()
	}
}

// push writes one raw event line to every open subscription.
func (s *fakeServer) push(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.subs {
		_, _ = c.Write([]byte(line + "\n"))
	}
}

func (s *fakeServer) setPanes(panes ...Pane) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.panes = panes
}

// dropSubscriptions closes every open stream, as a restarting server would.
func (s *fakeServer) dropSubscriptions() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.subs {
		_ = c.Close()
	}
	s.subs = nil
}

func (s *fakeServer) awaitSubscription(t *testing.T) []string {
	t.Helper()
	select {
	case ids := <-s.subscribed:
		return ids
	case <-time.After(3 * time.Second):
		t.Fatal("no subscription arrived")
		return nil
	}
}

func agentPane(id, agent, status string) Pane {
	return Pane{PaneID: id, TerminalID: "term_" + id, Agent: &agent, AgentStatus: status}
}

func startWatcher(t *testing.T, grace time.Duration, sockets ...string) *Watcher {
	t.Helper()
	w := newWatcher(Call, func(ctx context.Context, socket string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}, time.Now, grace)
	w.SetServers(context.Background(), sockets)
	t.Cleanup(w.Close)
	return w
}

// eventually polls cond until it holds or the deadline passes.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func statusIs(w *Watcher, key PaneKey, want string) func() bool {
	return func() bool {
		got, ok := w.Status(key)
		return ok && got.Status == want
	}
}

func TestWatcherShouldReportTheListedStatusWhenSubscribed(t *testing.T) {
	s := newFakeServer(t, agentPane("w1:p1", "claude", StatusIdle))
	w := startWatcher(t, time.Minute, s.socket)
	if ids := s.awaitSubscription(t); len(ids) != 1 || ids[0] != "w1:p1" {
		t.Fatalf("subscribed panes = %v, want [w1:p1]", ids)
	}
	key := PaneKey{Socket: s.socket, PaneID: "w1:p1"}
	eventually(t, "idle status", statusIs(w, key, StatusIdle))
	if got, _ := w.Status(key); got.Agent != "claude" {
		t.Fatalf("agent = %q, want claude", got.Agent)
	}
}

func TestWatcherShouldApplyAStatusEventWhenTheAgentChangesState(t *testing.T) {
	s := newFakeServer(t, agentPane("w1:p1", "claude", StatusIdle))
	w := startWatcher(t, time.Minute, s.socket)
	s.awaitSubscription(t)
	key := PaneKey{Socket: s.socket, PaneID: "w1:p1"}
	eventually(t, "idle status", statusIs(w, key, StatusIdle))

	s.push(`{"data":{"agent":"claude","agent_status":"blocked","pane_id":"w1:p1","workspace_id":"w1"},"event":"pane.agent_status_changed"}`)
	eventually(t, "blocked status", statusIs(w, key, StatusBlocked))
}

func TestWatcherShouldSignalAChangeWhenAStatusEventArrives(t *testing.T) {
	s := newFakeServer(t, agentPane("w1:p1", "pi", StatusIdle))
	w := startWatcher(t, time.Minute, s.socket)
	s.awaitSubscription(t)
	key := PaneKey{Socket: s.socket, PaneID: "w1:p1"}
	eventually(t, "idle status", statusIs(w, key, StatusIdle))
	for len(w.Changes()) > 0 {
		<-w.Changes()
	}

	s.push(`{"data":{"agent":"pi","agent_status":"working","pane_id":"w1:p1","workspace_id":"w1"},"event":"pane.agent_status_changed"}`)
	select {
	case got := <-w.Changes():
		if got != key {
			t.Fatalf("change = %+v, want %+v", got, key)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no change signalled")
	}
}

// An agent that exits reports unknown with no agent field (seen live).
func TestWatcherShouldClearTheAgentWhenAStatusEventCarriesNone(t *testing.T) {
	s := newFakeServer(t, agentPane("w1:p1", "claude", StatusIdle))
	w := startWatcher(t, time.Minute, s.socket)
	s.awaitSubscription(t)
	key := PaneKey{Socket: s.socket, PaneID: "w1:p1"}
	eventually(t, "idle status", statusIs(w, key, StatusIdle))

	s.push(`{"data":{"agent_status":"unknown","pane_id":"w1:p1","workspace_id":"w1"},"event":"pane.agent_status_changed"}`)
	eventually(t, "unknown status", statusIs(w, key, StatusUnknown))
	if got, _ := w.Status(key); got.Agent != "" {
		t.Fatalf("agent = %q, want none", got.Agent)
	}
}

func TestWatcherShouldResubscribeWithTheNewPaneWhenAPaneIsCreated(t *testing.T) {
	s := newFakeServer(t, agentPane("w1:p1", "claude", StatusIdle))
	w := startWatcher(t, time.Minute, s.socket)
	s.awaitSubscription(t)

	s.setPanes(agentPane("w1:p1", "claude", StatusIdle), agentPane("w1:p2", "opencode", StatusWorking))
	s.push(`{"data":{"pane_id":"w1:p2","type":"pane_created","workspace_id":"w1"},"event":"pane_created"}`)
	if ids := s.awaitSubscription(t); len(ids) != 2 {
		t.Fatalf("resubscribed panes = %v, want both", ids)
	}
	eventually(t, "the new pane's status", statusIs(w, PaneKey{Socket: s.socket, PaneID: "w1:p2"}, StatusWorking))
}

func TestWatcherShouldForgetAPaneWhenItCloses(t *testing.T) {
	s := newFakeServer(t, agentPane("w1:p1", "claude", StatusIdle), agentPane("w1:p2", "pi", StatusIdle))
	w := startWatcher(t, time.Minute, s.socket)
	s.awaitSubscription(t)
	gone := PaneKey{Socket: s.socket, PaneID: "w1:p2"}
	eventually(t, "the pane's status", statusIs(w, gone, StatusIdle))

	s.setPanes(agentPane("w1:p1", "claude", StatusIdle))
	s.push(`{"data":{"pane_id":"w1:p2","type":"pane_closed","workspace_id":"w1"},"event":"pane_closed"}`)
	s.awaitSubscription(t)
	eventually(t, "the closed pane forgotten", func() bool { _, ok := w.Status(gone); return !ok })
}

// A pane that closes between the list and the subscribe makes herdr reject the
// whole request; the watcher must list again rather than give up.
func TestWatcherShouldRetryWhenThePaneSetChangesBeforeTheSubscriptionIsAccepted(t *testing.T) {
	s := newFakeServer(t, agentPane("w1:p1", "claude", StatusIdle))
	stale := []Pane{agentPane("w1:p1", "claude", StatusIdle), agentPane("w9:p9", "pi", StatusIdle)}
	listed := 0
	call := func(ctx context.Context, socket, method string, params, result any) error {
		listed++
		if listed == 1 {
			raw, _ := json.Marshal(map[string]any{"panes": stale})
			return json.Unmarshal(raw, result)
		}
		return Call(ctx, socket, method, params, result)
	}
	w := newWatcher(call, func(ctx context.Context, socket string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}, time.Now, time.Minute)
	w.SetServers(context.Background(), []string{s.socket})
	t.Cleanup(w.Close)

	if ids := s.awaitSubscription(t); len(ids) != 1 || ids[0] != "w1:p1" {
		t.Fatalf("subscribed panes = %v, want only the live w1:p1", ids)
	}
}

func TestWatcherShouldKeepStatusesDuringGraceWhenTheServerDropsTheStream(t *testing.T) {
	s := newFakeServer(t, agentPane("w1:p1", "claude", StatusWorking))
	w := startWatcher(t, time.Minute, s.socket)
	s.awaitSubscription(t)
	key := PaneKey{Socket: s.socket, PaneID: "w1:p1"}
	eventually(t, "working status", statusIs(w, key, StatusWorking))

	s.close() // the server goes away entirely
	time.Sleep(50 * time.Millisecond)
	if _, ok := w.Status(key); !ok {
		t.Fatal("status dropped immediately; want it kept within the grace period")
	}
}

func TestWatcherShouldStopReportingAfterGraceWhenTheServerStaysDown(t *testing.T) {
	s := newFakeServer(t, agentPane("w1:p1", "claude", StatusWorking))
	w := startWatcher(t, 30*time.Millisecond, s.socket)
	s.awaitSubscription(t)
	key := PaneKey{Socket: s.socket, PaneID: "w1:p1"}
	eventually(t, "working status", statusIs(w, key, StatusWorking))

	s.close()
	eventually(t, "status dropped after grace", func() bool { _, ok := w.Status(key); return !ok })
}

func TestWatcherShouldReconnectWhenTheStreamDropsAndTheServerIsStillUp(t *testing.T) {
	s := newFakeServer(t, agentPane("w1:p1", "claude", StatusIdle))
	w := startWatcher(t, time.Minute, s.socket)
	s.awaitSubscription(t)

	s.setPanes(agentPane("w1:p1", "claude", StatusBlocked))
	s.dropSubscriptions()
	s.awaitSubscription(t)
	eventually(t, "status re-read after reconnect", statusIs(w, PaneKey{Socket: s.socket, PaneID: "w1:p1"}, StatusBlocked))
}

func TestWatcherShouldReconnectWhenHerdrReportsLostEvents(t *testing.T) {
	s := newFakeServer(t, agentPane("w1:p1", "claude", StatusIdle))
	w := startWatcher(t, time.Minute, s.socket)
	s.awaitSubscription(t)

	s.setPanes(agentPane("w1:p1", "claude", StatusWorking))
	s.push(`{"id":"switchboard","error":{"code":"events_lost","message":"subscriber fell behind"}}`)
	s.awaitSubscription(t)
	eventually(t, "status re-read after events_lost", statusIs(w, PaneKey{Socket: s.socket, PaneID: "w1:p1"}, StatusWorking))
}

func TestWatcherShouldForgetAServerWhenItIsNoLongerWatched(t *testing.T) {
	s := newFakeServer(t, agentPane("w1:p1", "claude", StatusIdle))
	w := startWatcher(t, time.Minute, s.socket)
	s.awaitSubscription(t)
	key := PaneKey{Socket: s.socket, PaneID: "w1:p1"}
	eventually(t, "idle status", statusIs(w, key, StatusIdle))

	w.SetServers(context.Background(), nil)
	if _, ok := w.Status(key); ok {
		t.Fatal("status still reported for an unwatched server")
	}
}

func TestWatcherShouldReportNothingForAPaneItHasNotSeen(t *testing.T) {
	s := newFakeServer(t, agentPane("w1:p1", "claude", StatusIdle))
	w := startWatcher(t, time.Minute, s.socket)
	s.awaitSubscription(t)
	if _, ok := w.Status(PaneKey{Socket: s.socket, PaneID: "w5:p5"}); ok {
		t.Fatal("status reported for an unknown pane")
	}
	if _, ok := w.Status(PaneKey{Socket: "/elsewhere.sock", PaneID: "w1:p1"}); ok {
		t.Fatal("status reported for an unwatched socket")
	}
}
