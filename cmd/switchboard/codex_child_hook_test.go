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

func newCodexChildHookCoordinator(t *testing.T, sink *history.Sink) (*agentCoordinator, *state.Store, provider.RootRef, time.Time) {
	t.Helper()
	base := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Minute)
	store := state.New("")
	ref := seedCoordinatorSession(store, 8301, base.Add(-time.Hour), state.AgentKindCodex, "root", "/project")
	coordinator := newAgentCoordinator(store, sink, nil, nil)
	coordinator.refreshTrackedRoots()
	t.Cleanup(coordinator.Close)
	return coordinator, store, ref, base
}

func codexChildTopology(ref provider.RootRef, observedAt time.Time, complete bool, nodes ...agentgraph.Node) agentgraph.Observation {
	observation := agentgraph.Observation{
		Provider: agentgraph.ProviderCodex, RootID: "root", Source: agentgraph.SourceCodexAppServer,
		ObservedAt: observedAt, FreshUntil: observedAt.Add(time.Hour), Complete: complete,
		Nodes: []agentgraph.Node{{
			ID: "root", Runtime: agentgraph.RuntimeIdle, Attention: agentgraph.AttentionNone,
			Lifecycle: agentgraph.LifecycleRunning, StartedAt: ref.StartedAt, UpdatedAt: observedAt,
		}},
	}
	observation.Nodes = append(observation.Nodes, nodes...)
	return observation
}

func applyCodexChildTopology(t *testing.T, coordinator *agentCoordinator, ref provider.RootRef, observation agentgraph.Observation, now time.Time) {
	t.Helper()
	if !coordinator.applyObservation(ref, coordinator.begin(ref.Key()), observation, claudeprovider.Compatibility{}, now) {
		t.Fatal("Codex topology observation was not applied")
	}
}

func sendCodexChildHook(coordinator *agentCoordinator, store *state.Store, event, rootID, childID string, at time.Time) {
	coordinator.HandleHook(rpc.Request{
		Agent: state.AgentKindCodex, Event: event, SessionID: rootID, AgentID: childID, ObservedAt: at,
	}, store.Snapshot().Sessions[0])
}

func childNode(t *testing.T, graph *state.AgentGraph, id string) state.AgentNode {
	t.Helper()
	if graph != nil {
		for _, node := range graph.Nodes {
			if node.ID == id {
				return node
			}
		}
	}
	t.Fatalf("child %q not found in graph %#v", id, graph)
	return state.AgentNode{}
}

func diagnosticCount(coordinator *agentCoordinator, category string) uint64 {
	for _, diagnostic := range coordinator.Diagnostics() {
		if diagnostic.Provider == string(agentgraph.ProviderCodex) && diagnostic.Category == category {
			return diagnostic.Count
		}
	}
	return 0
}

func TestCodexChildHookStartStopDuplicateAndReactivation(t *testing.T) {
	historyDir := t.TempDir()
	sink := history.NewSink(history.Config{Enabled: true, Detail: history.DetailFull, Dir: historyDir})
	coordinator, store, ref, base := newCodexChildHookCoordinator(t, sink)
	applyCodexChildTopology(t, coordinator, ref, codexChildTopology(ref, base, true, agentgraph.Node{
		ID: "child", ParentID: "root", Runtime: agentgraph.RuntimeNotLoaded,
		Attention: agentgraph.AttentionNone, Lifecycle: agentgraph.LifecycleUnknown, UpdatedAt: base,
	}), base)
	if got := codexGraph(t, store).Summary.LiveChildren; got != 0 {
		t.Fatalf("topology-only live children = %d, want 0", got)
	}

	startAt := base.Add(time.Second)
	sendCodexChildHook(coordinator, store, "SubagentStart", "root", "child", startAt)
	coordinator.reconcileCodexChildHooks(ref, startAt)
	started := codexGraph(t, store)
	startedChild := childNode(t, started, "child")
	if started.Source != agentgraph.SourceCodexAppServer || started.Summary.LiveChildren != 1 ||
		startedChild.Runtime != agentgraph.RuntimeActive || startedChild.Lifecycle != agentgraph.LifecycleRunning {
		t.Fatalf("started child graph = %#v", started)
	}

	stopAt := base.Add(2 * time.Second)
	sendCodexChildHook(coordinator, store, "SubagentStop", "root", "child", stopAt)
	coordinator.reconcileCodexChildHooks(ref, stopAt)
	stopped := childNode(t, codexGraph(t, store), "child")
	if stopped.Runtime != agentgraph.RuntimeIdle || stopped.Lifecycle != agentgraph.LifecycleCompleted ||
		!stopped.CompletedAt.Equal(stopAt) || codexGraph(t, store).Summary.LiveChildren != 0 {
		t.Fatalf("stopped child = %#v", stopped)
	}

	// An exact replay is idempotent, while a later start reopens the same node.
	duplicateAt := stopAt.Add(500 * time.Millisecond)
	sendCodexChildHook(coordinator, store, "SubagentStop", "root", "child", duplicateAt)
	coordinator.reconcileCodexChildHooks(ref, duplicateAt)
	if got := childNode(t, codexGraph(t, store), "child").CompletedAt; !got.Equal(stopAt) {
		t.Fatalf("duplicate stop moved completed_at to %v, want %v", got, stopAt)
	}
	// The duplicate's newer cursor still fences an older delayed start.
	sendCodexChildHook(coordinator, store, "SubagentStart", "root", "child", stopAt.Add(250*time.Millisecond))
	coordinator.reconcileCodexChildHooks(ref, duplicateAt)
	if got := childNode(t, codexGraph(t, store), "child").Lifecycle; got != agentgraph.LifecycleCompleted {
		t.Fatalf("delayed start after duplicate stop reopened lifecycle to %q", got)
	}
	restartAt := base.Add(3 * time.Second)
	sendCodexChildHook(coordinator, store, "SubagentStart", "root", "child", restartAt)
	coordinator.reconcileCodexChildHooks(ref, restartAt)
	restarted := childNode(t, codexGraph(t, store), "child")
	if restarted.Runtime != agentgraph.RuntimeActive || restarted.Lifecycle != agentgraph.LifecycleRunning ||
		!restarted.CompletedAt.IsZero() || codexGraph(t, store).Summary.LiveChildren != 1 {
		t.Fatalf("reactivated child = %#v", restarted)
	}

	sink.Close()
	events, err := history.ReadRange(historyDir, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var childEvents []history.Event
	for _, event := range events {
		if event.Type == history.EventAgentState && event.ThreadID == "child" &&
			event.FromLifecycle != event.ToLifecycle {
			childEvents = append(childEvents, event)
		}
	}
	if len(childEvents) != 3 {
		t.Fatalf("child history events = %d, want 3: %+v", len(childEvents), childEvents)
	}
	wantTimes := []time.Time{startAt, stopAt, restartAt}
	for i, event := range childEvents {
		if !event.Ts.Equal(wantTimes[i]) || event.ParentThreadID != "root" || event.Source != agentgraph.SourceCodexAppServer {
			t.Fatalf("child event[%d] = %+v, want ts=%v parent=root source=codex_app_server", i, event, wantTimes[i])
		}
	}
}

func TestCodexChildHooksReplayStartAndStopAfterTopologyArrives(t *testing.T) {
	historyDir := t.TempDir()
	sink := history.NewSink(history.Config{Enabled: true, Detail: history.DetailFull, Dir: historyDir})
	coordinator, store, ref, base := newCodexChildHookCoordinator(t, sink)
	startAt, stopAt := base.Add(time.Second), base.Add(2*time.Second)
	sendCodexChildHook(coordinator, store, "SubagentStart", "root", "child", startAt)
	sendCodexChildHook(coordinator, store, "SubagentStop", "root", "child", stopAt)
	coordinator.reconcileCodexChildHooks(ref, stopAt)
	if store.Snapshot().Sessions[0].AgentGraph != nil {
		t.Fatal("hook-only evidence synthesized a child graph")
	}

	topologyAt := base.Add(3 * time.Second)
	applyCodexChildTopology(t, coordinator, ref, codexChildTopology(ref, topologyAt, true, agentgraph.Node{
		ID: "child", ParentID: "root", Runtime: agentgraph.RuntimeNotLoaded,
		Attention: agentgraph.AttentionNone, Lifecycle: agentgraph.LifecycleUnknown, UpdatedAt: topologyAt,
	}), topologyAt)
	coordinator.reconcileCodexChildHooks(ref, topologyAt)
	child := childNode(t, codexGraph(t, store), "child")
	if child.Runtime != agentgraph.RuntimeIdle || child.Lifecycle != agentgraph.LifecycleCompleted || !child.CompletedAt.Equal(stopAt) {
		t.Fatalf("replayed child = %#v", child)
	}

	sink.Close()
	events, err := history.ReadRange(historyDir, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var transitions []history.Event
	for _, event := range events {
		if event.Type == history.EventAgentState && event.ThreadID == "child" &&
			event.FromLifecycle != event.ToLifecycle {
			transitions = append(transitions, event)
		}
	}
	if len(transitions) != 2 || !transitions[0].Ts.Equal(startAt) || !transitions[1].Ts.Equal(stopAt) {
		t.Fatalf("queued transition history = %+v", transitions)
	}
}

func TestCodexChildHookOrderingIsIndependentAcrossRootAndSiblings(t *testing.T) {
	coordinator, store, ref, base := newCodexChildHookCoordinator(t, nil)
	topology := codexChildTopology(ref, base, true,
		agentgraph.Node{ID: "child-a", ParentID: "root", Runtime: agentgraph.RuntimeNotLoaded, Lifecycle: agentgraph.LifecycleUnknown, UpdatedAt: base},
		agentgraph.Node{ID: "child-b", ParentID: "root", Runtime: agentgraph.RuntimeNotLoaded, Lifecycle: agentgraph.LifecycleUnknown, UpdatedAt: base},
	)
	applyCodexChildTopology(t, coordinator, ref, topology, base)

	rootAt := base.Add(20 * time.Second)
	sendCodexHook(coordinator, store, rpc.Request{Event: "Stop", SessionID: "root", ObservedAt: rootAt})
	providerAfterRoot := topology.Clone()
	providerAfterRoot.ObservedAt = rootAt.Add(time.Second)
	providerAfterRoot.FreshUntil = providerAfterRoot.ObservedAt.Add(time.Hour)
	applyCodexChildTopology(t, coordinator, ref, providerAfterRoot, providerAfterRoot.ObservedAt)

	// This sibling edge is older than the unrelated root hook but newer than its
	// own provider transition, so it must still apply.
	siblingAt := base.Add(5 * time.Second)
	sendCodexChildHook(coordinator, store, "SubagentStart", "root", "child-b", siblingAt)
	coordinator.reconcileCodexChildHooks(ref, providerAfterRoot.ObservedAt)
	if got := childNode(t, codexGraph(t, store), "child-b").Lifecycle; got != agentgraph.LifecycleRunning {
		t.Fatalf("older sibling lifecycle = %q, want running", got)
	}

	startAt, stopAt := base.Add(10*time.Second), base.Add(12*time.Second)
	sendCodexChildHook(coordinator, store, "SubagentStart", "root", "child-a", startAt)
	sendCodexChildHook(coordinator, store, "SubagentStop", "root", "child-a", stopAt)
	coordinator.reconcileCodexChildHooks(ref, providerAfterRoot.ObservedAt)
	// A late delivery whose event time precedes the applied stop cannot reopen A.
	sendCodexChildHook(coordinator, store, "SubagentStart", "root", "child-a", base.Add(11*time.Second))
	coordinator.reconcileCodexChildHooks(ref, providerAfterRoot.ObservedAt)
	if got := childNode(t, codexGraph(t, store), "child-a").Lifecycle; got != agentgraph.LifecycleCompleted {
		t.Fatalf("stale child hook reopened lifecycle to %q", got)
	}
}

func TestCodexChildHookRequiresFreshExactNonRootTopology(t *testing.T) {
	t.Run("missing id", func(t *testing.T) {
		coordinator, store, _, base := newCodexChildHookCoordinator(t, nil)
		sendCodexChildHook(coordinator, store, "SubagentStart", "root", "", base)
		if diagnosticCount(coordinator, "subagent_hook_missing_id") != 1 {
			t.Fatalf("diagnostics = %+v", coordinator.Diagnostics())
		}
	})

	t.Run("wrong root", func(t *testing.T) {
		coordinator, store, _, base := newCodexChildHookCoordinator(t, nil)
		sendCodexChildHook(coordinator, store, "SubagentStart", "other-root", "child", base)
		coordinator.codexHookMu.Lock()
		queued := len(coordinator.codexHookRoots[providerRootKey(store.Snapshot().Sessions[0])].childQueue)
		coordinator.codexHookMu.Unlock()
		if queued != 0 || diagnosticCount(coordinator, "stale_observation_rejected") != 1 {
			t.Fatalf("wrong-root queue=%d diagnostics=%+v", queued, coordinator.Diagnostics())
		}
	})

	t.Run("stale topology queues", func(t *testing.T) {
		coordinator, store, ref, base := newCodexChildHookCoordinator(t, nil)
		observation := codexChildTopology(ref, base, true, agentgraph.Node{
			ID: "child", ParentID: "root", Runtime: agentgraph.RuntimeNotLoaded, Lifecycle: agentgraph.LifecycleUnknown,
		})
		observation.FreshUntil = base.Add(time.Second)
		applyCodexChildTopology(t, coordinator, ref, observation, base)
		hookAt := base.Add(2 * time.Second)
		sendCodexChildHook(coordinator, store, "SubagentStart", "root", "child", hookAt)
		coordinator.reconcileCodexChildHooks(ref, hookAt)
		if got := childNode(t, codexGraph(t, store), "child").Lifecycle; got != agentgraph.LifecycleUnknown {
			t.Fatalf("stale topology accepted child hook: %q", got)
		}
	})

	t.Run("root id is unmatched", func(t *testing.T) {
		coordinator, store, ref, base := newCodexChildHookCoordinator(t, nil)
		applyCodexChildTopology(t, coordinator, ref, codexChildTopology(ref, base, true), base)
		sendCodexChildHook(coordinator, store, "SubagentStart", "root", "root", base.Add(time.Second))
		coordinator.reconcileCodexChildHooks(ref, base.Add(time.Second))
		if len(codexGraph(t, store).Nodes) != 1 || diagnosticCount(coordinator, "subagent_hook_unmatched_topology") != 1 {
			t.Fatalf("root-node hook diagnostics=%+v graph=%#v", coordinator.Diagnostics(), codexGraph(t, store))
		}
	})
}

func TestCodexChildHookPreservesNestedImmediateParent(t *testing.T) {
	coordinator, store, ref, base := newCodexChildHookCoordinator(t, nil)
	applyCodexChildTopology(t, coordinator, ref, codexChildTopology(ref, base, true,
		agentgraph.Node{ID: "parent", ParentID: "root", Runtime: agentgraph.RuntimeNotLoaded, Lifecycle: agentgraph.LifecycleUnknown},
		agentgraph.Node{ID: "nested", ParentID: "parent", Runtime: agentgraph.RuntimeNotLoaded, Lifecycle: agentgraph.LifecycleUnknown},
	), base)
	sendCodexChildHook(coordinator, store, "SubagentStart", "root", "nested", base.Add(time.Second))
	coordinator.reconcileCodexChildHooks(ref, base.Add(time.Second))
	if got := childNode(t, codexGraph(t, store), "nested").ParentID; got != "parent" {
		t.Fatalf("nested parent = %q, want parent", got)
	}
}

func TestCodexRunningChildOverlayExpiresWithoutInventingCompletion(t *testing.T) {
	coordinator, store, ref, base := newCodexChildHookCoordinator(t, nil)
	observation := codexChildTopology(ref, base, true, agentgraph.Node{
		ID: "child", ParentID: "root", Runtime: agentgraph.RuntimeNotLoaded,
		Attention: agentgraph.AttentionNone, Lifecycle: agentgraph.LifecycleUnknown, UpdatedAt: base,
	})
	observation.FreshUntil = base.Add(2 * codexChildHookRetention)
	applyCodexChildTopology(t, coordinator, ref, observation, base)
	startAt := base.Add(time.Second)
	sendCodexChildHook(coordinator, store, "SubagentStart", "root", "child", startAt)
	coordinator.reconcileCodexChildHooks(ref, startAt)

	expiresAt := startAt.Add(codexChildHookRetention)
	coordinator.reconcileCodexChildHooks(ref, expiresAt)
	graph := codexGraph(t, store)
	child := childNode(t, graph, "child")
	if child.Runtime != agentgraph.RuntimeNotLoaded || child.Lifecycle != agentgraph.LifecycleUnknown ||
		!child.CompletedAt.IsZero() || graph.Summary.LiveChildren != 0 {
		t.Fatalf("expired running overlay = %#v summary=%#v", child, graph.Summary)
	}
	if diagnosticCount(coordinator, "subagent_hook_expired") != 1 {
		t.Fatalf("expiry diagnostics = %+v", coordinator.Diagnostics())
	}
}

func TestCodexNewerProviderStateSupersedesChildHookOverlay(t *testing.T) {
	coordinator, store, ref, base := newCodexChildHookCoordinator(t, nil)
	initial := codexChildTopology(ref, base, true, agentgraph.Node{
		ID: "child", ParentID: "root", Runtime: agentgraph.RuntimeNotLoaded,
		Lifecycle: agentgraph.LifecycleUnknown, UpdatedAt: base,
	})
	applyCodexChildTopology(t, coordinator, ref, initial, base)
	startAt := base.Add(time.Second)
	sendCodexChildHook(coordinator, store, "SubagentStart", "root", "child", startAt)
	coordinator.reconcileCodexChildHooks(ref, startAt)

	providerAt := base.Add(2 * time.Second)
	provider := codexChildTopology(ref, providerAt, true, agentgraph.Node{
		ID: "child", ParentID: "root", Runtime: agentgraph.RuntimeIdle,
		Lifecycle: agentgraph.LifecycleCompleted, UpdatedAt: providerAt, CompletedAt: providerAt,
	})
	applyCodexChildTopology(t, coordinator, ref, provider, providerAt)
	child := childNode(t, codexGraph(t, store), "child")
	if child.Runtime != agentgraph.RuntimeIdle || child.Lifecycle != agentgraph.LifecycleCompleted ||
		!child.CompletedAt.Equal(providerAt) || codexGraph(t, store).Summary.LiveChildren != 0 {
		t.Fatalf("newer provider did not win: %#v", child)
	}
	if diagnosticCount(coordinator, "subagent_hook_provider_superseded") != 1 {
		t.Fatalf("provider diagnostics = %+v", coordinator.Diagnostics())
	}
}

func TestCodexCompleteOmissionProducesNotFoundAfterCompletedOverlay(t *testing.T) {
	historyDir := t.TempDir()
	sink := history.NewSink(history.Config{Enabled: true, Detail: history.DetailFull, Dir: historyDir})
	coordinator, store, ref, base := newCodexChildHookCoordinator(t, sink)
	applyCodexChildTopology(t, coordinator, ref, codexChildTopology(ref, base, true, agentgraph.Node{
		ID: "child", ParentID: "root", Runtime: agentgraph.RuntimeNotLoaded, Lifecycle: agentgraph.LifecycleUnknown,
	}), base)
	stopAt := base.Add(time.Second)
	sendCodexChildHook(coordinator, store, "SubagentStop", "root", "child", stopAt)
	coordinator.reconcileCodexChildHooks(ref, stopAt)
	omissionAt := base.Add(2 * time.Second)
	applyCodexChildTopology(t, coordinator, ref, codexChildTopology(ref, omissionAt, true), omissionAt)
	if len(codexGraph(t, store).Nodes) != 1 {
		t.Fatalf("complete omission retained public child: %#v", codexGraph(t, store))
	}

	sink.Close()
	events, err := history.ReadRange(historyDir, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	sawNotFound := false
	for _, event := range events {
		if event.Type == history.EventAgentState && event.ThreadID == "child" &&
			event.ToLifecycle == agentgraph.LifecycleNotFound && event.Ts.Equal(omissionAt) {
			sawNotFound = true
		}
	}
	if !sawNotFound {
		t.Fatalf("complete omission did not emit not_found: %+v", events)
	}
}

func TestCodexPartialAndHookOnlyEvidenceNeverSynthesizesChildren(t *testing.T) {
	historyDir := t.TempDir()
	sink := history.NewSink(history.Config{Enabled: true, Detail: history.DetailFull, Dir: historyDir})
	coordinator, store, ref, base := newCodexChildHookCoordinator(t, sink)
	sendCodexChildHook(coordinator, store, "SubagentStart", "root", "child", base)
	coordinator.reconcileCodexChildHooks(ref, base)
	if store.Snapshot().Sessions[0].AgentGraph != nil {
		t.Fatal("hook-only fallback created a graph")
	}
	partialAt := base.Add(time.Second)
	applyCodexChildTopology(t, coordinator, ref, codexChildTopology(ref, partialAt, false), partialAt)
	coordinator.reconcileCodexChildHooks(ref, partialAt)
	if graph := codexGraph(t, store); len(graph.Nodes) != 1 || graph.Summary.LiveChildren != 0 {
		t.Fatalf("partial topology synthesized child: %#v", graph)
	}
	sink.Close()
	events, err := history.ReadRange(historyDir, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == history.EventAgentState && event.ParentThreadID != "" {
			t.Fatalf("partial/hook-only fallback synthesized child history: %+v", event)
		}
	}
}

func TestCodexChildHookQueueIsBoundedAndUnmatchedEdgesExpire(t *testing.T) {
	coordinator, store, ref, base := newCodexChildHookCoordinator(t, nil)
	applyCodexChildTopology(t, coordinator, ref, codexChildTopology(ref, base, true), base)
	for i := range codexChildHookQueueLimit + 1 {
		sendCodexChildHook(coordinator, store, "SubagentStart", "root", "missing-child", base.Add(time.Duration(i)*time.Nanosecond))
	}
	coordinator.codexHookMu.Lock()
	queued := len(coordinator.codexHookRoots[ref.Key()].childQueue)
	coordinator.codexHookMu.Unlock()
	if queued != codexChildHookQueueLimit || diagnosticCount(coordinator, "subagent_hook_queue_full") != 1 {
		t.Fatalf("bounded queue=%d diagnostics=%+v", queued, coordinator.Diagnostics())
	}

	coordinator.reconcileCodexChildHooks(ref, base.Add(time.Second))
	if diagnosticCount(coordinator, "subagent_hook_unmatched_topology") != codexChildHookQueueLimit {
		t.Fatalf("unmatched diagnostics = %+v", coordinator.Diagnostics())
	}
	coordinator.reconcileCodexChildHooks(ref, base.Add(codexChildHookRetention+time.Second))
	coordinator.codexHookMu.Lock()
	queued = len(coordinator.codexHookRoots[ref.Key()].childQueue)
	coordinator.codexHookMu.Unlock()
	if queued != 0 || diagnosticCount(coordinator, "subagent_hook_expired") != codexChildHookQueueLimit {
		t.Fatalf("expired queue=%d diagnostics=%+v", queued, coordinator.Diagnostics())
	}
}

func TestCodexChildHookStateClearsOnRootRotationAndProcessCleanup(t *testing.T) {
	coordinator, store, ref, base := newCodexChildHookCoordinator(t, nil)
	sendCodexChildHook(coordinator, store, "SubagentStart", "root", "child", base)
	sendCodexHook(coordinator, store, rpc.Request{
		Event: "SessionStart", SessionID: "root-new", ObservedAt: base.Add(time.Second),
	})
	coordinator.codexHookMu.Lock()
	rotated := coordinator.codexHookRoots[ref.Key()]
	queueAfterRotation := len(rotated.childQueue)
	overlaysAfterRotation := len(rotated.childOverlays)
	coordinator.codexHookMu.Unlock()
	if rotated.sessionID != "root-new" || queueAfterRotation != 0 || overlaysAfterRotation != 0 {
		t.Fatalf("root rotation retained child hook state: %#v", rotated)
	}

	store.Apply(func(sessions map[int]*state.Session) { delete(sessions, ref.PID) })
	coordinator.refreshTrackedRoots()
	coordinator.codexHookMu.Lock()
	_, retained := coordinator.codexHookRoots[ref.Key()]
	coordinator.codexHookMu.Unlock()
	if retained {
		t.Fatal("process cleanup retained child hook state")
	}
}
