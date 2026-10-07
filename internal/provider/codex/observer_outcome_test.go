package codex

import (
	"context"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/provider"
)

func newOutcomeTestObserver(t *testing.T) *Observer {
	t.Helper()
	observer := NewObserver(Config{
		Connector: newFakeProxy(), ResnapshotInterval: time.Hour,
		RequestTimeout: time.Second, ReconnectMinimum: 5 * time.Millisecond,
		ReconnectMaximum: 10 * time.Millisecond, Jitter: func(time.Duration) time.Duration { return 0 },
	})
	t.Cleanup(func() { observer.Close() })
	return observer
}

func TestObserveShouldReportUnsupportedWhenTheRootHasNoProcessStartIdentity(t *testing.T) {
	observer := newOutcomeTestObserver(t)
	observation, err := observer.Observe(context.Background(), provider.RootRef{PID: 9, Provider: agentgraph.ProviderCodex}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if observation.Outcome != agentgraph.OutcomeUnsupported || observation.RootID != "" {
		t.Fatalf("observation = %#v, want unsupported with no root", observation)
	}
}

func TestObserveShouldReportUnavailableWhenNoThreadIsBoundOrItsSnapshotIsPending(t *testing.T) {
	observer := newOutcomeTestObserver(t)
	key := provider.RootKey{PID: 10, StartedAt: time.Unix(10, 0)}
	ref := provider.RootRef{PID: key.PID, StartedAt: key.StartedAt, Provider: agentgraph.ProviderCodex}
	unbound, err := observer.Observe(context.Background(), ref, time.Now())
	if err != nil || unbound.Outcome != agentgraph.OutcomeUnavailable || unbound.RootID != "" {
		t.Fatalf("unbound observation = %#v, %v; want unavailable with no root", unbound, err)
	}
	if err := observer.RegisterHookBinding(key, "root"); err != nil {
		t.Fatal(err)
	}
	pending, err := observer.Observe(context.Background(), ref, time.Now())
	if err != nil || pending.Outcome != agentgraph.OutcomeUnavailable || pending.RootID != "root" || len(pending.Nodes) != 0 {
		t.Fatalf("pending observation = %#v, %v; want unavailable placeholder for root", pending, err)
	}
}

func TestObserveShouldReportUsableWhenTheCachedSnapshotIsComplete(t *testing.T) {
	observer := newOutcomeTestObserver(t)
	key := provider.RootKey{PID: 11, StartedAt: time.Unix(11, 0)}
	ref := provider.RootRef{PID: key.PID, StartedAt: key.StartedAt, Provider: agentgraph.ProviderCodex}
	if err := observer.RegisterHookBinding(key, "root"); err != nil {
		t.Fatal(err)
	}
	if _, err := observer.Observe(context.Background(), ref, time.Now()); err != nil {
		t.Fatal(err)
	}
	complete := waitCompleteObservation(t, observer, ref, time.Second)
	if complete.Outcome != agentgraph.OutcomeUsable {
		t.Fatalf("complete snapshot outcome = %q, want usable", complete.Outcome)
	}
}

func TestObserveShouldReportResetOnceWhenTheBindingMovesToAnotherThread(t *testing.T) {
	observer := newOutcomeTestObserver(t)
	key := provider.RootKey{PID: 12, StartedAt: time.Unix(12, 0)}
	ref := provider.RootRef{PID: key.PID, StartedAt: key.StartedAt, Provider: agentgraph.ProviderCodex}
	if err := observer.RegisterHookBinding(key, "root"); err != nil {
		t.Fatal(err)
	}
	if _, err := observer.Observe(context.Background(), ref, time.Now()); err != nil {
		t.Fatal(err)
	}
	if update, err := observer.ReconcileHookBinding(key, "next"); err != nil || !update.Rotated {
		t.Fatalf("rotation = %#v, %v", update, err)
	}
	reset, err := observer.Observe(context.Background(), ref, time.Now())
	if err != nil || reset.Outcome != agentgraph.OutcomeReset || reset.RootID != "next" || len(reset.Nodes) != 0 {
		t.Fatalf("rebound observation = %#v, %v; want reset naming the new thread", reset, err)
	}
	again, err := observer.Observe(context.Background(), ref, time.Now())
	if err != nil || again.Outcome == agentgraph.OutcomeReset {
		t.Fatalf("second observation after the rebind = %#v, %v; the reset must be reported once", again, err)
	}
}
