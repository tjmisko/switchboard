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
	waiting := waitUpdateObservation(t, observer, ref, time.Second, func(observation agentgraph.Observation) bool {
		node := findNode(observation, "child")
		return node != nil && node.Attention == agentgraph.AttentionApproval
	})
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
	// The direct storm has completed synchronously, so its coalesced signal must
	// expose the final cleared state.
	cleared, _ := observer.Observe(context.Background(), ref, time.Now())
	assertNodeAttention(t, cleared, "child", agentgraph.AttentionNone)
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

func TestNotificationSignalExposesMutation(t *testing.T) {
	observer, key := fixtureObserver(t)
	drainUpdates(observer.Updates())
	params := mustJSON(t, map[string]any{
		"threadId": fixtureRoot,
		"status":   map[string]any{"type": "active", "activeFlags": []string{"waitingOnApproval"}},
	})
	done := make(chan struct{})
	go func() {
		observer.handleNotification(rpcNotification{
			Generation: 1,
			Method:     "thread/status/changed",
			Params:     params,
		})
		close(done)
	}()
	waitUpdate(t, observer.Updates(), time.Second)
	observer.mu.Lock()
	observation := observer.roots[key].observation.Clone()
	observer.mu.Unlock()
	assertNodeAttention(t, observation, fixtureRoot, agentgraph.AttentionApproval)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("notification handler did not return after signaling")
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

func TestObserverDisconnectDuringDescendantListKeepsLastCompleteSnapshot(t *testing.T) {
	proxy := newFakeProxy()
	key := provider.RootKey{PID: 88, StartedAt: time.Unix(88, 0)}
	environment := &fakeEnvironment{values: map[provider.RootKey][]byte{key: []byte("CODEX_THREAD_ID=root\x00")}}
	observer := NewObserver(Config{
		Connector: proxy, Environment: environment,
		Freshness: 2 * time.Second, ResnapshotInterval: time.Hour,
		RequestTimeout: time.Second, ReconnectMinimum: 5 * time.Millisecond,
		ReconnectMaximum: 10 * time.Millisecond, Jitter: func(time.Duration) time.Duration { return 0 },
	})
	defer observer.Close()
	ref := provider.RootRef{PID: key.PID, StartedAt: key.StartedAt, Provider: agentgraph.ProviderCodex}

	if _, err := observer.Observe(context.Background(), ref, time.Now()); err != nil {
		t.Fatal(err)
	}
	waitCompleteObservation(t, observer, ref, time.Second)
	drainUpdates(observer.Updates())

	// The root read succeeds, but the proxy drops while the descendant list is
	// in flight. Reconnect is disabled so only the retained snapshot can win.
	proxy.SetAvailable(false)
	listEntered, releaseList, disconnectedList := proxy.DisconnectOnNextList()
	// Registered after observer.Close's defer, so a failed assertion releases
	// the blocked fake request before Close waits for the supervisor.
	defer releaseList()
	observer.signalRefresh()
	select {
	case <-listEntered:
	case <-time.After(time.Second):
		t.Fatal("fake proxy never entered thread/list")
	}
	// Capture the authoritative boundary only after the selected list request
	// is paused. This includes any earlier redundant refresh that won the race.
	prior, err := observer.Observe(context.Background(), ref, time.Now())
	if err != nil || !prior.Complete {
		t.Fatalf("observation before descendant-list disconnect = %#v, %v", prior, err)
	}
	releaseList()
	select {
	case <-disconnectedList:
	case <-time.After(time.Second):
		t.Fatal("fake proxy never disconnected during thread/list")
	}
	waitCondition(t, time.Second, func() bool {
		observer.mu.Lock()
		defer observer.mu.Unlock()
		return !observer.connected
	})
	waitUpdate(t, observer.Updates(), time.Second)

	retained, err := observer.Observe(context.Background(), ref, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !retained.Complete || !retained.ObservedAt.Equal(prior.ObservedAt) || !retained.FreshUntil.Equal(prior.FreshUntil) {
		t.Fatalf("partial descendant snapshot replaced last complete observation: prior=%#v retained=%#v", prior, retained)
	}
	if node := findNode(retained, "child"); node == nil || node.Nickname != "First" || node.Lifecycle != agentgraph.LifecycleRunning {
		t.Fatalf("retained descendant changed after list disconnect: %#v", node)
	}
}

func TestObserverCloseInterruptsReconnectBackoff(t *testing.T) {
	connector := &rejectingConnector{called: make(chan struct{})}
	backoffStarted := make(chan struct{})
	var backoffOnce sync.Once
	observer := NewObserver(Config{
		Connector:        connector,
		ReconnectMinimum: time.Hour, ReconnectMaximum: time.Hour,
		Jitter: func(time.Duration) time.Duration {
			backoffOnce.Do(func() { close(backoffStarted) })
			return 0
		},
	})
	select {
	case <-connector.called:
	case <-time.After(time.Second):
		t.Fatal("observer never attempted connection")
	}
	select {
	case <-backoffStarted:
	case <-time.After(time.Second):
		t.Fatal("observer never entered reconnect backoff")
	}

	started := time.Now()
	if err := observer.Close(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("Close took %v while reconnect backoff was active", elapsed)
	}
	if err := observer.Close(); err != nil {
		t.Fatal(err)
	}
	if connector.calls != 1 {
		t.Fatalf("connector calls after Close = %d, want 1", connector.calls)
	}
}

type fakeProxy struct {
	mu                 sync.Mutex
	available          bool
	root               rpcThread
	descendants        []rpcThread
	readOverride       rpcThread
	server             net.Conn
	encoder            *json.Encoder
	methods            []string
	listRequests       []fakeListRequest
	disconnectNextList bool
	listEntered        chan struct{}
	listRelease        chan struct{}
	listDisconnected   chan struct{}
	listEnterOnce      sync.Once
	listReleaseOnce    sync.Once
	listDisconnectOnce sync.Once
}

type fakeListRequest struct {
	AncestorThreadID string   `json:"ancestorThreadId"`
	SourceKinds      []string `json:"sourceKinds"`
}

func newFakeProxy() *fakeProxy {
	return &fakeProxy{
		available:   true,
		listEntered: make(chan struct{}), listRelease: make(chan struct{}), listDisconnected: make(chan struct{}),
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
			if p.disconnectNextList {
				p.disconnectNextList = false
				p.mu.Unlock()
				p.listEnterOnce.Do(func() { close(p.listEntered) })
				<-p.listRelease
				p.mu.Lock()
				p.server = nil
				p.encoder = nil
				p.mu.Unlock()
				_ = connection.Close()
				p.listDisconnectOnce.Do(func() { close(p.listDisconnected) })
				return
			}
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
	// Keep the fake's authoritative snapshot coherent with emitted status
	// notifications. Otherwise a concurrent resnapshot can legitimately
	// supersede the notification with test-only stale state.
	if envelope.Method == "thread/status/changed" {
		var params struct {
			ThreadID string    `json:"threadId"`
			Status   rpcStatus `json:"status"`
		}
		if json.Unmarshal(envelope.Params, &params) == nil {
			if p.root.ID == params.ThreadID {
				p.root.Status = params.Status
			}
			for i := range p.descendants {
				if p.descendants[i].ID == params.ThreadID {
					p.descendants[i].Status = params.Status
				}
			}
		}
	}
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

func (p *fakeProxy) DisconnectOnNextList() (<-chan struct{}, func(), <-chan struct{}) {
	p.mu.Lock()
	p.disconnectNextList = true
	entered := p.listEntered
	release := func() { p.listReleaseOnce.Do(func() { close(p.listRelease) }) }
	done := p.listDisconnected
	p.mu.Unlock()
	return entered, release, done
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

type rejectingConnector struct {
	called chan struct{}
	once   sync.Once
	calls  int
}

func (c *rejectingConnector) Connect(context.Context) (Connection, error) {
	c.calls++
	c.once.Do(func() { close(c.called) })
	return nil, errors.New("fake proxy unavailable")
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

func waitCondition(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for condition")
}

func waitCompleteObservation(t *testing.T, observer *Observer, ref provider.RootRef, timeout time.Duration) agentgraph.Observation {
	t.Helper()
	return waitObservation(t, observer, ref, timeout, func(observation agentgraph.Observation) bool { return observation.Complete })
}

// waitUpdateObservation follows the provider invalidation contract: a key says
// to Observe again, but may be an older coalesced snapshot invalidation rather
// than a one-to-one event acknowledgement. It succeeds only after an update
// signal is followed by the requested semantic state.
func waitUpdateObservation(t *testing.T, observer *Observer, ref provider.RootRef, timeout time.Duration, accept func(agentgraph.Observation) bool) agentgraph.Observation {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatal("timed out waiting for observer invalidation and semantic state")
			return agentgraph.Observation{}
		}
		waitUpdate(t, observer.Updates(), remaining)
		observation, err := observer.Observe(context.Background(), ref, time.Now())
		if err == nil && accept(observation) {
			return observation
		}
	}
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
