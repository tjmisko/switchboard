package main

import (
	"context"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/provider"
	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/terminal"
	"github.com/tjmisko/switchboard/internal/wm"
)

type adversarialCodexObserver struct {
	mu sync.Mutex

	observation agentgraph.Observation
	updates     chan provider.RootKey
	started     chan struct{}
	release     chan struct{}
	canceled    chan struct{}
	returned    chan struct{}
	startOnce   sync.Once
	cancelOnce  sync.Once
	returnOnce  sync.Once
	forgets     map[provider.RootKey]int
	closes      int
}

func newAdversarialCodexObserver(observation agentgraph.Observation) *adversarialCodexObserver {
	return &adversarialCodexObserver{
		observation: observation, updates: make(chan provider.RootKey, 8),
		started: make(chan struct{}), canceled: make(chan struct{}), returned: make(chan struct{}),
		forgets: make(map[provider.RootKey]int),
	}
}

func (o *adversarialCodexObserver) Observe(ctx context.Context, _ provider.RootRef, _ time.Time) (agentgraph.Observation, error) {
	o.startOnce.Do(func() { close(o.started) })
	select {
	case <-ctx.Done():
		o.cancelOnce.Do(func() { close(o.canceled) })
		o.returnOnce.Do(func() { close(o.returned) })
		return agentgraph.Observation{}, ctx.Err()
	case <-o.release:
		o.mu.Lock()
		observation := o.observation.Clone()
		o.mu.Unlock()
		o.returnOnce.Do(func() { close(o.returned) })
		return observation, nil
	}
}

func (o *adversarialCodexObserver) Updates() <-chan provider.RootKey { return o.updates }

func (o *adversarialCodexObserver) Forget(key provider.RootKey) {
	o.mu.Lock()
	o.forgets[key]++
	o.mu.Unlock()
}

func (o *adversarialCodexObserver) Close() error {
	o.mu.Lock()
	o.closes++
	o.mu.Unlock()
	return nil
}

func (*adversarialCodexObserver) RegisterHookBinding(provider.RootKey, string) error { return nil }

func (o *adversarialCodexObserver) counts(key provider.RootKey) (forgets, closes int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.forgets[key], o.closes
}

func waitForAdversarialCondition(t *testing.T, accept func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if accept() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal(message)
}

func TestProviderUpdateRacingRootRemovalCannotLandStateOrHistory(t *testing.T) {
	store := state.New("")
	startedAt := time.Now().Add(-time.Hour)
	ref := seedCoordinatorSession(store, 4901, startedAt, state.AgentKindCodex, "root", "/project")
	now := time.Now()
	observation := testCodexObservation(ref, "root", now, agentgraph.RuntimeIdle, agentgraph.AttentionNone)
	observation.Nodes = append(observation.Nodes, agentgraph.Node{
		ID: "child", ParentID: "root", Runtime: agentgraph.RuntimeActive,
		Attention: agentgraph.AttentionApproval, Lifecycle: agentgraph.LifecycleRunning,
		StartedAt: startedAt.Add(time.Minute), UpdatedAt: now,
	})
	fake := newAdversarialCodexObserver(observation)
	fake.release = make(chan struct{})
	historyDir := t.TempDir()
	sink := history.NewSink(history.Config{Enabled: true, Detail: history.DetailFull, Dir: historyDir})
	coordinator := newAgentCoordinator(store, sink, nil, fake)
	coordinator.Start(context.Background(), time.Hour)
	sinkClosed := false
	t.Cleanup(func() {
		coordinator.Close()
		if !sinkClosed {
			sink.Close()
		}
	})
	select {
	case <-fake.started:
	case <-time.After(time.Second):
		t.Fatal("provider observation did not block")
	}

	startRace := make(chan struct{})
	var race sync.WaitGroup
	race.Add(2)
	go func() {
		defer race.Done()
		<-startRace
		store.Apply(func(sessions map[int]*state.Session) { delete(sessions, ref.PID) })
		coordinator.RequestCleanup()
	}()
	go func() {
		defer race.Done()
		<-startRace
		fake.updates <- ref.Key()
	}()
	close(startRace)
	race.Wait()
	close(fake.release)

	waitForAdversarialCondition(t, func() bool {
		forgets, _ := fake.counts(ref.Key())
		return forgets == 1
	}, "removed root was not forgotten")
	coordinator.Close()
	if snapshot := store.Snapshot(); len(snapshot.Sessions) != 0 {
		t.Fatalf("provider result resurrected removed root: %+v", snapshot.Sessions)
	}
	forgets, closes := fake.counts(ref.Key())
	if forgets != 1 || closes != 1 {
		t.Fatalf("lifecycle calls after removal: Forget=%d Close=%d, want 1/1", forgets, closes)
	}

	sink.Close()
	sinkClosed = true
	events, err := history.ReadRange(historyDir, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == history.EventAgentState && event.SessionID == "root" {
			t.Fatalf("removed root landed canonical history after cleanup: %+v", event)
		}
	}
}

func TestBlockedProviderLeavesRPCSubscriptionAndGraphHookResponsive(t *testing.T) {
	store := state.New("")
	ref := seedCoordinatorSession(store, 4902, time.Now().Add(-time.Hour), state.AgentKindCodex, "root", "/project")
	fake := newAdversarialCodexObserver(testCodexObservation(
		ref, "root", time.Now(), agentgraph.RuntimeActive, agentgraph.AttentionNone,
	))
	fake.release = make(chan struct{})
	coordinator := newAgentCoordinator(store, nil, nil, fake)
	coordinator.Start(context.Background(), time.Hour)
	t.Cleanup(coordinator.Close)
	select {
	case <-fake.started:
	case <-time.After(time.Second):
		t.Fatal("provider observation did not block")
	}

	server := rpc.New(store, "", terminal.NewNone(), wm.NewNone())
	server.SetAgentHookHandler(coordinator.HandleHook)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	subServer, subClient := net.Pipe()
	hookServer, hookClient := net.Pipe()
	defer subClient.Close()
	defer hookClient.Close()
	subDone, hookDone := make(chan struct{}), make(chan struct{})
	go func() {
		server.ServeConnection(ctx, subServer)
		close(subDone)
	}()
	go func() {
		server.ServeConnection(ctx, hookServer)
		close(hookDone)
	}()
	deadline := time.Now().Add(time.Second)
	_ = subClient.SetDeadline(deadline)
	_ = hookClient.SetDeadline(deadline)
	subEncoder, subDecoder := json.NewEncoder(subClient), json.NewDecoder(subClient)
	if err := subEncoder.Encode(rpc.Request{Cmd: "subscribe"}); err != nil {
		t.Fatal(err)
	}
	var initial rpc.Response
	if err := subDecoder.Decode(&initial); err != nil {
		t.Fatalf("initial subscription frame while provider blocked: %v", err)
	}
	if initial.Snapshot == nil || len(initial.Snapshot.Sessions) != 1 {
		t.Fatalf("initial subscription frame = %+v", initial)
	}

	hookEncoder, hookDecoder := json.NewEncoder(hookClient), json.NewDecoder(hookClient)
	if err := hookEncoder.Encode(rpc.Request{
		Cmd: "hook", PID: ref.PID, Agent: state.AgentKindCodex,
		Event: "PermissionRequest", SessionID: "root", ToolName: "Bash",
	}); err != nil {
		t.Fatal(err)
	}
	var hookResponse rpc.Response
	if err := hookDecoder.Decode(&hookResponse); err != nil {
		t.Fatalf("graph-aware hook RPC blocked behind provider: %v", err)
	}
	if !hookResponse.OK {
		t.Fatalf("graph-aware hook response = %+v", hookResponse)
	}
	var update rpc.Response
	if err := subDecoder.Decode(&update); err != nil {
		t.Fatalf("subscription publication blocked behind provider: %v", err)
	}
	if update.Snapshot == nil || len(update.Snapshot.Sessions) != 1 {
		t.Fatalf("subscription update = %+v", update)
	}
	graph := update.Snapshot.Sessions[0].AgentGraph
	if graph == nil || graph.Source != agentgraph.SourceHook || graph.Summary.Status != state.StatusWorking {
		t.Fatalf("subscription did not publish graph-aware hook fallback: %#v", graph)
	}
	select {
	case <-fake.returned:
		t.Fatal("provider was released before RPC and subscription completed")
	default:
	}

	close(fake.release)
	select {
	case <-fake.returned:
	case <-time.After(time.Second):
		t.Fatal("released provider did not return")
	}
	_ = subClient.Close()
	_ = hookClient.Close()
	cancel()
	for name, done := range map[string]<-chan struct{}{"subscribe": subDone, "hook": hookDone} {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("%s RPC connection did not close", name)
		}
	}
}

func TestCoordinatorCloseCancelsBlockedObserveAndJoinsRunLoop(t *testing.T) {
	store := state.New("")
	ref := seedCoordinatorSession(store, 4903, time.Now().Add(-time.Hour), state.AgentKindCodex, "root", "/project")
	fake := newAdversarialCodexObserver(testCodexObservation(
		ref, "root", time.Now(), agentgraph.RuntimeActive, agentgraph.AttentionNone,
	))
	// A nil release channel makes context cancellation the only Observe exit.
	coordinator := newAgentCoordinator(store, nil, nil, fake)
	coordinator.Start(context.Background(), time.Hour)
	select {
	case <-fake.started:
	case <-time.After(time.Second):
		t.Fatal("provider observation did not block")
	}

	closed := make(chan struct{})
	go func() {
		coordinator.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel blocked Observe and join the coordinator run loop")
	}
	select {
	case <-fake.canceled:
	default:
		t.Fatal("blocked provider did not observe coordinator cancellation")
	}
	select {
	case <-fake.returned:
	default:
		t.Fatal("blocked provider Observe had not returned when Close completed")
	}
	coordinator.Close()
	_, closes := fake.counts(ref.Key())
	if closes != 1 {
		t.Fatalf("provider Close calls = %d, want exactly 1", closes)
	}
}
