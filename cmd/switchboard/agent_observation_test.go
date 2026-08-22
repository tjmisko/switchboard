package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/provider"
	claudeprovider "github.com/tjmisko/switchboard/internal/provider/claude"
	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/state"
)

type fakeCodexCoordinatorObserver struct {
	mu sync.Mutex

	observations map[provider.RootKey]agentgraph.Observation
	updates      chan provider.RootKey
	started      chan struct{}
	release      chan struct{}
	startsOnce   sync.Once
	bindings     map[provider.RootKey]string
	forgets      map[provider.RootKey]int
	closes       int
	observes     int
	nameCalls    chan codexNameCall
	nameErrors   chan error
}

type codexNameCall struct {
	key        provider.RootKey
	threadID   string
	generation uint64
	name       string
}

func newFakeCodexCoordinatorObserver() *fakeCodexCoordinatorObserver {
	return &fakeCodexCoordinatorObserver{
		observations: make(map[provider.RootKey]agentgraph.Observation), updates: make(chan provider.RootKey, 64),
		bindings: make(map[provider.RootKey]string), forgets: make(map[provider.RootKey]int),
		nameCalls: make(chan codexNameCall, 16),
	}
}

func (f *fakeCodexCoordinatorObserver) Observe(ctx context.Context, ref provider.RootRef, _ time.Time) (agentgraph.Observation, error) {
	f.mu.Lock()
	f.observes++
	started, release := f.started, f.release
	observation := f.observations[ref.Key()].Clone()
	f.mu.Unlock()
	if started != nil {
		f.startsOnce.Do(func() { close(started) })
	}
	if release != nil {
		select {
		case <-ctx.Done():
			return agentgraph.Observation{}, ctx.Err()
		case <-release:
		}
	}
	return observation, nil
}

func (f *fakeCodexCoordinatorObserver) Updates() <-chan provider.RootKey { return f.updates }
func (f *fakeCodexCoordinatorObserver) Forget(key provider.RootKey) {
	f.mu.Lock()
	f.forgets[key]++
	f.mu.Unlock()
}
func (f *fakeCodexCoordinatorObserver) Close() error {
	f.mu.Lock()
	f.closes++
	f.mu.Unlock()
	return nil
}
func (f *fakeCodexCoordinatorObserver) RegisterHookBinding(key provider.RootKey, id string) error {
	f.mu.Lock()
	f.bindings[key] = id
	f.mu.Unlock()
	return nil
}

func (f *fakeCodexCoordinatorObserver) SetThreadName(_ context.Context, key provider.RootKey, threadID string, generation uint64, name string) error {
	f.nameCalls <- codexNameCall{key: key, threadID: threadID, generation: generation, name: name}
	if f.nameErrors != nil {
		return <-f.nameErrors
	}
	return nil
}

func (f *fakeCodexCoordinatorObserver) binding(key provider.RootKey) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bindings[key]
}

func testCodexObservation(ref provider.RootRef, rootID string, observed time.Time, runtime agentgraph.RuntimeState, attention agentgraph.AttentionState) agentgraph.Observation {
	return agentgraph.Observation{
		Provider: agentgraph.ProviderCodex, RootID: rootID, Source: agentgraph.SourceCodexAppServer,
		ObservedAt: observed, FreshUntil: observed.Add(time.Minute), Complete: true,
		Nodes: []agentgraph.Node{{
			ID: rootID, Runtime: runtime, Attention: attention,
			Lifecycle: agentgraph.LifecycleRunning, StartedAt: ref.StartedAt, UpdatedAt: observed,
		}},
	}
}

func seedCoordinatorSession(store *state.Store, pid int, started time.Time, agent, sessionID, cwd string) provider.RootRef {
	store.Apply(func(sessions map[int]*state.Session) {
		sess := &state.Session{PID: pid, StartedAt: started, Agent: agent, CWD: cwd}
		if sessionID != "" {
			sess.AgentBlock(agent).SessionID = sessionID
		}
		sessions[pid] = sess
	})
	ref, _ := providerRootRef(store.Snapshot().Sessions[0])
	return ref
}

func TestCodexObserverModeValidationAndConstruction(t *testing.T) {
	if defaultCodexObserverMode != codexObserverAuto {
		t.Fatalf("default mode = %q, want auto", defaultCodexObserverMode)
	}
	for _, value := range []string{"auto", "off"} {
		mode, err := parseCodexObserverMode(value)
		if err != nil || string(mode) != value {
			t.Fatalf("parseCodexObserverMode(%q) = %q, %v", value, mode, err)
		}
	}
	for _, value := range []string{"", "on", "AUTO"} {
		if _, err := parseCodexObserverMode(value); err == nil {
			t.Fatalf("parseCodexObserverMode(%q) accepted invalid mode", value)
		}
	}

	fake := newFakeCodexCoordinatorObserver()
	constructed := 0
	factory := func() codexObserver {
		constructed++
		return fake
	}
	if got := codexObserverForMode(codexObserverOff, factory); got != nil {
		t.Fatalf("off observer = %T, want nil", got)
	}
	if constructed != 0 {
		t.Fatalf("off constructed observer %d times", constructed)
	}
	if got := codexObserverForMode(codexObserverAuto, factory); got != fake {
		t.Fatalf("auto observer = %T, want factory result", got)
	}
	if constructed != 1 {
		t.Fatalf("auto constructed observer %d times, want 1", constructed)
	}
	if got := codexObserverModeCategory(codexObserverAuto); got != "observer_enabled" {
		t.Fatalf("auto category = %q", got)
	}
	if got := codexObserverModeCategory(codexObserverOff); got != "observer_disabled" {
		t.Fatalf("off category = %q", got)
	}
}

func TestCodexObserverOffKeepsHookFallbackAndClaudeGraph(t *testing.T) {
	store := state.New("")
	started := time.Now().Add(-time.Hour)
	codexRef := provider.RootRef{PID: 4051, StartedAt: started, Provider: agentgraph.ProviderCodex, CWD: "/codex"}
	claudeRef := provider.RootRef{
		PID: 4052, StartedAt: started.Add(time.Second), Provider: agentgraph.ProviderClaude,
		ProviderSessionID: "claude-root", CWD: "/claude",
	}
	store.Apply(func(sessions map[int]*state.Session) {
		sessions[codexRef.PID] = &state.Session{
			PID: codexRef.PID, StartedAt: codexRef.StartedAt,
			Agent: state.AgentKindCodex, CWD: codexRef.CWD,
		}
		sessions[claudeRef.PID] = &state.Session{
			PID: claudeRef.PID, StartedAt: claudeRef.StartedAt,
			Agent: state.AgentKindClaude, CWD: claudeRef.CWD,
			Claude: &state.AgentInfo{SessionID: claudeRef.ProviderSessionID},
		}
	})
	realClaude := claudeprovider.NewObserver(t.TempDir())
	coordinator := newAgentCoordinator(store, nil, realClaude, nil)
	coordinator.refreshTrackedRoots()
	defer coordinator.Close()

	codexSession, ok := sessionForKey(store.Snapshot(), codexRef.Key())
	if !ok {
		t.Fatal("Codex root discovery was lost in off mode")
	}
	coordinator.HandleHook(rpc.Request{
		Agent: state.AgentKindCodex, Event: "SessionStart", SessionID: "codex-root",
	}, codexSession)
	codexSession, _ = sessionForKey(store.Snapshot(), codexRef.Key())
	if codexSession.AgentGraph == nil || codexSession.AgentGraph.Source != agentgraph.SourceHook ||
		codexSession.AgentGraph.Summary.Status != state.StatusIdle {
		t.Fatalf("off-mode Codex hook graph = %#v", codexSession.AgentGraph)
	}
	if codexSession.Codex == nil || codexSession.Codex.SessionID != "codex-root" || codexSession.Codex.Status != state.StatusIdle {
		t.Fatalf("off-mode Codex legacy projection = %#v", codexSession.Codex)
	}

	claudeSession, ok := sessionForKey(store.Snapshot(), claudeRef.Key())
	if !ok {
		t.Fatal("Claude root discovery was lost in Codex off mode")
	}
	coordinator.HandleHook(rpc.Request{
		Agent: state.AgentKindClaude, Event: "PermissionRequest", SessionID: "claude-root", ToolName: "Bash",
	}, claudeSession)
	claudeSession, _ = sessionForKey(store.Snapshot(), claudeRef.Key())
	if claudeSession.AgentGraph == nil || claudeSession.AgentGraph.Summary.Status != state.StatusPermission {
		t.Fatalf("Claude graph while Codex observer is off = %#v", claudeSession.AgentGraph)
	}
}

func TestCodexLaterLifecycleHookSelfHealsExactBindingAfterSessionStartRace(t *testing.T) {
	store := state.New("")
	ref := seedCoordinatorSession(store, 4053, time.Now().Add(-time.Hour), state.AgentKindCodex, "", "/codex")
	fake := newFakeCodexCoordinatorObserver()
	coordinator := newAgentCoordinator(store, nil, nil, fake)
	coordinator.refreshTrackedRoots()
	defer coordinator.Close()

	sess, ok := sessionForKey(store.Snapshot(), ref.Key())
	if !ok {
		t.Fatal("Codex root discovery was lost before the later hook")
	}
	coordinator.HandleHook(rpc.Request{
		Agent: state.AgentKindCodex, Event: "PermissionRequest",
		SessionID: "codex-root", ToolName: "Bash",
	}, sess)

	if got := fake.binding(ref.Key()); got != "codex-root" {
		t.Fatalf("later exact hook binding = %q, want codex-root", got)
	}
	sess, _ = sessionForKey(store.Snapshot(), ref.Key())
	if sess.AgentGraph == nil || sess.AgentGraph.Source != agentgraph.SourceHook ||
		sess.AgentGraph.Summary.Status != state.StatusPermission {
		t.Fatalf("later Codex hook fallback graph = %#v", sess.AgentGraph)
	}
}

func TestCodexPersistedExactIdentityRestoresObserverBindingAfterDaemonRestart(t *testing.T) {
	store := state.New("")
	ref := seedCoordinatorSession(store, 4054, time.Now().Add(-time.Hour), state.AgentKindCodex, "codex-root", "/codex")
	fake := newFakeCodexCoordinatorObserver()
	coordinator := newAgentCoordinator(store, nil, nil, fake)
	coordinator.refreshTrackedRoots()
	defer coordinator.Close()

	if got := fake.binding(ref.Key()); got != "codex-root" {
		t.Fatalf("restored exact hook binding = %q, want codex-root", got)
	}
}

func TestCodexHookFallbackFreshnessIsBoundedByState(t *testing.T) {
	now := time.Date(2026, 8, 21, 16, 0, 0, 0, time.UTC)
	startedAt := now.Add(-time.Hour)
	tests := []struct {
		name  string
		event string
		tool  string
		want  time.Duration
	}{
		{name: "active", event: "UserPromptSubmit", want: codexHookActiveFreshness},
		{name: "approval", event: "PermissionRequest", tool: "Bash", want: codexHookAttentionFreshness},
		{name: "user input", event: "PermissionRequest", tool: "AskUserQuestion", want: codexHookAttentionFreshness},
		{name: "idle", event: "Stop", want: codexHookIdleFreshness},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			observation, mapped := codexHookObservation("codex-root", tt.event, tt.tool, startedAt, now)
			if !mapped {
				t.Fatalf("event %q did not map", tt.event)
			}
			if got := observation.FreshUntil.Sub(now); got != tt.want {
				t.Fatalf("freshness = %v, want %v", got, tt.want)
			}
			if !observation.Fresh(observation.FreshUntil.Add(-time.Nanosecond)) {
				t.Fatal("observation expired before its state-specific deadline")
			}
			if observation.Fresh(observation.FreshUntil) {
				t.Fatal("observation remained fresh at the half-open deadline")
			}
		})
	}
}

func TestAgentObservationDoesNotHoldStoreLockAndFencesLateGeneration(t *testing.T) {
	startedAt := time.Now().Add(-time.Hour)
	store := state.New("")
	ref := seedCoordinatorSession(store, 4101, startedAt, state.AgentKindCodex, "root", "/same")
	fake := newFakeCodexCoordinatorObserver()
	fake.started, fake.release = make(chan struct{}), make(chan struct{})
	oldAt := time.Now()
	fake.observations[ref.Key()] = testCodexObservation(ref, "root", oldAt, agentgraph.RuntimeActive, agentgraph.AttentionNone)
	coordinator := newAgentCoordinator(store, nil, nil, fake)
	coordinator.refreshTrackedRoots()

	done := make(chan struct{})
	go func() {
		coordinator.observe(context.Background(), ref)
		close(done)
	}()
	select {
	case <-fake.started:
	case <-time.After(time.Second):
		t.Fatal("blocking observer did not start")
	}

	// If Observe ran under Store.Apply this read would block until release.
	readDone := make(chan struct{})
	go func() { store.Snapshot(); close(readDone) }()
	select {
	case <-readDone:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("store read blocked behind provider I/O")
	}

	// A newer hook result lands while the old periodic observation is blocked.
	coordinator.HandleHook(rpc.Request{Agent: state.AgentKindCodex, Event: "SessionStart", SessionID: "root"}, store.Snapshot().Sessions[0])
	close(fake.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("old observation did not finish")
	}

	graph := store.Snapshot().Sessions[0].AgentGraph
	if graph == nil || graph.Source != agentgraph.SourceHook || graph.Summary.Status != state.StatusIdle {
		t.Fatalf("late periodic observation overwrote newer hook: %#v", graph)
	}
}

func TestNewerCodexHookIsImmediateAndLaterAppServerCorrectsIt(t *testing.T) {
	startedAt := time.Now().Add(-time.Hour)
	store := state.New("")
	ref := seedCoordinatorSession(store, 4102, startedAt, state.AgentKindCodex, "root", "/same")
	fake := newFakeCodexCoordinatorObserver()
	coordinator := newAgentCoordinator(store, nil, nil, fake)
	coordinator.refreshTrackedRoots()
	now := time.Now().Add(-time.Second)
	observation := testCodexObservation(ref, "root", now, agentgraph.RuntimeIdle, agentgraph.AttentionApproval)
	observation.Nodes[0].Nickname = "kept-name"
	observation.Nodes = append(observation.Nodes, agentgraph.Node{
		ID: "child", ParentID: "root", Runtime: agentgraph.RuntimeIdle,
		Lifecycle: agentgraph.LifecycleCompleted, UpdatedAt: now,
	})
	generation := coordinator.begin(ref.Key())
	if !coordinator.applyObservation(ref, generation, observation, claudeprovider.Compatibility{}, now) {
		t.Fatal("fresh app-server observation was not applied")
	}

	hookAt := now.Add(500 * time.Millisecond)
	coordinator.HandleHook(rpc.Request{Agent: state.AgentKindCodex, Event: "PostToolUse", ObservedAt: hookAt}, store.Snapshot().Sessions[0])
	graph := store.Snapshot().Sessions[0].AgentGraph
	if graph.Source != agentgraph.SourceHook || graph.Summary.Status != state.StatusWorking {
		t.Fatalf("newer hook did not provide its immediate transition: %#v", graph)
	}
	if len(graph.Nodes) != 2 || graph.Nodes[0].Nickname != "kept-name" {
		t.Fatalf("partial hook erased app-server structure: %#v", graph.Nodes)
	}

	corrected := testCodexObservation(ref, "root", hookAt.Add(time.Second), agentgraph.RuntimeIdle, agentgraph.AttentionApproval)
	if !coordinator.applyObservation(ref, coordinator.begin(ref.Key()), corrected, claudeprovider.Compatibility{}, corrected.ObservedAt) {
		t.Fatal("later app-server correction was not applied")
	}
	graph = store.Snapshot().Sessions[0].AgentGraph
	if graph.Source != agentgraph.SourceCodexAppServer || graph.Summary.Status != state.StatusPermission {
		t.Fatalf("later app-server snapshot did not correct hook state: %#v", graph)
	}
}

func TestExpiredObservationClearsAuthorityWithoutDroppingRoot(t *testing.T) {
	startedAt := time.Now().Add(-time.Hour)
	store := state.New("")
	ref := seedCoordinatorSession(store, 4103, startedAt, state.AgentKindCodex, "root", "/same")
	fake := newFakeCodexCoordinatorObserver()
	coordinator := newAgentCoordinator(store, nil, nil, fake)
	coordinator.refreshTrackedRoots()
	observed := time.Now().Add(-time.Second)
	observation := testCodexObservation(ref, "root", observed, agentgraph.RuntimeIdle, agentgraph.AttentionApproval)
	observation.FreshUntil = observed.Add(500 * time.Millisecond)
	generation := coordinator.begin(ref.Key())
	if !coordinator.applyObservation(ref, generation, observation, claudeprovider.Compatibility{}, observed.Add(time.Millisecond)) {
		t.Fatal("initial fresh observation was not applied")
	}
	coordinator.expireCurrent(ref, generation, time.Now())

	snap := store.Snapshot()
	if len(snap.Sessions) != 1 || snap.Sessions[0].AgentGraph == nil {
		t.Fatalf("expiration dropped root or graph detail: %+v", snap.Sessions)
	}
	if got := snap.Sessions[0].AgentGraph.Summary.Status; got != "" {
		t.Fatalf("expired legacy status = %q, want unknown", got)
	}
	if len(snap.Sessions[0].AgentGraph.Nodes) != 1 {
		t.Fatalf("expiration discarded bounded graph detail: %+v", snap.Sessions[0].AgentGraph.Nodes)
	}
}

func TestTwoSameCWDRootsKeepExactCodexGraphs(t *testing.T) {
	store := state.New("")
	started := time.Now().Add(-time.Hour)
	store.Apply(func(sessions map[int]*state.Session) {
		sessions[4201] = &state.Session{PID: 4201, StartedAt: started, Agent: state.AgentKindCodex, CWD: "/same"}
		sessions[4202] = &state.Session{PID: 4202, StartedAt: started.Add(time.Second), Agent: state.AgentKindCodex, CWD: "/same"}
	})
	fake := newFakeCodexCoordinatorObserver()
	coordinator := newAgentCoordinator(store, nil, nil, fake)
	refs := coordinator.refreshTrackedRoots()
	if len(refs) != 2 {
		t.Fatalf("got %d roots", len(refs))
	}
	now := time.Now()
	for i, ref := range refs {
		rootID := []string{"thread-a", "thread-b"}[i]
		observation := testCodexObservation(ref, rootID, now.Add(time.Duration(i)*time.Millisecond), agentgraph.RuntimeIdle, agentgraph.AttentionNone)
		if !coordinator.applyObservation(ref, coordinator.begin(ref.Key()), observation, claudeprovider.Compatibility{}, now.Add(time.Duration(i)*time.Millisecond)) {
			t.Fatalf("root %d was not applied", i)
		}
	}
	snap := store.Snapshot()
	if snap.Sessions[0].AgentGraph.RootID != "thread-a" || snap.Sessions[1].AgentGraph.RootID != "thread-b" {
		t.Fatalf("same-cwd roots crossed: %q %q", snap.Sessions[0].AgentGraph.RootID, snap.Sessions[1].AgentGraph.RootID)
	}
}

func TestUnboundCodexRootRemainsVisibleAndUnknown(t *testing.T) {
	store := state.New("")
	ref := seedCoordinatorSession(store, 4301, time.Now().Add(-time.Hour), state.AgentKindCodex, "", "/project")
	fake := newFakeCodexCoordinatorObserver()
	coordinator := newAgentCoordinator(store, nil, nil, fake)
	coordinator.refreshTrackedRoots()
	coordinator.observe(context.Background(), ref)
	snap := store.Snapshot()
	if len(snap.Sessions) != 1 || snap.Sessions[0].PID != ref.PID {
		t.Fatalf("unbound root was removed: %+v", snap.Sessions)
	}
	if snap.Sessions[0].AgentGraph != nil || snap.Sessions[0].Codex != nil {
		t.Fatalf("unbound root was heuristically enriched: %+v", snap.Sessions[0])
	}
}

func TestProviderLifecycleForgetsAndClosesExactlyOnce(t *testing.T) {
	store := state.New("")
	ref := seedCoordinatorSession(store, 4401, time.Now().Add(-time.Hour), state.AgentKindCodex, "root", "/project")
	fake := newFakeCodexCoordinatorObserver()
	coordinator := newAgentCoordinator(store, nil, nil, fake)
	coordinator.refreshTrackedRoots()
	store.Apply(func(sessions map[int]*state.Session) { delete(sessions, ref.PID) })
	coordinator.refreshTrackedRoots()
	coordinator.refreshTrackedRoots()
	coordinator.Close()
	coordinator.Close()

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.forgets[ref.Key()] != 1 {
		t.Fatalf("Forget calls = %d, want 1", fake.forgets[ref.Key()])
	}
	if fake.closes != 1 {
		t.Fatalf("Close calls = %d, want 1", fake.closes)
	}
}

func TestClaudeHookAgentIDIsNormalizedExactlyOnceByAdapter(t *testing.T) {
	store := state.New("")
	started := time.Now().Add(-time.Hour)
	seedCoordinatorSession(store, 4501, started, state.AgentKindClaude, "claude-root", "/project")
	realClaude := claudeprovider.NewObserver(t.TempDir())
	coordinator := newAgentCoordinator(store, nil, realClaude, nil)
	coordinator.refreshTrackedRoots()
	coordinator.HandleHook(rpc.Request{
		Agent: state.AgentKindClaude, Event: "PermissionRequest", SessionID: "claude-root",
		AgentID: "agent-agent-child", ToolName: "Bash",
	}, store.Snapshot().Sessions[0])

	graph := store.Snapshot().Sessions[0].AgentGraph
	if graph == nil || len(graph.Nodes) != 2 {
		t.Fatalf("hook graph = %#v", graph)
	}
	if got := graph.Nodes[1].ID; got != "agent-child" {
		t.Fatalf("child id = %q, want one prefix strip to agent-child", got)
	}
	coordinator.Close()
}

func TestClaudeShadowFixturesAuthorizeGraphSummary(t *testing.T) {
	for _, fixture := range claudeprovider.CanonicalShadowCases() {
		t.Run(fixture.Name, func(t *testing.T) {
			observer := claudeprovider.NewObserver(t.TempDir())
			root := provider.RootRef{
				PID: 4601, StartedAt: time.Now().Add(-time.Hour), Provider: agentgraph.ProviderClaude,
				ProviderSessionID: "shadow-" + fixture.Name, Transcript: "/not/read/by-hooks",
			}
			base := time.Now()
			var result claudeprovider.HookResult
			for i, hook := range fixture.Hooks {
				hook.Root = root
				hook.At = base.Add(time.Duration(i) * time.Millisecond)
				result = observer.ApplyHook(hook)
			}
			comparison := claudeprovider.CompareShadow(result.Projection.Status, result.Observation, agentgraph.Summary{}, base.Add(time.Duration(len(fixture.Hooks))*time.Millisecond))
			if !comparison.Match || comparison.GraphStatus != fixture.LegacyStatus ||
				comparison.LiveChildren != fixture.LiveChildren || comparison.WaitingNodes != fixture.WaitingNodes {
				t.Fatalf("shadow comparison = %+v, fixture = %+v", comparison, fixture)
			}
			_ = observer.Close()
		})
	}
}

func TestUpdateStormDoesNotStarvePeriodicReconciliation(t *testing.T) {
	store := state.New("")
	ref := seedCoordinatorSession(store, 4701, time.Now().Add(-time.Hour), state.AgentKindCodex, "root", "/project")
	fake := newFakeCodexCoordinatorObserver()
	now := time.Now()
	fake.observations[ref.Key()] = testCodexObservation(ref, "root", now, agentgraph.RuntimeIdle, agentgraph.AttentionNone)
	coordinator := newAgentCoordinator(store, nil, nil, fake)
	coordinator.Start(context.Background(), 10*time.Millisecond)
	for range 1000 {
		coordinator.Request(ref.Key())
	}
	time.Sleep(80 * time.Millisecond)
	coordinator.Close()

	fake.mu.Lock()
	observes := fake.observes
	fake.mu.Unlock()
	if observes < 2 {
		t.Fatalf("Observe calls = %d, want initial/event work plus a periodic backstop", observes)
	}
}
