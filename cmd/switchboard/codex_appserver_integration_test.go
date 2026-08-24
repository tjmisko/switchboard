package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/provider"
	codexprovider "github.com/tjmisko/switchboard/internal/provider/codex"
	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/state"
)

type integrationCodexEnvironment struct{}

func (integrationCodexEnvironment) Environ(context.Context, provider.RootKey) ([]byte, error) {
	return nil, errors.New("process environment unavailable in integration test")
}

type integrationCodexStatus struct {
	Type        string   `json:"type"`
	ActiveFlags []string `json:"activeFlags,omitempty"`
}

type integrationCodexAgentState struct {
	Status string `json:"status"`
}

type integrationCodexItem struct {
	Type              string                                `json:"type"`
	SenderThreadID    string                                `json:"senderThreadId"`
	ReceiverThreadIDs []string                              `json:"receiverThreadIds"`
	AgentsStates      map[string]integrationCodexAgentState `json:"agentsStates"`
}

type integrationCodexTurn struct {
	ID    string                 `json:"id"`
	Items []integrationCodexItem `json:"items,omitempty"`
}

type integrationCodexThread struct {
	ID             string                 `json:"id"`
	ParentThreadID string                 `json:"parentThreadId,omitempty"`
	AgentNickname  string                 `json:"agentNickname,omitempty"`
	AgentRole      string                 `json:"agentRole,omitempty"`
	Status         integrationCodexStatus `json:"status"`
	Turns          []integrationCodexTurn `json:"turns,omitempty"`
}

type integrationCodexConnection struct {
	server    net.Conn
	encoder   *json.Encoder
	writeMu   sync.Mutex
	closeOnce sync.Once
}

func (c *integrationCodexConnection) write(value any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.encoder.Encode(value)
}

func (c *integrationCodexConnection) close() {
	c.closeOnce.Do(func() { _ = c.server.Close() })
}

// integrationCodexAppServer is a JSONL peer behind the observer's public
// Connector seam. It implements only the allowlisted initialize/read/list and
// turns-list surface and never invokes an installed Codex binary or a live user
// service.
type integrationCodexAppServer struct {
	mu sync.Mutex

	available   bool
	root        json.RawMessage
	descendants []json.RawMessage
	active      *integrationCodexConnection
	connections int
	readRoots   []string
	listRoots   []string
}

func newIntegrationCodexAppServer(t *testing.T, root integrationCodexThread, descendants []integrationCodexThread) *integrationCodexAppServer {
	t.Helper()
	server := &integrationCodexAppServer{available: true}
	server.setSnapshot(t, root, descendants)
	return server
}

func (s *integrationCodexAppServer) Connect(ctx context.Context) (codexprovider.Connection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if !s.available {
		s.mu.Unlock()
		return nil, errors.New("fake Codex app-server unavailable")
	}
	client, peer := net.Pipe()
	connection := &integrationCodexConnection{server: peer, encoder: json.NewEncoder(peer)}
	s.active = connection
	s.connections++
	s.mu.Unlock()
	go s.serve(connection)
	return client, nil
}

func (s *integrationCodexAppServer) serve(connection *integrationCodexConnection) {
	decoder := json.NewDecoder(connection.server)
	for {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := decoder.Decode(&request); err != nil {
			return
		}

		var result json.RawMessage
		switch request.Method {
		case "initialize":
			result = integrationMustJSON(map[string]string{"userAgent": "codex_app_server/0.149.0"})
		case "initialized":
			continue
		case "thread/read":
			var params struct {
				ThreadID string `json:"threadId"`
			}
			_ = json.Unmarshal(request.Params, &params)
			s.mu.Lock()
			s.readRoots = append(s.readRoots, params.ThreadID)
			root := append(json.RawMessage(nil), s.root...)
			s.mu.Unlock()
			result = integrationMustJSON(map[string]json.RawMessage{"thread": root})
		case "thread/turns/list":
			s.mu.Lock()
			rootBody := append(json.RawMessage(nil), s.root...)
			s.mu.Unlock()
			var root integrationCodexThread
			_ = json.Unmarshal(rootBody, &root)
			result = integrationMustJSON(struct {
				Data []integrationCodexTurn `json:"data"`
			}{Data: root.Turns})
		case "thread/list":
			var params struct {
				AncestorThreadID string `json:"ancestorThreadId"`
			}
			_ = json.Unmarshal(request.Params, &params)
			s.mu.Lock()
			s.listRoots = append(s.listRoots, params.AncestorThreadID)
			descendants := cloneIntegrationRawMessages(s.descendants)
			s.mu.Unlock()
			result = integrationMustJSON(struct {
				Data []json.RawMessage `json:"data"`
			}{Data: descendants})
		default:
			continue
		}
		if len(request.ID) == 0 {
			continue
		}
		if err := connection.write(struct {
			ID     json.RawMessage `json:"id"`
			Result json.RawMessage `json:"result"`
		}{ID: request.ID, Result: result}); err != nil {
			return
		}
	}
}

func (s *integrationCodexAppServer) notify(method string, params any) bool {
	return s.send(json.RawMessage(nil), method, params)
}

func (s *integrationCodexAppServer) request(id json.RawMessage, method string, params any) bool {
	return s.send(id, method, params)
}

func (s *integrationCodexAppServer) send(id json.RawMessage, method string, params any) bool {
	s.mu.Lock()
	connection := s.active
	s.mu.Unlock()
	if connection == nil {
		return false
	}
	return connection.write(struct {
		ID     json.RawMessage `json:"id,omitempty"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}{ID: id, Method: method, Params: integrationMustJSON(params)}) == nil
}

func (s *integrationCodexAppServer) disconnect() {
	s.mu.Lock()
	connection := s.active
	s.active = nil
	s.mu.Unlock()
	if connection != nil {
		connection.close()
	}
}

func (s *integrationCodexAppServer) setAvailable(available bool) {
	s.mu.Lock()
	s.available = available
	s.mu.Unlock()
}

func (s *integrationCodexAppServer) setSnapshot(t *testing.T, root integrationCodexThread, descendants []integrationCodexThread) {
	t.Helper()
	rootBody, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	descendantBodies := make([]json.RawMessage, len(descendants))
	for i, descendant := range descendants {
		descendantBodies[i], err = json.Marshal(descendant)
		if err != nil {
			t.Fatal(err)
		}
	}
	s.mu.Lock()
	s.root = rootBody
	s.descendants = descendantBodies
	s.mu.Unlock()
}

func (s *integrationCodexAppServer) requestRoots() (reads, lists []string, connections int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.readRoots...), append([]string(nil), s.listRoots...), s.connections
}

func cloneIntegrationRawMessages(values []json.RawMessage) []json.RawMessage {
	clone := make([]json.RawMessage, len(values))
	for i, value := range values {
		clone[i] = append(json.RawMessage(nil), value...)
	}
	return clone
}

func integrationMustJSON(value any) json.RawMessage {
	body, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return body
}

func waitForIntegrationCodexGraph(t *testing.T, store *state.Store, key provider.RootKey, accept func(*state.AgentGraph) bool) *state.AgentGraph {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var latest *state.AgentGraph
	for time.Now().Before(deadline) {
		if session, ok := sessionForKey(store.Snapshot(), key); ok {
			latest = session.AgentGraph
			if accept(latest) {
				return latest
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for Codex graph; latest = %#v", latest)
	return nil
}

func TestCodexAppServerObserverThroughCoordinatorStateAndHistory(t *testing.T) {
	const rootID = "root-exact"
	root := integrationCodexThread{
		ID: rootID, Status: integrationCodexStatus{Type: "idle"},
		Turns: []integrationCodexTurn{{
			ID: "root-turn",
			Items: []integrationCodexItem{{
				Type: "collabAgentToolCall", SenderThreadID: rootID,
				ReceiverThreadIDs: []string{"child-approval", "child-worker"},
				AgentsStates: map[string]integrationCodexAgentState{
					"child-approval": {Status: "running"},
					"child-worker":   {Status: "running"},
				},
			}},
		}},
	}
	descendants := []integrationCodexThread{
		{
			ID: "child-approval", ParentThreadID: rootID, AgentNickname: "Reviewer", AgentRole: "reviewer",
			Status: integrationCodexStatus{Type: "active"},
		},
		{
			ID: "child-worker", ParentThreadID: rootID, AgentNickname: "Builder", AgentRole: "worker",
			Status: integrationCodexStatus{Type: "active"},
		},
	}
	appServer := newIntegrationCodexAppServer(t, root, descendants)
	historyDir := t.TempDir()
	sink := history.NewSink(history.Config{Enabled: true, Detail: history.DetailFull, Dir: historyDir})
	observer := codexprovider.NewObserver(codexprovider.Config{
		Connector: appServer, Environment: integrationCodexEnvironment{},
		Freshness: 250 * time.Millisecond, ResnapshotInterval: time.Hour,
		RequestTimeout: time.Second, ReconnectMinimum: 5 * time.Millisecond,
		ReconnectMaximum: 10 * time.Millisecond, Jitter: func(time.Duration) time.Duration { return 0 },
	})
	store := state.New("")
	startedAt := time.Now().Add(-time.Hour)
	ref := seedCoordinatorSession(store, 4801, startedAt, state.AgentKindCodex, "", "/project")
	coordinator := newAgentCoordinator(store, sink, nil, observer)
	coordinator.Start(context.Background(), time.Hour)
	sinkClosed := false
	t.Cleanup(func() {
		coordinator.Close()
		if !sinkClosed {
			sink.Close()
		}
		appServer.disconnect()
	})

	// The process environment deliberately has no identity. SessionStart is the
	// sole exact-binding source and must drive read/list for this thread only.
	session, _ := sessionForKey(store.Snapshot(), ref.Key())
	coordinator.HandleHook(rpc.Request{
		Agent: state.AgentKindCodex, Event: "SessionStart", SessionID: rootID,
	}, session)
	delegating := waitForIntegrationCodexGraph(t, store, ref.Key(), func(graph *state.AgentGraph) bool {
		return graph != nil && graph.Source == agentgraph.SourceCodexAppServer && graph.Complete &&
			len(graph.Nodes) == 3 && graph.Summary.Status == state.StatusDelegating
	})
	if delegating.RootID != rootID || delegating.Summary.LiveChildren != 2 || delegating.Summary.WaitingNodes != 0 {
		t.Fatalf("initial app-server projection = %#v", delegating)
	}

	// Reproduce the false-red escalation: the mechanical wait and server request
	// arrive before auto-review evidence. Publication is held until ownership is
	// known, then stays green through every automated review edge.
	if !appServer.notify("thread/status/changed", map[string]any{
		"threadId": "child-approval",
		"status":   map[string]any{"type": "active", "activeFlags": []string{"waitingOnApproval"}},
	}) {
		t.Fatal("fake app-server had no live JSONL connection")
	}
	if !appServer.request(json.RawMessage(`"auto-request-1"`), "item/commandExecution/requestApproval", map[string]any{
		"threadId": "child-approval", "turnId": "child-turn", "itemId": "auto-item-1",
	}) || !appServer.notify("item/autoApprovalReview/started", map[string]any{
		"threadId": "child-approval", "turnId": "child-turn", "reviewId": "review-1", "targetItemId": "auto-item-1",
		"review": map[string]any{"status": "inProgress"},
	}) {
		t.Fatal("fake app-server could not emit wait-first auto review")
	}
	automatic := waitForIntegrationCodexGraph(t, store, ref.Key(), func(graph *state.AgentGraph) bool {
		return graph != nil && graph.Source == agentgraph.SourceCodexAppServer &&
			graph.Summary.Status == state.StatusDelegating && graph.Summary.WaitingNodes == 0
	})
	if automatic.Summary.LiveChildren != 2 {
		t.Fatalf("automatic-review graph live children = %d, want 2", automatic.Summary.LiveChildren)
	}
	if !appServer.notify("item/autoApprovalReview/completed", map[string]any{
		"threadId": "child-approval", "turnId": "child-turn", "reviewId": "review-1", "targetItemId": "auto-item-1",
		"review": map[string]any{"status": "denied"},
	}) || !appServer.notify("serverRequest/resolved", map[string]any{
		"threadId": "child-approval", "requestId": "auto-request-1",
	}) || !appServer.notify("thread/status/changed", map[string]any{
		"threadId": "child-approval", "status": map[string]any{"type": "active", "activeFlags": []string{}},
	}) {
		t.Fatal("fake app-server could not complete wait-first auto review")
	}

	// Exercise the inverse event order as well.
	if !appServer.notify("item/autoApprovalReview/started", map[string]any{
		"threadId": "child-approval", "turnId": "child-turn", "reviewId": "review-2", "targetItemId": "auto-item-2",
		"review": map[string]any{"status": "inProgress"},
	}) || !appServer.request(json.RawMessage(`"auto-request-2"`), "item/commandExecution/requestApproval", map[string]any{
		"threadId": "child-approval", "turnId": "child-turn", "itemId": "auto-item-2",
	}) || !appServer.notify("thread/status/changed", map[string]any{
		"threadId": "child-approval", "status": map[string]any{"type": "active", "activeFlags": []string{"waitingOnApproval"}},
	}) || !appServer.notify("item/autoApprovalReview/completed", map[string]any{
		"threadId": "child-approval", "turnId": "child-turn", "reviewId": "review-2", "targetItemId": "auto-item-2",
		"review": map[string]any{"status": "allowed"},
	}) || !appServer.notify("serverRequest/resolved", map[string]any{
		"threadId": "child-approval", "requestId": "auto-request-2",
	}) || !appServer.notify("thread/status/changed", map[string]any{
		"threadId": "child-approval", "status": map[string]any{"type": "active", "activeFlags": []string{}},
	}) {
		t.Fatal("fake app-server could not emit review-first auto review")
	}
	waitForIntegrationCodexGraph(t, store, ref.Key(), func(graph *state.AgentGraph) bool {
		return graph != nil && graph.Summary.Status == state.StatusDelegating && graph.Summary.WaitingNodes == 0
	})

	// A positively identified user-routed request is the only red edge.
	if !appServer.notify("thread/settings/updated", map[string]any{
		"threadId": "child-approval", "threadSettings": map[string]any{"approvalsReviewer": "user"},
	}) || !appServer.notify("thread/status/changed", map[string]any{
		"threadId": "child-approval", "status": map[string]any{"type": "active", "activeFlags": []string{"waitingOnApproval"}},
	}) || !appServer.request(json.RawMessage(`"human-request"`), "item/commandExecution/requestApproval", map[string]any{
		"threadId": "child-approval", "turnId": "child-turn", "itemId": "human-item",
	}) {
		t.Fatal("fake app-server could not emit human approval")
	}
	permission := waitForIntegrationCodexGraph(t, store, ref.Key(), func(graph *state.AgentGraph) bool {
		return graph != nil && graph.Summary.Status == state.StatusPermission && graph.Summary.ApprovalNodes == 1
	})
	if permission.Summary.Attention != agentgraph.AttentionApproval {
		t.Fatalf("human approval projection = %#v", permission)
	}
	if !appServer.notify("serverRequest/resolved", map[string]any{
		"threadId": "child-approval", "requestId": "human-request",
	}) || !appServer.notify("thread/status/changed", map[string]any{
		"threadId": "child-approval", "status": map[string]any{"type": "active", "activeFlags": []string{}},
	}) {
		t.Fatal("fake app-server could not resolve human approval")
	}
	waitForIntegrationCodexGraph(t, store, ref.Key(), func(graph *state.AgentGraph) bool {
		return graph != nil && graph.Summary.Status == state.StatusDelegating && graph.Summary.WaitingNodes == 0
	})

	// Disconnect preserves the last snapshot only through its exact freshness
	// boundary. The coordinator must then project unknown instead of freezing a
	// falsely authoritative delegating state.
	appServer.setAvailable(false)
	appServer.disconnect()
	expired := waitForIntegrationCodexGraph(t, store, ref.Key(), func(graph *state.AgentGraph) bool {
		return graph != nil && graph.Source == agentgraph.SourceCodexAppServer &&
			!graph.Fresh(time.Now()) && graph.Summary.Status == ""
	})
	if len(expired.Nodes) != 3 {
		t.Fatalf("expiry discarded bounded graph detail: %#v", expired.Nodes)
	}

	// Reconnect returns an authoritative root-only snapshot. This also proves the
	// hook binding survived transport loss and complete omission closes the two
	// descendant history lanes as not_found.
	appServer.setSnapshot(t, integrationCodexThread{ID: rootID, Status: integrationCodexStatus{Type: "idle"}}, nil)
	appServer.setAvailable(true)
	reconnected := waitForIntegrationCodexGraph(t, store, ref.Key(), func(graph *state.AgentGraph) bool {
		return graph != nil && graph.Source == agentgraph.SourceCodexAppServer && graph.Complete &&
			graph.Fresh(time.Now()) && len(graph.Nodes) == 1 && graph.Summary.Status == state.StatusIdle
	})
	if reconnected.RootID != rootID {
		t.Fatalf("reconnected root = %q, want exact binding %q", reconnected.RootID, rootID)
	}
	reads, lists, connections := appServer.requestRoots()
	if connections < 2 || len(reads) < 2 || len(lists) < 2 {
		t.Fatalf("app-server resnapshot counts: connections=%d reads=%v lists=%v", connections, reads, lists)
	}
	for _, got := range append(reads, lists...) {
		if got != rootID {
			t.Fatalf("app-server request used non-exact root %q; all reads=%v lists=%v", got, reads, lists)
		}
	}

	coordinator.Close()
	sink.Close()
	sinkClosed = true
	events, err := history.ReadRange(historyDir, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var approvalStarts, approvalClears int
	var sawNotFound bool
	for _, event := range events {
		if event.Type != history.EventAgentState || event.ThreadID != "child-approval" {
			continue
		}
		if event.ToAttention == agentgraph.AttentionApproval {
			approvalStarts++
		}
		if event.FromAttention == agentgraph.AttentionApproval && event.ToAttention == agentgraph.AttentionNone {
			approvalClears++
		}
		sawNotFound = sawNotFound || event.ToLifecycle == agentgraph.LifecycleNotFound
	}
	if approvalStarts != 1 || approvalClears != 1 || !sawNotFound {
		t.Fatalf("canonical history approval intervals=%d/%d not_found=%t; automated reviews must create none: events=%+v",
			approvalStarts, approvalClears, sawNotFound, events)
	}
}
