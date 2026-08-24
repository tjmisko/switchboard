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
	Name           string                 `json:"name,omitempty"`
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
	methods     []string
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
		s.mu.Lock()
		s.methods = append(s.methods, request.Method)
		s.mu.Unlock()
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

func (s *integrationCodexAppServer) requestMethods() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.methods...)
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

func TestStandardCodexHooksGenerateDisplayNameAndNativeRenameOverridesIt(t *testing.T) {
	const rootID = "root-standard"
	root := integrationCodexThread{
		ID: rootID, Name: "session-naming", Status: integrationCodexStatus{Type: "idle"},
	}
	appServer := newIntegrationCodexAppServer(t, root, nil)
	observer := codexprovider.NewObserver(codexprovider.Config{
		Connector: appServer,
		Freshness: 250 * time.Millisecond, ResnapshotInterval: time.Hour,
		RequestTimeout: time.Second, ReconnectMinimum: 5 * time.Millisecond,
		ReconnectMaximum: 10 * time.Millisecond, Jitter: func(time.Duration) time.Duration { return 0 },
	})
	store := state.New("")
	ref := seedCoordinatorSession(store, 4800, time.Now().Add(-time.Hour), state.AgentKindCodex, "", "/project")
	coordinator := newAgentCoordinator(store, nil, nil, observer)
	namer := &scriptedDisplayNamer{results: []scriptedDisplayNameResult{{name: "context-aware-display"}}}
	coordinator.SetCodexDisplayNamer(namer, "test-model")
	coordinator.Start(context.Background(), time.Hour)
	t.Cleanup(func() {
		coordinator.Close()
		appServer.disconnect()
	})

	// A standard hook is the only identity source. The observer may read that
	// exact thread but has no discovery or write path of its own.
	session, _ := sessionForKey(store.Snapshot(), ref.Key())
	coordinator.HandleHook(rpc.Request{
		Agent: state.AgentKindCodex, Event: "SessionStart", SessionID: rootID,
		ObservedAt: time.Now(),
	}, session)
	initial := waitForIntegrationCodexGraph(t, store, ref.Key(), func(graph *state.AgentGraph) bool {
		return graph != nil && graph.RootID == rootID && graph.Source == agentgraph.SourceCodexAppServer &&
			graph.Complete && len(graph.Nodes) == 1 && graph.Nodes[0].Nickname == "session-naming"
	})
	if initial.Nodes[0].ID != rootID {
		t.Fatalf("observer bound a non-root thread: %#v", initial.Nodes)
	}

	base := time.Now()
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "UserPromptSubmit", SessionID: rootID, TurnID: "turn-1",
		Prompt: "Implement context-aware names", ObservedAt: base,
	})
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "Stop", SessionID: rootID, TurnID: "turn-1",
		LastAssistantMessage: "Implemented and verified", ObservedAt: base.Add(time.Millisecond),
	})
	waitForDisplayName(t, store, "context-aware-display")

	// The Stop hook is partial, so wait for the next authoritative read to pin
	// the native baseline. The generated label remains the visible winner.
	baselineDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(baselineDeadline) {
		session, ok := sessionForKey(store.Snapshot(), ref.Key())
		if ok && session.DisplayName != nil && session.DisplayName.NativeBaseline != nil &&
			*session.DisplayName.NativeBaseline == "session-naming" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	session, _ = sessionForKey(store.Snapshot(), ref.Key())
	if session.DisplayName == nil || session.DisplayName.NativeBaseline == nil || *session.DisplayName.NativeBaseline != "session-naming" {
		t.Fatalf("authoritative baseline was not captured: %+v", session.DisplayName)
	}

	root.Name = "manual-native-name"
	appServer.setSnapshot(t, root, nil)
	if !appServer.notify("thread/name/updated", map[string]any{
		"threadId": rootID, "threadName": "manual-native-name",
	}) {
		t.Fatal("fake app-server had no live observer connection")
	}
	waitForNoDisplayName(t, store)
	waitForIntegrationCodexGraph(t, store, ref.Key(), func(graph *state.AgentGraph) bool {
		return graph != nil && len(graph.Nodes) > 0 && graph.Nodes[0].Nickname == "manual-native-name"
	})

	reads, lists, _ := appServer.requestRoots()
	if len(reads) == 0 || len(lists) == 0 {
		t.Fatalf("observer did not use read/list: reads=%v lists=%v", reads, lists)
	}
	for _, threadID := range append(reads, lists...) {
		if threadID != rootID {
			t.Fatalf("observer escaped exact hook binding: %q", threadID)
		}
	}
	for _, method := range appServer.requestMethods() {
		switch method {
		case "initialize", "initialized", "thread/read", "thread/turns/list", "thread/list":
		default:
			t.Fatalf("observer attempted a non-read request; methods=%v", appServer.requestMethods())
		}
	}
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
		Connector: appServer,
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

func TestCodexChildHooksThroughAppServerCoordinatorAndHistory(t *testing.T) {
	const rootID = "root-child-hooks"
	root := integrationCodexThread{ID: rootID, Status: integrationCodexStatus{Type: "idle"}}
	descendants := []integrationCodexThread{
		{
			ID: "structural-parent", ParentThreadID: rootID, AgentNickname: "Parent", AgentRole: "worker",
			Status: integrationCodexStatus{Type: "notLoaded"},
		},
		{
			ID: "nested-child", ParentThreadID: "structural-parent", AgentNickname: "Nested", AgentRole: "worker",
			Status: integrationCodexStatus{Type: "notLoaded"},
		},
	}
	appServer := newIntegrationCodexAppServer(t, root, descendants)
	historyDir := t.TempDir()
	sink := history.NewSink(history.Config{Enabled: true, Detail: history.DetailFull, Dir: historyDir})
	observer := codexprovider.NewObserver(codexprovider.Config{
		Connector: appServer,
		Freshness: 2 * time.Second, ResnapshotInterval: time.Hour,
		RequestTimeout: time.Second, ReconnectMinimum: 5 * time.Millisecond,
		ReconnectMaximum: 10 * time.Millisecond, Jitter: func(time.Duration) time.Duration { return 0 },
	})
	store := state.New("")
	ref := seedCoordinatorSession(store, 4802, time.Now().Add(-time.Hour), state.AgentKindCodex, rootID, "/project")
	if err := observer.RegisterHookBinding(ref.Key(), rootID); err != nil {
		t.Fatal(err)
	}
	coordinator := newAgentCoordinator(store, sink, nil, observer)
	coordinator.Start(context.Background(), time.Hour)
	closed := false
	t.Cleanup(func() {
		coordinator.Close()
		if !closed {
			sink.Close()
		}
		appServer.disconnect()
	})

	initial := waitForIntegrationCodexGraph(t, store, ref.Key(), func(graph *state.AgentGraph) bool {
		return graph != nil && graph.Source == agentgraph.SourceCodexAppServer && graph.Complete && len(graph.Nodes) == 3
	})
	if initial.Summary.LiveChildren != 0 {
		t.Fatalf("topology-only live children = %d, want 0: %#v", initial.Summary.LiveChildren, initial)
	}

	send := func(event string, at time.Time) {
		session, ok := sessionForKey(store.Snapshot(), ref.Key())
		if !ok {
			t.Fatal("Codex root disappeared")
		}
		coordinator.HandleHook(rpc.Request{
			Agent: state.AgentKindCodex, Event: event, SessionID: rootID,
			AgentID: "nested-child", ObservedAt: at,
		}, session)
	}
	startAt := time.Now().UTC()
	send("SubagentStart", startAt)
	started := waitForIntegrationCodexGraph(t, store, ref.Key(), func(graph *state.AgentGraph) bool {
		if graph == nil || graph.Summary.LiveChildren != 1 {
			return false
		}
		node := childNode(t, graph, "nested-child")
		return node.Runtime == agentgraph.RuntimeActive && node.Lifecycle == agentgraph.LifecycleRunning
	})
	if node := childNode(t, started, "nested-child"); node.ParentID != "structural-parent" {
		t.Fatalf("nested immediate parent = %q, want structural-parent", node.ParentID)
	}

	stopAt := startAt.Add(time.Second)
	send("SubagentStop", stopAt)
	waitForIntegrationCodexGraph(t, store, ref.Key(), func(graph *state.AgentGraph) bool {
		if graph == nil || graph.Summary.LiveChildren != 0 {
			return false
		}
		node := childNode(t, graph, "nested-child")
		return node.Runtime == agentgraph.RuntimeIdle && node.Lifecycle == agentgraph.LifecycleCompleted && node.CompletedAt.Equal(stopAt)
	})
	// Exact replay must not create another canonical history edge.
	send("SubagentStop", stopAt)

	restartAt := stopAt.Add(time.Second)
	send("SubagentStart", restartAt)
	waitForIntegrationCodexGraph(t, store, ref.Key(), func(graph *state.AgentGraph) bool {
		return graph != nil && graph.Summary.LiveChildren == 1 &&
			childNode(t, graph, "nested-child").Lifecycle == agentgraph.LifecycleRunning
	})
	finalStopAt := restartAt.Add(time.Second)
	send("SubagentStop", finalStopAt)
	waitForIntegrationCodexGraph(t, store, ref.Key(), func(graph *state.AgentGraph) bool {
		return graph != nil && graph.Summary.LiveChildren == 0 &&
			childNode(t, graph, "nested-child").Lifecycle == agentgraph.LifecycleCompleted
	})

	coordinator.Close()
	sink.Close()
	closed = true
	events, err := history.ReadRange(historyDir, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var childEvents []history.Event
	for _, event := range events {
		if event.Type == history.EventAgentState && event.ThreadID == "nested-child" &&
			event.FromLifecycle != event.ToLifecycle {
			childEvents = append(childEvents, event)
		}
	}
	wantTimes := []time.Time{startAt, stopAt, restartAt, finalStopAt}
	if len(childEvents) != len(wantTimes) {
		t.Fatalf("canonical child events = %d, want %d: %+v", len(childEvents), len(wantTimes), childEvents)
	}
	for i, event := range childEvents {
		if !event.Ts.Equal(wantTimes[i]) || event.ParentThreadID != "structural-parent" ||
			event.Source != agentgraph.SourceCodexAppServer {
			t.Fatalf("child event[%d] = %+v", i, event)
		}
	}
}
