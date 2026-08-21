package codex

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/provider"
)

func TestObserverReconnectFreshnessGenerationAndCoalescing(t *testing.T) {
	proxy := newFakeProxy()
	started := time.Unix(100, 0)
	key := provider.RootKey{PID: 42, StartedAt: started}
	environment := &fakeEnvironment{values: map[provider.RootKey][]byte{key: []byte("CODEX_THREAD_ID=root\x00")}}
	observer := NewObserver(Config{
		Connector: proxy, Environment: environment,
		Freshness: 500 * time.Millisecond, ResnapshotInterval: time.Hour,
		RequestTimeout: time.Second, ReconnectMinimum: 5 * time.Millisecond,
		ReconnectMaximum: 10 * time.Millisecond, Jitter: func(time.Duration) time.Duration { return 0 },
	})
	defer observer.Close()
	ref := provider.RootRef{PID: key.PID, StartedAt: key.StartedAt, Provider: agentgraph.ProviderCodex, CWD: "/same"}

	initial, err := observer.Observe(context.Background(), ref, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if initial.Complete || initial.RootID != "root" {
		t.Fatalf("initial observation = %#v", initial)
	}
	waitUpdate(t, observer.Updates(), time.Second)
	complete := waitCompleteObservation(t, observer, ref, time.Second)
	assertNode(t, complete, "child", "root", "First", "worker", agentgraph.RuntimeActive, agentgraph.AttentionNone, agentgraph.LifecycleRunning)

	methods := proxy.Methods()
	assertMethodOrder(t, methods, []string{"initialize", "initialized", "thread/read", "thread/list"})
	for _, method := range methods {
		if _, request := allowedRequests[method]; request {
			continue
		}
		if _, notification := allowedNotifications[method]; notification {
			continue
		}
		t.Fatalf("observer emitted non-allowlisted method %q", method)
	}
	listRequests := proxy.ListRequests()
	if len(listRequests) == 0 {
		t.Fatal("observer sent no descendant thread/list request")
	}
	for i, request := range listRequests {
		if request.AncestorThreadID != "root" {
			t.Errorf("thread/list request %d ancestorThreadId = %q", i, request.AncestorThreadID)
		}
		if !reflect.DeepEqual(request.SourceKinds, descendantSourceKinds) {
			t.Errorf("thread/list request %d sourceKinds = %v, want %v", i, request.SourceKinds, descendantSourceKinds)
		}
	}
	drainUpdates(observer.Updates())

	// Returned observations are detached from the cache.
	complete.Nodes[1].Nickname = "caller mutation"
	again, _ := observer.Observe(context.Background(), ref, time.Now())
	if node := findNode(again, "child"); node == nil || node.Nickname != "First" {
		t.Fatalf("caller mutated cached observation: %#v", node)
	}

	proxy.Notify(rpcEnvelope{Method: "thread/status/changed", Params: mustJSON(t, map[string]any{
		"threadId": "child", "status": map[string]any{"type": "active", "activeFlags": []string{"waitingOnApproval"}},
	})})
	waitUpdate(t, observer.Updates(), time.Second)
	waiting, _ := observer.Observe(context.Background(), ref, time.Now())
	assertNodeAttention(t, waiting, "child", agentgraph.AttentionApproval)

	// Unrelated roots never invalidate this root.
	drainUpdates(observer.Updates())
	observer.mu.Lock()
	unrelatedKeys, _ := observer.applyNotificationLocked(rpcNotification{Generation: observer.generation, Method: "thread/status/changed", Params: mustJSON(t, map[string]any{
		"threadId": "unrelated", "status": map[string]any{"type": "active"},
	})})
	observer.mu.Unlock()
	if len(unrelatedKeys) != 0 {
		t.Fatalf("unrelated root touched keys %#v", unrelatedKeys)
	}

	// A slow consumer gets one coalesced signal rather than blocking the reader.
	observer.mu.Lock()
	stormGeneration := observer.generation
	observer.mu.Unlock()
	for i := 0; i < 500; i++ {
		observer.handleNotification(rpcNotification{Generation: stormGeneration, Method: "thread/status/changed", Params: mustJSON(t, map[string]any{
			"threadId": "child", "status": map[string]any{"type": "active", "activeFlags": []string{}},
		})})
	}
	waitUpdate(t, observer.Updates(), time.Second)
	select {
	case extra := <-observer.Updates():
		t.Fatalf("notification storm was not coalesced: %#v", extra)
	case <-time.After(20 * time.Millisecond):
	}

	observer.mu.Lock()
	oldGeneration := observer.generation
	deadline := observer.roots[key].observation.FreshUntil
	observer.mu.Unlock()
	proxy.SetAvailable(false)
	proxy.Disconnect()
	waitUpdate(t, observer.Updates(), time.Second) // immediate disconnect invalidation
	retained, _ := observer.Observe(context.Background(), ref, time.Now())
	if !retained.Fresh(time.Now()) || !retained.FreshUntil.Equal(deadline) {
		t.Fatalf("disconnect did not retain exact last freshness window: %#v", retained)
	}
	waitUpdate(t, observer.Updates(), time.Until(deadline)+time.Second) // expiry invalidation
	expired, _ := observer.Observe(context.Background(), ref, time.Now())
	if summary := agentgraph.Reduce(expired, agentgraph.Summary{}, time.Now()); summary.LegacyStatus != "" || summary.Runtime != agentgraph.RuntimeUnknown {
		t.Fatalf("expired disconnected observation reduced authoritatively: %#v", summary)
	}

	proxy.SetSnapshot(
		rpcThread{ID: "root", Status: rpcStatus{Type: "idle"}},
		[]rpcThread{{ID: "child", ParentThreadID: "root", AgentNickname: "After reconnect", AgentRole: "worker", Status: rpcStatus{Type: "idle"}}},
	)
	proxy.SetAvailable(true)
	waitUpdate(t, observer.Updates(), time.Second)
	reconnected := waitObservation(t, observer, ref, time.Second, func(observation agentgraph.Observation) bool {
		node := findNode(observation, "child")
		return observation.Complete && observation.Fresh(time.Now()) && node != nil && node.Nickname == "After reconnect"
	})
	if node := findNode(reconnected, "child"); node == nil || node.Nickname != "After reconnect" || node.Runtime != agentgraph.RuntimeIdle {
		t.Fatalf("reconnect did not install authoritative resnapshot: %#v", node)
	}

	// A delayed notification from the previous connection generation is fenced.
	observer.handleNotification(rpcNotification{Generation: oldGeneration, Method: "thread/status/changed", Params: mustJSON(t, map[string]any{
		"threadId": "child", "status": map[string]any{"type": "active", "activeFlags": []string{"waitingOnUserInput"}},
	})})
	afterStale, _ := observer.Observe(context.Background(), ref, time.Now())
	if node := findNode(afterStale, "child"); node == nil || node.Runtime != agentgraph.RuntimeIdle || node.Attention != agentgraph.AttentionNone {
		t.Fatalf("stale generation mutated resnapshot: %#v", node)
	}

	observer.Forget(key)
	observer.Forget(key)
	observer.mu.Lock()
	_, retainedKey := observer.roots[key]
	observer.mu.Unlock()
	if retainedKey {
		t.Fatal("Forget retained root state")
	}
	if err := observer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := observer.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestObserverHookBindingAndRootIDVerification(t *testing.T) {
	proxy := newFakeProxy()
	proxy.readOverride = rpcThread{ID: "different", Status: rpcStatus{Type: "idle"}}
	environment := &fakeEnvironment{errors: map[provider.RootKey]error{}}
	observer := NewObserver(Config{
		Connector: proxy, Environment: environment, ResnapshotInterval: time.Hour,
		RequestTimeout: 30 * time.Millisecond, ReconnectMinimum: 5 * time.Millisecond,
		ReconnectMaximum: 10 * time.Millisecond, Jitter: func(time.Duration) time.Duration { return 0 },
	})
	defer observer.Close()
	key := provider.RootKey{PID: 7, StartedAt: time.Unix(7, 0)}
	if err := observer.RegisterHookBinding(key, "root"); err != nil {
		t.Fatal(err)
	}
	ref := provider.RootRef{PID: key.PID, StartedAt: key.StartedAt, Provider: agentgraph.ProviderCodex}
	observation, err := observer.Observe(context.Background(), ref, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if observation.RootID != "root" || observation.Complete {
		t.Fatalf("hook pending observation = %#v", observation)
	}
	time.Sleep(50 * time.Millisecond)
	observation, _ = observer.Observe(context.Background(), ref, time.Now())
	if observation.Complete {
		t.Fatal("mismatched thread/read response became authoritative")
	}
}

type fakeProxy struct {
	mu           sync.Mutex
	available    bool
	root         rpcThread
	descendants  []rpcThread
	readOverride rpcThread
	server       net.Conn
	encoder      *json.Encoder
	methods      []string
	listRequests []fakeListRequest
}

type fakeListRequest struct {
	AncestorThreadID string   `json:"ancestorThreadId"`
	SourceKinds      []string `json:"sourceKinds"`
}

func newFakeProxy() *fakeProxy {
	return &fakeProxy{
		available: true,
		root: rpcThread{ID: "root", Status: rpcStatus{Type: "idle"}, Turns: []rpcTurn{{
			ID: "root-turn", Items: []rpcItem{{
				Type: "collabAgentToolCall", SenderThreadID: "root", ReceiverThreadIDs: []string{"child"},
				AgentsStates: map[string]rpcAgentState{"child": {Status: "running"}},
			}},
		}}},
		descendants: []rpcThread{{
			ID: "child", ParentThreadID: "root", AgentNickname: "First", AgentRole: "worker", Status: rpcStatus{Type: "active"},
			Turns: []rpcTurn{{ID: "child-turn"}},
		}},
	}
}

func (p *fakeProxy) Connect(context.Context) (Connection, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.available {
		return nil, errors.New("fake proxy unavailable")
	}
	client, server := net.Pipe()
	p.server = server
	p.encoder = json.NewEncoder(server)
	go p.serve(server)
	return client, nil
}

func (p *fakeProxy) serve(connection net.Conn) {
	decoder := json.NewDecoder(connection)
	for {
		var envelope rpcEnvelope
		if err := decoder.Decode(&envelope); err != nil {
			return
		}
		p.mu.Lock()
		p.methods = append(p.methods, envelope.Method)
		root := p.root
		if p.readOverride.ID != "" {
			root = p.readOverride
		}
		descendants := append([]rpcThread(nil), p.descendants...)
		encoder := p.encoder
		var result any
		switch envelope.Method {
		case "initialize":
			result = initializeResult{UserAgent: "codex_app_server/0.149.0"}
		case "thread/read":
			result = threadReadResult{Thread: root}
		case "thread/list":
			var request fakeListRequest
			_ = json.Unmarshal(envelope.Params, &request)
			request.SourceKinds = append([]string(nil), request.SourceKinds...)
			p.listRequests = append(p.listRequests, request)
			result = threadListResult{Data: descendants}
		case "initialized":
			p.mu.Unlock()
			continue
		default:
			p.mu.Unlock()
			continue
		}
		if len(envelope.ID) != 0 {
			_ = encoder.Encode(struct {
				ID     json.RawMessage `json:"id"`
				Result any             `json:"result"`
			}{ID: envelope.ID, Result: result})
		}
		p.mu.Unlock()
	}
}

func (p *fakeProxy) Notify(envelope rpcEnvelope) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.encoder != nil {
		_ = p.encoder.Encode(envelope)
	}
}

func (p *fakeProxy) Disconnect() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.server != nil {
		_ = p.server.Close()
		p.server = nil
		p.encoder = nil
	}
}

func (p *fakeProxy) SetAvailable(available bool) {
	p.mu.Lock()
	p.available = available
	p.mu.Unlock()
}

func (p *fakeProxy) SetSnapshot(root rpcThread, descendants []rpcThread) {
	p.mu.Lock()
	p.root = root
	p.descendants = append([]rpcThread(nil), descendants...)
	p.readOverride = rpcThread{}
	p.mu.Unlock()
}

func (p *fakeProxy) Methods() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.methods...)
}

func (p *fakeProxy) ListRequests() []fakeListRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]fakeListRequest, len(p.listRequests))
	for i, request := range p.listRequests {
		out[i] = request
		out[i].SourceKinds = append([]string(nil), request.SourceKinds...)
	}
	return out
}

func waitUpdate(t *testing.T, updates <-chan provider.RootKey, timeout time.Duration) provider.RootKey {
	t.Helper()
	if timeout <= 0 {
		timeout = time.Second
	}
	select {
	case key := <-updates:
		return key
	case <-time.After(timeout):
		t.Fatal("timed out waiting for observer invalidation")
		return provider.RootKey{}
	}
}

func drainUpdates(updates <-chan provider.RootKey) {
	for {
		select {
		case <-updates:
		default:
			return
		}
	}
}

func waitCompleteObservation(t *testing.T, observer *Observer, ref provider.RootRef, timeout time.Duration) agentgraph.Observation {
	t.Helper()
	return waitObservation(t, observer, ref, timeout, func(observation agentgraph.Observation) bool { return observation.Complete })
}

func waitObservation(t *testing.T, observer *Observer, ref provider.RootRef, timeout time.Duration, accept func(agentgraph.Observation) bool) agentgraph.Observation {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		observation, err := observer.Observe(context.Background(), ref, time.Now())
		if err == nil && accept(observation) {
			return observation
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for complete observation")
	return agentgraph.Observation{}
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func assertMethodOrder(t *testing.T, got, ordered []string) {
	t.Helper()
	position := 0
	for _, method := range got {
		if position < len(ordered) && method == ordered[position] {
			position++
		}
	}
	if position != len(ordered) {
		t.Fatalf("method order %v does not contain %v", got, ordered)
	}
}
