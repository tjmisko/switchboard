package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/provider"
	claudeprovider "github.com/tjmisko/switchboard/internal/provider/claude"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/statusexplain"
)

// scriptedCodexObserver answers Observe with whatever the test scripts next,
// and runs during (if set) while the answer is "in flight": the seam a
// stale-event race test uses to land newer evidence mid-observation.
type scriptedCodexObserver struct {
	mu      sync.Mutex
	answer  agentgraph.Observation
	err     error
	during  func()
	updates chan provider.RootKey
}

func newScriptedCodexObserver() *scriptedCodexObserver {
	return &scriptedCodexObserver{updates: make(chan provider.RootKey, 1)}
}

func (f *scriptedCodexObserver) script(answer agentgraph.Observation, err error) {
	f.mu.Lock()
	f.answer, f.err = answer, err
	f.mu.Unlock()
}

func (f *scriptedCodexObserver) Observe(context.Context, provider.RootRef, time.Time) (agentgraph.Observation, error) {
	f.mu.Lock()
	answer, err, during := f.answer.Clone(), f.err, f.during
	f.during = nil
	f.mu.Unlock()
	if during != nil {
		during()
	}
	return answer, err
}

func (f *scriptedCodexObserver) Updates() <-chan provider.RootKey                   { return f.updates }
func (f *scriptedCodexObserver) Forget(provider.RootKey)                            {}
func (f *scriptedCodexObserver) Close() error                                       { return nil }
func (f *scriptedCodexObserver) RegisterHookBinding(provider.RootKey, string) error { return nil }

var outcomeT0 = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func codexOutcomeGraph(rootID string, runtime agentgraph.RuntimeState, at time.Time, lease time.Duration, outcome agentgraph.Outcome) agentgraph.Observation {
	return agentgraph.Observation{
		Provider: agentgraph.ProviderCodex, RootID: rootID, Source: agentgraph.SourceCodexAppServer,
		ObservedAt: at, FreshUntil: at.Add(lease), Complete: true, Outcome: outcome,
		Nodes: []agentgraph.Node{{ID: rootID, Runtime: runtime, UpdatedAt: at}},
	}
}

// outcomeCoordinator is a coordinator over one Codex root bound to "thread-1"
// whose app-server graph read working at outcomeT0, fresh for a minute.
func outcomeCoordinator(t *testing.T) (*agentCoordinator, *scriptedCodexObserver, provider.RootRef) {
	t.Helper()
	store := state.New("")
	ref := seedCoordinatorSession(store, 980, outcomeT0.Add(-time.Hour), state.AgentKindCodex, "thread-1", "/repo")
	observer := newScriptedCodexObserver()
	c := newAgentCoordinator(store, nil, nil, observer)
	t.Cleanup(c.Close)
	c.refreshTrackedRoots()
	observer.script(codexOutcomeGraph("thread-1", agentgraph.RuntimeActive, outcomeT0, time.Minute, agentgraph.OutcomeUsable), nil)
	c.observeAt(t.Context(), ref, outcomeT0)
	if got := publishedStatus(t, c, ref); got != state.StatusWorking {
		t.Fatalf("setup: published %q, want working", got)
	}
	return c, observer, ref
}

func publishedSession(t *testing.T, c *agentCoordinator, ref provider.RootRef) state.Session {
	t.Helper()
	sess, ok := sessionForKey(c.store.Snapshot(), ref.Key())
	if !ok {
		t.Fatal("root left the store")
	}
	return sess
}

func publishedStatus(t *testing.T, c *agentCoordinator, ref provider.RootRef) string {
	t.Helper()
	sess := publishedSession(t, c, ref)
	if info := sess.Enrichment(); info != nil {
		return info.Status
	}
	return ""
}

// #98 criterion 1: a temporary failure neither fabricates idle nor extends
// freshness, and fresh evidence recovers at once after the deadline.
func TestObserveAtShouldHoldThePriorGraphToItsOriginalDeadlineWhenTheObserverIsUnavailable(t *testing.T) {
	c, observer, ref := outcomeCoordinator(t)
	deadline := outcomeT0.Add(time.Minute)

	// An unavailable answer carrying the prior graph with a renewed window (an
	// observer bug) must not extend it, and neither may one carrying nothing.
	renewed := codexOutcomeGraph("thread-1", agentgraph.RuntimeActive, outcomeT0.Add(30*time.Second), time.Minute, agentgraph.OutcomeUnavailable)
	observer.script(renewed, nil)
	c.observeAt(t.Context(), ref, outcomeT0.Add(30*time.Second))
	observer.script(agentgraph.Observation{Provider: agentgraph.ProviderCodex, RootID: "thread-1", Outcome: agentgraph.OutcomeUnavailable}, nil)
	c.observeAt(t.Context(), ref, outcomeT0.Add(45*time.Second))
	sess := publishedSession(t, c, ref)
	if !sess.AgentGraph.ObservedAt.Equal(outcomeT0) || !sess.AgentGraph.FreshUntil.Equal(deadline) {
		t.Fatalf("graph window = [%v, %v), want the original [%v, %v)", sess.AgentGraph.ObservedAt, sess.AgentGraph.FreshUntil, outcomeT0, deadline)
	}
	if got := publishedStatus(t, c, ref); got != state.StatusWorking {
		t.Fatalf("published %q while unavailable inside the deadline, want the held working", got)
	}

	// At the original deadline the held graph lapses to unknown, never idle.
	c.observeAt(t.Context(), ref, deadline)
	if got := publishedStatus(t, c, ref); got != "" {
		t.Fatalf("published %q at the original deadline, want unknown", got)
	}

	// The first usable answer after the lapse publishes on that same tick.
	recoveredAt := deadline.Add(time.Second)
	observer.script(codexOutcomeGraph("thread-1", agentgraph.RuntimeIdle, recoveredAt, time.Minute, agentgraph.OutcomeUsable), nil)
	c.observeAt(t.Context(), ref, recoveredAt)
	if got := publishedStatus(t, c, ref); got != state.StatusIdle {
		t.Fatalf("published %q on the first fresh answer after expiry, want idle", got)
	}
}

func TestObserveAtShouldNotPublishWhenAnErrorComesWithANewerGraph(t *testing.T) {
	c, observer, ref := outcomeCoordinator(t)
	at := outcomeT0.Add(10 * time.Second)
	observer.script(codexOutcomeGraph("thread-1", agentgraph.RuntimeIdle, at, time.Hour, ""), errors.New("read failed"))
	c.observeAt(t.Context(), ref, at)
	sess := publishedSession(t, c, ref)
	if got := publishedStatus(t, c, ref); got != state.StatusWorking || !sess.AgentGraph.ObservedAt.Equal(outcomeT0) {
		t.Fatalf("an errored answer published %q from %v, want the held working from %v", got, sess.AgentGraph.ObservedAt, outcomeT0)
	}
}

// #98 criterion 2: unsupported coverage and an authoritative reset are
// explained differently and behave differently.
func TestObserveAtShouldExplainAndTreatUnsupportedAndResetDifferentlyWhenAPriorGraphIsFresh(t *testing.T) {
	t.Run("should hold the prior graph to its deadline and then explain coverage_unsupported when unsupported", func(t *testing.T) {
		c, observer, ref := outcomeCoordinator(t)
		observer.script(agentgraph.Observation{Provider: agentgraph.ProviderCodex, Outcome: agentgraph.OutcomeUnsupported}, nil)
		at := outcomeT0.Add(10 * time.Second)
		c.observeAt(t.Context(), ref, at)
		if got := publishedStatus(t, c, ref); got != state.StatusWorking {
			t.Fatalf("unsupported dropped the fresh prior graph: published %q", got)
		}
		lapsed := outcomeT0.Add(2 * time.Minute)
		c.observeAt(t.Context(), ref, lapsed)
		d := explainAt(t, c, ref.PID, lapsed)
		if d.Status != statusexplain.StatusUnknown || d.Reason != statusexplain.ReasonCoverageUnsupported {
			t.Fatalf("decision after the lapse = %+v, want unknown for coverage_unsupported", d.Choice)
		}
	})
	t.Run("should drop the prior graph at once and explain observation_reset when reset", func(t *testing.T) {
		c, observer, ref := outcomeCoordinator(t)
		observer.script(agentgraph.Observation{Provider: agentgraph.ProviderCodex, RootID: "thread-2", Source: agentgraph.SourceCodexAppServer, Outcome: agentgraph.OutcomeReset}, nil)
		at := outcomeT0.Add(10 * time.Second)
		c.observeAt(t.Context(), ref, at)
		sess := publishedSession(t, c, ref)
		if got := publishedStatus(t, c, ref); got != "" || sess.AgentGraph != nil {
			t.Fatalf("reset kept the old conversation's graph: published %q graph=%v", got, sess.AgentGraph)
		}
		d := explainAt(t, c, ref.PID, at)
		if d.Root.SessionID != "thread-2" || d.Status != statusexplain.StatusUnknown || d.Reason != statusexplain.ReasonObservationReset {
			t.Fatalf("decision after reset = root %q %+v, want unknown for observation_reset on thread-2", d.Root.SessionID, d.Choice)
		}
		// Evidence about the new binding decides as soon as it lands.
		next := at.Add(time.Second)
		observer.script(codexOutcomeGraph("thread-2", agentgraph.RuntimeActive, next, time.Minute, agentgraph.OutcomeUsable), nil)
		c.observeAt(t.Context(), ref, next)
		if d := explainAt(t, c, ref.PID, next); d.Status != state.StatusWorking || d.Reason != statusexplain.ReasonGraphAuthority {
			t.Fatalf("decision after the new binding's snapshot = %+v, want working by graph_authority", d.Choice)
		}
	})
}

// A stale-event race: a reset computed before a newer graph landed must not
// drop that graph. The generation fence orders them.
func TestObserveAtShouldNotApplyAResetWhenNewerEvidenceLandedWhileItWasInFlight(t *testing.T) {
	c, observer, ref := outcomeCoordinator(t)
	hookAt := outcomeT0.Add(5 * time.Second)
	observer.mu.Lock()
	observer.during = func() {
		hook := codexOutcomeGraph("thread-1", agentgraph.RuntimeIdle, hookAt, time.Minute, "")
		hook.Source = agentgraph.SourceHook
		if !c.applyObservationAs(ref, c.begin(ref.Key()), hook, claudeprovider.Compatibility{}, hookAt, "", state.GraphHookEvent) {
			t.Error("the racing hook did not land")
		}
	}
	observer.mu.Unlock()
	observer.script(agentgraph.Observation{Provider: agentgraph.ProviderCodex, RootID: "thread-2", Outcome: agentgraph.OutcomeReset}, nil)
	c.observeAt(t.Context(), ref, hookAt.Add(time.Millisecond))
	sess := publishedSession(t, c, ref)
	if sess.AgentGraph == nil || sess.AgentGraph.RootID != "thread-1" {
		t.Fatalf("a fenced reset dropped the graph that landed after it began: %v", sess.AgentGraph)
	}
	if got := publishedStatus(t, c, ref); got != state.StatusIdle {
		t.Fatalf("published %q, want the racing hook's idle", got)
	}
}
