package agentgraph_test

import (
	"context"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/provider"
)

func TestProviderObserverContractAndRootKey(t *testing.T) {
	t.Parallel()

	var _ provider.Observer = observerStub{}
	started := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	ref := provider.RootRef{PID: 42, StartedAt: started}
	if got, want := ref.Key(), (provider.RootKey{PID: 42, StartedAt: started}); got != want {
		t.Fatalf("RootRef.Key() = %#v, want %#v", got, want)
	}
}

func TestInvalidationQueueIsNonBlockingAndCoalesced(t *testing.T) {
	t.Parallel()

	queue := provider.NewInvalidationQueue(8)
	key := provider.RootKey{PID: 42}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100_000; i++ {
			queue.Signal(key)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Signal blocked on a full invalidation queue")
	}

	select {
	case got := <-queue.Updates():
		if got != key {
			t.Fatalf("Updates() = %#v, want %#v", got, key)
		}
	default:
		t.Fatal("coalesced queue lost every invalidation")
	}
	select {
	case got := <-queue.Updates():
		t.Fatalf("queue did not coalesce duplicate storm; extra %#v", got)
	default:
	}

	queue.Signal(key)
	select {
	case got := <-queue.Updates():
		if got != key {
			t.Fatalf("later Updates() = %#v, want %#v", got, key)
		}
	default:
		t.Fatal("coalescing suppressed a later invalidation after drain")
	}
}

type observerStub struct{}

func (observerStub) Observe(context.Context, provider.RootRef, time.Time) (agentgraph.Observation, error) {
	return agentgraph.Observation{}, nil
}

func (observerStub) Updates() <-chan provider.RootKey { return nil }
func (observerStub) Forget(provider.RootKey)          {}
func (observerStub) Close() error                     { return nil }
