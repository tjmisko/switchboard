package codex

import (
	"context"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/provider"
)

func TestSlotObserverDiscoversLoadedThreadAndIsolatesEndpoints(t *testing.T) {
	first := newFakeProxy()
	first.SetSnapshot(rpcThread{ID: "thread-a", Name: "alpha", CreatedAt: 10, Status: rpcStatus{Type: "active"}}, nil)
	second := newFakeProxy()
	second.SetSnapshot(rpcThread{ID: "thread-b", Name: "beta", CreatedAt: 20, Status: rpcStatus{Type: "idle"}}, nil)
	connectors := map[string]Connector{"/a.sock": first, "/b.sock": second}
	observer := NewSlotObserver(Config{
		EndpointConnector: func(endpoint string) Connector { return connectors[endpoint] },
		Environment:       &fakeEnvironment{}, Freshness: time.Second, ResnapshotInterval: time.Hour,
		RequestTimeout: time.Second, ReconnectMinimum: time.Millisecond, ReconnectMaximum: 2 * time.Millisecond,
		Jitter: func(time.Duration) time.Duration { return 0 },
	})
	defer observer.Close()
	refs := []provider.RootRef{
		{PID: 11, StartedAt: time.Unix(11, 0), SlotID: "slot-a", ProviderEndpoint: "/a.sock", Provider: agentgraph.ProviderCodex},
		{PID: 12, StartedAt: time.Unix(12, 0), SlotID: "slot-b", ProviderEndpoint: "/b.sock", Provider: agentgraph.ProviderCodex},
	}
	for _, ref := range refs {
		if _, err := observer.Observe(context.Background(), ref, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	want := []struct {
		id, name string
		runtime  agentgraph.RuntimeState
	}{{"thread-a", "alpha", agentgraph.RuntimeActive}, {"thread-b", "beta", agentgraph.RuntimeIdle}}
	for i, ref := range refs {
		deadline := time.Now().Add(time.Second)
		for {
			observation, err := observer.Observe(context.Background(), ref, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if observation.Complete {
				if observation.RootID != want[i].id {
					t.Fatalf("slot %d root = %q, want %q", i, observation.RootID, want[i].id)
				}
				root := findNode(observation, want[i].id)
				if root == nil || root.Nickname != want[i].name || root.Runtime != want[i].runtime {
					t.Fatalf("slot %d root = %+v", i, root)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("slot %d never completed: %+v", i, observation)
			}
			time.Sleep(time.Millisecond)
		}
	}
	if reads := first.Methods(); !containsMethod(reads, "thread/loaded/list") {
		t.Fatalf("first endpoint methods = %v", reads)
	}
}

func TestSlotObserverPollRotatesRapidClearAndRejectsRetiredRoot(t *testing.T) {
	proxy := newFakeProxy()
	proxy.SetSnapshot(rpcThread{ID: "one", CreatedAt: 1, Status: rpcStatus{Type: "idle"}}, nil)
	observer := NewSlotObserver(Config{
		EndpointConnector: func(string) Connector { return proxy }, Environment: &fakeEnvironment{},
		Freshness: time.Second, ResnapshotInterval: time.Hour, RequestTimeout: time.Second,
		ReconnectMinimum: time.Millisecond, ReconnectMaximum: 2 * time.Millisecond, Jitter: func(time.Duration) time.Duration { return 0 },
	})
	defer observer.Close()
	ref := provider.RootRef{PID: 21, StartedAt: time.Unix(21, 0), SlotID: "slot", ProviderEndpoint: "/slot.sock", Provider: agentgraph.ProviderCodex}
	if _, err := observer.Observe(context.Background(), ref, time.Now()); err != nil {
		t.Fatal(err)
	}
	waitSlotRoot(t, observer, ref, "one")

	for _, next := range []struct {
		id      string
		created int64
	}{{"two", 2}, {"three", 3}} {
		proxy.SetSnapshot(rpcThread{ID: next.id, CreatedAt: next.created, Status: rpcStatus{Type: "active"}}, nil)
		observer.mu.Lock()
		child := observer.slots[ref.Key()].observer
		observer.mu.Unlock()
		child.signalRefresh()
		waitSlotRoot(t, observer, ref, next.id)
	}

	// A delayed loaded-list result naming a retired root cannot rotate backwards.
	proxy.SetSnapshot(rpcThread{ID: "one", CreatedAt: 4, Status: rpcStatus{Type: "idle"}}, nil)
	observer.mu.Lock()
	child := observer.slots[ref.Key()].observer
	observer.mu.Unlock()
	child.signalRefresh()
	time.Sleep(20 * time.Millisecond)
	observation, _ := observer.Observe(context.Background(), ref, time.Now())
	if observation.RootID != "three" {
		t.Fatalf("retired root rotated observer backwards: %+v", observation)
	}
}

func TestSlotObserverAdvancesNameGenerationOnHookRotation(t *testing.T) {
	proxy := newFakeProxy()
	proxy.SetSnapshot(rpcThread{ID: "one", CreatedAt: 1, Status: rpcStatus{Type: "idle"}}, nil)
	observer := NewSlotObserver(Config{
		EndpointConnector: func(string) Connector { return proxy }, Environment: &fakeEnvironment{},
		Freshness: time.Second, ResnapshotInterval: time.Hour, RequestTimeout: time.Second,
		ReconnectMinimum: time.Millisecond, ReconnectMaximum: 2 * time.Millisecond, Jitter: func(time.Duration) time.Duration { return 0 },
	})
	defer observer.Close()
	ref := provider.RootRef{
		PID: 31, StartedAt: time.Unix(31, 0), SlotID: "slot", ProviderEndpoint: "/slot.sock",
		Provider: agentgraph.ProviderCodex, ProviderSessionID: "one", BindingGeneration: 1,
	}
	if _, err := observer.Observe(context.Background(), ref, time.Now()); err != nil {
		t.Fatal(err)
	}
	waitSlotRoot(t, observer, ref, "one")

	update, err := observer.ReconcileHookBinding(ref.Key(), "two")
	if err != nil || !update.Rotated || update.Generation != 2 {
		t.Fatalf("rotation = %+v, %v", update, err)
	}
	proxy.SetSnapshot(rpcThread{ID: "two", CreatedAt: 2, Status: rpcStatus{Type: "idle"}}, nil)
	observer.mu.Lock()
	observer.slots[ref.Key()].observer.signalRefresh()
	observer.mu.Unlock()
	ref.ProviderSessionID, ref.BindingGeneration = "two", 2
	waitSlotRoot(t, observer, ref, "two")
	if err := observer.SetThreadName(context.Background(), ref.Key(), "two", 2, "new-name"); err != nil {
		t.Fatalf("current-generation name rejected: %v", err)
	}
	if err := observer.SetThreadName(context.Background(), ref.Key(), "two", 1, "stale-name"); err == nil {
		t.Fatal("stale generation name was accepted")
	}
}

func waitSlotRoot(t *testing.T, observer *SlotObserver, ref provider.RootRef, rootID string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		observation, err := observer.Observe(context.Background(), ref, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if observation.Complete && observation.RootID == rootID {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("slot never reached root %q: %+v", rootID, observation)
		}
		time.Sleep(time.Millisecond)
	}
}

func containsMethod(methods []string, want string) bool {
	for _, method := range methods {
		if method == want {
			return true
		}
	}
	return false
}
