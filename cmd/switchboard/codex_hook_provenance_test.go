package main

import (
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/provider"
	claudeprovider "github.com/tjmisko/switchboard/internal/provider/claude"
	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/state"
)

// These tests pin the provenance half of the memory-footprint fix. A root hook
// is an immediate status edge, not a new observation of the app-server's child
// topology. Relabelling the whole composed graph as hook-derived both drove the
// fresh_until publish oscillation and wrote fabricated child provenance into
// the history day-file consumed by the dashboard (plan §1.4.3, issue #83).

var provenanceBase = time.Date(2026, 8, 23, 16, 0, 0, 0, time.UTC)

func provenanceRef() provider.RootRef {
	return provider.RootRef{
		PID: 8451, StartedAt: provenanceBase.Add(-time.Hour), Provider: agentgraph.ProviderCodex,
		ProviderSessionID: "root", CWD: "/project",
	}
}

func provenanceAppServerObservation(ref provider.RootRef, at time.Time, runtime agentgraph.RuntimeState) agentgraph.Observation {
	observation := testCodexObservation(ref, "root", at, runtime, agentgraph.AttentionNone)
	observation.FreshUntil = at.Add(15 * time.Second)
	observation.Nodes = append(observation.Nodes, agentgraph.Node{
		ID: "child", ParentID: "root", Nickname: "worker", Runtime: agentgraph.RuntimeActive,
		Attention: agentgraph.AttentionNone, Lifecycle: agentgraph.LifecycleRunning,
		StartedAt: at.Add(-time.Minute), UpdatedAt: at,
	})
	return observation
}

func provenanceGraph(t *testing.T, observation agentgraph.Observation) *state.AgentGraph {
	t.Helper()
	graph, err := state.ProjectAgentGraph(observation, nil, observation.ObservedAt)
	if err != nil {
		t.Fatal(err)
	}
	return graph
}

func provenanceHook(t *testing.T, event string, at time.Time) agentgraph.Observation {
	t.Helper()
	hook, mapped := codexHookObservation("root", rpc.Request{Event: event}, provenanceBase.Add(-time.Hour), at)
	if !mapped {
		t.Fatalf("event %q did not map to a Codex hook observation", event)
	}
	return hook
}

func TestShouldPreserveLiveAppServerProvenanceCompletenessAndHorizonWhenAHookComposes(t *testing.T) {
	ref := provenanceRef()
	providerAt := provenanceBase
	current := provenanceGraph(t, provenanceAppServerObservation(ref, providerAt, agentgraph.RuntimeIdle))
	hookAt := providerAt.Add(time.Second)
	hook := provenanceHook(t, "UserPromptSubmit", hookAt)

	overlay := overlayCodexHookObservation(hook, current)
	if overlay.Source != agentgraph.SourceCodexAppServer {
		t.Fatalf("Source = %q, want codex_app_server: a root hook must not relabel child topology", overlay.Source)
	}
	if !overlay.Complete {
		t.Fatal("Complete became false even though the composed topology is the same complete app-server graph")
	}
	if !overlay.FreshUntil.Equal(current.FreshUntil) {
		t.Fatalf("FreshUntil = %v, want app-server horizon %v", overlay.FreshUntil, current.FreshUntil)
	}
	if overlay.FreshUntil.Equal(hookAt.Add(codexHookActiveFreshness)) {
		t.Fatal("FreshUntil imported the hook fallback horizon onto a live app-server graph")
	}
	if overlay.Diagnostic != codexComposedObservationDiagnostic {
		t.Fatalf("Diagnostic = %q, want composed-observation marker", overlay.Diagnostic)
	}
	if overlay.Nodes[0].Runtime != agentgraph.RuntimeActive {
		t.Fatalf("root Runtime = %q, want the hook's immediate active edge", overlay.Nodes[0].Runtime)
	}
	if len(overlay.Nodes) != 2 || overlay.Nodes[1].ID != "child" {
		t.Fatalf("hook erased app-server topology: %#v", overlay.Nodes)
	}
}

func TestShouldPreserveTheSameFieldsEndToEndThroughHandleHook(t *testing.T) {
	ref := provenanceRef()
	store := state.New("")
	seedCoordinatorSession(store, ref.PID, ref.StartedAt, state.AgentKindCodex, "root", ref.CWD)
	coordinator := newAgentCoordinator(store, nil, nil, nil)
	coordinator.refreshTrackedRoots()
	defer coordinator.Close()

	providerAt := provenanceBase
	providerObservation := provenanceAppServerObservation(ref, providerAt, agentgraph.RuntimeIdle)
	if !coordinator.applyObservation(ref, coordinator.begin(ref.Key()), providerObservation, claudeprovider.Compatibility{}, providerAt) {
		t.Fatal("app-server observation was not applied")
	}
	hookAt := providerAt.Add(time.Second)
	sendCodexHook(coordinator, store, rpc.Request{Event: "UserPromptSubmit", SessionID: "root", ObservedAt: hookAt})

	graph := codexGraph(t, store)
	if graph.Source != agentgraph.SourceCodexAppServer || !graph.Complete ||
		!graph.FreshUntil.Equal(providerObservation.FreshUntil) {
		t.Fatalf("stored composed graph lost app-server authority: %#v", graph)
	}
	if graph.Summary.Status != state.StatusWorking || len(graph.Nodes) != 2 {
		t.Fatalf("stored graph lost the hook edge or provider topology: %#v", graph)
	}
}

func TestShouldStillApplyTheHookFallbackWhenNoUsableAppServerGraphExists(t *testing.T) {
	hookAt := provenanceBase.Add(time.Second)
	hook := provenanceHook(t, "UserPromptSubmit", hookAt)

	if got := overlayCodexHookObservation(hook, nil); got.Source != agentgraph.SourceHook ||
		!got.FreshUntil.Equal(hook.FreshUntil) || got.Complete != hook.Complete {
		t.Fatalf("nil-current fallback changed: %#v", got)
	}

	ref := provenanceRef()
	for _, runtime := range []agentgraph.RuntimeState{agentgraph.RuntimeUnknown, agentgraph.RuntimeNotLoaded} {
		t.Run(string(runtime), func(t *testing.T) {
			current := provenanceGraph(t, provenanceAppServerObservation(ref, provenanceBase, runtime))
			overlay := overlayCodexHookObservation(hook, current)
			if overlay.Source != agentgraph.SourceHook || overlay.Complete || !overlay.FreshUntil.Equal(hook.FreshUntil) {
				t.Fatalf("unavailable-root fallback = %#v", overlay)
			}
			if overlay.Nodes[0].Runtime != agentgraph.RuntimeActive || len(overlay.Nodes) != 2 {
				t.Fatalf("fallback did not overlay the root while retaining topology: %#v", overlay.Nodes)
			}
		})
	}

	hookCurrent := provenanceGraph(t, hook)
	later := provenanceHook(t, "Stop", hookAt.Add(time.Second))
	if got := overlayCodexHookObservation(later, hookCurrent); got.Source != agentgraph.SourceHook ||
		!got.FreshUntil.Equal(later.FreshUntil) {
		t.Fatalf("hook-on-hook fallback adopted non-hook authority: %#v", got)
	}
}

func TestShouldNotMoveFreshUntilAcrossABurstOfHooksOnOneAppServerHorizon(t *testing.T) {
	ref := provenanceRef()
	providerObservation := provenanceAppServerObservation(ref, provenanceBase, agentgraph.RuntimeIdle)
	current := provenanceGraph(t, providerObservation)

	for i := 1; i <= 10; i++ {
		hookAt := provenanceBase.Add(time.Duration(i) * time.Second)
		hook := provenanceHook(t, "UserPromptSubmit", hookAt)
		overlay := overlayCodexHookObservation(hook, current)
		if !overlay.FreshUntil.Equal(providerObservation.FreshUntil) {
			t.Fatalf("hook %d moved FreshUntil to %v, want fixed %v", i, overlay.FreshUntil, providerObservation.FreshUntil)
		}
		var err error
		current, err = state.ProjectAgentGraph(overlay, current, hookAt)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestShouldStillComposeTheHookRootOntoALaterNotLoadedSnapshotForTheHooksOwnDeadline(t *testing.T) {
	ref := provenanceRef()
	store := state.New("")
	seedCoordinatorSession(store, ref.PID, ref.StartedAt, state.AgentKindCodex, "root", ref.CWD)
	coordinator := newAgentCoordinator(store, nil, nil, nil)
	coordinator.refreshTrackedRoots()
	defer coordinator.Close()

	providerObservation := provenanceAppServerObservation(ref, provenanceBase, agentgraph.RuntimeIdle)
	if !coordinator.applyObservation(ref, coordinator.begin(ref.Key()), providerObservation, claudeprovider.Compatibility{}, provenanceBase) {
		t.Fatal("known app-server graph was not applied")
	}
	hookAt := provenanceBase.Add(time.Second)
	sendCodexHook(coordinator, store, rpc.Request{Event: "UserPromptSubmit", SessionID: "root", ObservedAt: hookAt})
	if got := codexGraph(t, store).FreshUntil; !got.Equal(providerObservation.FreshUntil) {
		t.Fatalf("published hook composition = %v, want app-server horizon %v", got, providerObservation.FreshUntil)
	}

	notLoadedAt := hookAt.Add(time.Second)
	notLoaded := provenanceAppServerObservation(ref, notLoadedAt, agentgraph.RuntimeNotLoaded)
	notLoaded.Nodes[0].Lifecycle = agentgraph.LifecycleUnknown
	// Standalone topology leases outlive the exact hook edge. Composition must
	// retain the topology while bounding root status by the hook's own deadline.
	notLoaded.FreshUntil = notLoadedAt.Add(24 * time.Hour)
	if !coordinator.applyObservation(ref, coordinator.begin(ref.Key()), notLoaded, claudeprovider.Compatibility{}, notLoadedAt) {
		t.Fatal("later notLoaded app-server graph was not applied")
	}
	graph := codexGraph(t, store)
	wantFallback := hookAt.Add(codexHookActiveFreshness)
	if graph.Source != agentgraph.SourceCodexAppServer || graph.Summary.Status != state.StatusWorking ||
		!graph.FreshUntil.Equal(wantFallback) {
		t.Fatalf("later notLoaded graph lost the hook's own fallback deadline %v: %#v", wantFallback, graph)
	}
}

func TestShouldNotSupersedeAChildHookOverlayWhenARootHookComposes(t *testing.T) {
	coordinator, store, ref, base := newCodexChildHookCoordinator(t, nil)
	applyCodexChildTopology(t, coordinator, ref, codexChildTopology(ref, base, true,
		agentgraph.Node{ID: "child", ParentID: "root", Runtime: agentgraph.RuntimeNotLoaded,
			Attention: agentgraph.AttentionNone, Lifecycle: agentgraph.LifecycleUnknown, UpdatedAt: base},
	), base)

	childAt := base.Add(time.Second)
	sendCodexChildHook(coordinator, store, "SubagentStart", "root", "child", childAt)
	coordinator.reconcileCodexChildHooks(ref, childAt)
	before := childNode(t, codexGraph(t, store), "child")
	if before.Runtime != agentgraph.RuntimeActive || before.Lifecycle != agentgraph.LifecycleRunning {
		t.Fatalf("child hook did not establish its overlay: %#v", before)
	}

	sendCodexHook(coordinator, store, rpc.Request{
		Event: "UserPromptSubmit", SessionID: "root", ObservedAt: base.Add(2 * time.Second),
	})
	after := childNode(t, codexGraph(t, store), "child")
	if after.Runtime != agentgraph.RuntimeActive || after.Lifecycle != agentgraph.LifecycleRunning {
		t.Fatalf("root hook destroyed the retained child overlay: %#v", after)
	}
	if got := diagnosticCount(coordinator, "subagent_hook_provider_superseded"); got != 0 {
		t.Fatalf("root composition falsely superseded the child overlay %d time(s)", got)
	}
}

func TestShouldMarkNoAdditionalNodeNotFoundWhenAHookComposesOnACompleteGraph(t *testing.T) {
	historyDir := t.TempDir()
	sink, flushHistory := newRecordingSink(t, historyDir)
	coordinator, store, ref, base := newCodexChildHookCoordinator(t, sink)
	initial := codexChildTopology(ref, base, true,
		agentgraph.Node{ID: "child-a", ParentID: "root", Runtime: agentgraph.RuntimeActive, Lifecycle: agentgraph.LifecycleRunning, UpdatedAt: base},
		agentgraph.Node{ID: "child-b", ParentID: "root", Runtime: agentgraph.RuntimeIdle, Lifecycle: agentgraph.LifecycleCompleted, UpdatedAt: base},
	)
	applyCodexChildTopology(t, coordinator, ref, initial, base)

	// A real complete provider snapshot drops child-b and earns exactly one
	// not_found edge. The following root hook composes on that new complete graph
	// and must not manufacture or repeat another child disappearance.
	droppedAt := base.Add(time.Second)
	dropped := codexChildTopology(ref, droppedAt, true,
		agentgraph.Node{ID: "child-a", ParentID: "root", Runtime: agentgraph.RuntimeActive, Lifecycle: agentgraph.LifecycleRunning, UpdatedAt: droppedAt},
	)
	applyCodexChildTopology(t, coordinator, ref, dropped, droppedAt)
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "UserPromptSubmit", SessionID: "root", ObservedAt: base.Add(2 * time.Second),
	})
	flushHistory()

	var missing []history.Event
	for _, event := range eventsOfType(readEvents(t, historyDir), history.EventAgentState) {
		if event.ThreadID == "child-b" && event.ToLifecycle == agentgraph.LifecycleNotFound {
			missing = append(missing, event)
		}
	}
	if len(missing) != 1 || missing[0].Source != agentgraph.SourceCodexAppServer || !missing[0].Ts.Equal(droppedAt) {
		t.Fatalf("child not_found rows = %+v, want one provider-earned edge at %v", missing, droppedAt)
	}
}

func TestShouldEmitNoChildAgentStateRowCarryingSourceHookAfterAForgetFollowedByAHookFrame(t *testing.T) {
	historyDir := t.TempDir()
	sink, flushHistory := newRecordingSink(t, historyDir)
	coordinator, store, ref, base := newCodexChildHookCoordinator(t, sink)
	applyCodexChildTopology(t, coordinator, ref, codexChildTopology(ref, base, true,
		agentgraph.Node{ID: "child-a", ParentID: "root", Runtime: agentgraph.RuntimeActive, Lifecycle: agentgraph.LifecycleRunning, UpdatedAt: base},
		agentgraph.Node{ID: "child-b", ParentID: "root", Runtime: agentgraph.RuntimeIdle, Lifecycle: agentgraph.LifecycleCompleted, UpdatedAt: base},
	), base)

	// Production Forget calls come from root replacement, session loss, provider
	// cancellation, and failed application paths. Force it directly so the test
	// isolates the provenance of the first composed frame after that reset.
	coordinator.history.Forget(agentgraph.ProviderCodex, "root")
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "UserPromptSubmit", SessionID: "root", ObservedAt: base.Add(time.Second),
	})
	flushHistory()

	events := eventsOfType(readEvents(t, historyDir), history.EventAgentState)
	childRows, appServerRows := 0, 0
	for _, event := range events {
		if event.ParentThreadID == "" {
			continue
		}
		childRows++
		if event.Source == agentgraph.SourceHook {
			t.Fatalf("composed child row carried fabricated hook provenance: %+v", event)
		}
		if event.Source == agentgraph.SourceCodexAppServer {
			appServerRows++
		}
	}
	if childRows < 2 || appServerRows != childRows {
		t.Fatalf("child history positive control failed: child rows=%d app-server rows=%d events=%+v", childRows, appServerRows, events)
	}
}
