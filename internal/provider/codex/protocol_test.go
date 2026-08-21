package codex

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/provider"
)

const (
	fixtureRoot  = "01890f00-0000-7000-8000-000000000001"
	fixtureAtlas = "01890f00-0000-7000-8000-000000000002"
	fixtureNova  = "01890f00-0000-7000-8000-000000000003"
)

var fixtureNow = time.Unix(1700000030, 0).UTC()

func TestSchemaFixturesMapNestedRuntimeLifecycleAndAttention(t *testing.T) {
	observer, key := fixtureObserver(t)
	applyFixture(t, observer, "root-with-two-children.jsonl", -1)
	observation := observer.roots[key].observation
	if !observation.Complete || observation.RootID != fixtureRoot || len(observation.Nodes) != 3 {
		t.Fatalf("root fixture observation = %#v", observation)
	}
	assertNode(t, observation, fixtureRoot, "", "", "", agentgraph.RuntimeIdle, agentgraph.AttentionNone, agentgraph.LifecycleUnknown)
	assertNode(t, observation, fixtureAtlas, fixtureRoot, "Atlas", "researcher", agentgraph.RuntimeActive, agentgraph.AttentionNone, agentgraph.LifecycleRunning)
	assertNode(t, observation, fixtureNova, fixtureRoot, "Nova", "reviewer", agentgraph.RuntimeActive, agentgraph.AttentionNone, agentgraph.LifecycleRunning)
	if summary := agentgraph.Reduce(observation, agentgraph.Summary{}, fixtureNow); summary.LegacyStatus != agentgraph.LegacyDelegating {
		t.Fatalf("idle root with active children = %#v", summary)
	}

	applyFixture(t, observer, "child-waiting-approval.jsonl", 1)
	applyFixture(t, observer, "child-waiting-user-input.jsonl", 1)
	observation = observer.roots[key].observation
	assertNodeAttention(t, observation, fixtureAtlas, agentgraph.AttentionApproval)
	assertNodeAttention(t, observation, fixtureNova, agentgraph.AttentionUserInput)
	summary := agentgraph.Reduce(observation, agentgraph.Summary{}, fixtureNow)
	if summary.LegacyStatus != agentgraph.LegacyPermission || summary.ApprovalNodes != 1 || summary.UserInputNodes != 1 {
		t.Fatalf("simultaneous waits = %#v", summary)
	}

	// Apply the complete wait fixtures to clear both active flags, then drain.
	applyFixture(t, observer, "child-waiting-approval.jsonl", -1)
	applyFixture(t, observer, "child-waiting-user-input.jsonl", -1)
	applyFixture(t, observer, "drain-to-idle.jsonl", -1)
	observation = observer.roots[key].observation
	assertNode(t, observation, fixtureAtlas, fixtureRoot, "Atlas", "researcher", agentgraph.RuntimeIdle, agentgraph.AttentionNone, agentgraph.LifecycleShutdown)
	assertNode(t, observation, fixtureNova, fixtureRoot, "Nova", "reviewer", agentgraph.RuntimeIdle, agentgraph.AttentionNone, agentgraph.LifecycleShutdown)
	if summary := agentgraph.Reduce(observation, agentgraph.Summary{}, fixtureNow); summary.LegacyStatus != agentgraph.LegacyIdle || summary.LiveChildren != 0 {
		t.Fatalf("drain summary = %#v", summary)
	}
}

func TestThreadListFixtureUsesExplicitParentage(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "captures", "thread-list-descendants.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		AncestorQuery struct {
			Response rpcEnvelope `json:"response"`
		} `json:"ancestorQuery"`
	}
	if err := json.Unmarshal(body, &fixture); err != nil {
		t.Fatal(err)
	}
	var result threadListResult
	if err := json.Unmarshal(fixture.AncestorQuery.Response.Result, &result); err != nil {
		t.Fatal(err)
	}
	state := newGraphState(rpcThread{ID: fixtureRoot, Status: rpcStatus{Type: "idle"}}, result.Data, 32)
	observation, err := state.observation(fixtureNow, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, childID := range []string{fixtureAtlas, fixtureNova} {
		if node := findNode(observation, childID); node == nil || node.ParentID != fixtureRoot {
			t.Errorf("child %s explicit parent = %#v", childID, node)
		}
	}
}

func TestThreadDisplayNamePrefersExplicitNameAndDerivesStablePreviewSlug(t *testing.T) {
	cases := []struct {
		name     string
		explicit string
		preview  string
		want     string
	}{
		{
			name: "explicit Codex rename wins", explicit: "Release audit",
			preview: "Please investigate the failing release workflow.", want: "Release audit",
		},
		{
			name:    "conversational lead-in is removed",
			preview: "I'd like you to help me with Codex session naming. Currently the title is a UUID.",
			want:    "codex-session-naming",
		},
		{
			name:    "first useful sentence wins",
			preview: "Please help. Fix the authentication timeout in the API client.",
			want:    "fix-authentication-timeout-api-client",
		},
		{
			name:    "long content is bounded",
			preview: "Supercalifragilisticexpialidociousplusmore should never make the switchboard chip enormous",
			want:    "supercalifragilisticexpialidociousplusmo",
		},
		{name: "empty stays empty", preview: "Please help me with this.", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := threadDisplayName(tc.explicit, tc.preview); got != tc.want {
				t.Errorf("threadDisplayName(%q, %q) = %q, want %q", tc.explicit, tc.preview, got, tc.want)
			}
		})
	}
}

func TestRootThreadNameSnapshotAndUpdateRemainDisplayOnly(t *testing.T) {
	state := newGraphState(rpcThread{
		ID: "root", Name: "Initial title", Preview: "Investigate the session naming behavior.",
		Status: rpcStatus{Type: "idle"},
	}, nil, 32)
	observation, err := state.observation(fixtureNow, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	assertNode(t, observation, "root", "", "Initial title", "", agentgraph.RuntimeIdle, agentgraph.AttentionNone, agentgraph.LifecycleUnknown)

	if !state.setThreadName("root", "Short rename") {
		t.Fatal("root thread name update was not applied")
	}
	if got := state.nodes["root"].node.Nickname; got != "Short rename" {
		t.Fatalf("updated root nickname = %q, want Short rename", got)
	}
	if !state.setThreadName("root", "") {
		t.Fatal("root thread name clear was not applied")
	}
	if got := state.nodes["root"].node.Nickname; got != "investigate-session-naming-behavior" {
		t.Fatalf("cleared root nickname = %q, want preview fallback", got)
	}
	if state.nodes["root"].node.Runtime != agentgraph.RuntimeIdle || state.nodes["root"].node.Attention != agentgraph.AttentionNone {
		t.Fatal("display-name update changed status axes")
	}
}

func TestThreadNameUpdatedNotificationRefreshesRootMetadata(t *testing.T) {
	observer, key := fixtureObserver(t)
	root := observer.roots[key].graph.nodes[fixtureRoot]
	root.threadPreview = "Diagnose Codex title animation."
	root.node.Nickname = previewSlug(root.threadPreview)

	changed, unknown := observer.applyNotificationLocked(rpcNotification{
		Generation: 1,
		Method:     "thread/name/updated",
		Params: mustJSON(t, map[string]any{
			"threadId": fixtureRoot, "threadName": "motionless-session-name",
		}),
	})
	if unknown || len(changed) != 1 || changed[0] != key {
		t.Fatalf("name update result = changed %#v unknown %v", changed, unknown)
	}
	if got := findNode(observer.roots[key].observation, fixtureRoot); got == nil || got.Nickname != "motionless-session-name" {
		t.Fatalf("name update observation root = %#v", got)
	}

	changed, _ = observer.applyNotificationLocked(rpcNotification{
		Generation: 1,
		Method:     "thread/name/updated",
		Params: mustJSON(t, map[string]any{
			"threadId": fixtureRoot, "threadName": nil,
		}),
	})
	if len(changed) != 1 {
		t.Fatalf("name clear changed keys = %#v", changed)
	}
	if got := findNode(observer.roots[key].observation, fixtureRoot); got == nil || got.Nickname != "diagnose-codex-title-animation" {
		t.Fatalf("name clear observation root = %#v", got)
	}
}

func TestOrderingNestedParentageUnknownEnumsAndTurnPruning(t *testing.T) {
	root := rpcThread{ID: "root", Status: rpcStatus{Type: "idle"}}
	state := newGraphState(root, nil, 2)
	// Spawn reveals identity and parent before thread metadata. Status can then
	// safely update that placeholder without guessing ancestry.
	state.applyCollaboration("turn-1", rpcItem{
		Type: "collabAgentToolCall", SenderThreadID: "root", ReceiverThreadIDs: []string{"child"},
		AgentsStates: map[string]rpcAgentState{"child": {Status: "pendingInit"}},
	})
	state.applyStatus(state.nodes["child"], rpcStatus{Type: "futureRuntime", ActiveFlags: []string{"futureFlag"}})
	state.upsertThread(rpcThread{ID: "child", ParentThreadID: "root", AgentNickname: "Child", Status: rpcStatus{Type: "active"}}, false)
	state.applyCollaboration("child-turn", rpcItem{
		Type: "collabAgentToolCall", SenderThreadID: "child", ReceiverThreadIDs: []string{"grandchild"},
		AgentsStates: map[string]rpcAgentState{"grandchild": {Status: "futureLifecycle"}},
	})
	observation, err := state.observation(fixtureNow, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	grandchild := findNode(observation, "grandchild")
	if grandchild == nil || grandchild.ParentID != "child" || grandchild.Lifecycle != agentgraph.LifecycleUnknown {
		t.Fatalf("nested unknown child = %#v", grandchild)
	}
	if observation.Diagnostic == "" {
		t.Fatal("unknown enums did not produce a content-free diagnostic")
	}

	state.nodes["child"].node.Lifecycle = agentgraph.LifecycleCompleted
	state.nodes["child"].cohort = "turn-1"
	state.nodes["grandchild"].node.Lifecycle = agentgraph.LifecycleRunning
	state.beginRootTurn("turn-2")
	if state.nodes["child"] == nil {
		t.Fatal("terminal ancestor required by a live descendant was pruned")
	}
	if state.nodes["grandchild"] == nil {
		t.Fatal("live descendant was pruned with its terminal parent")
	}
	if _, err := state.observation(fixtureNow, time.Minute); err != nil {
		t.Fatalf("live descendant ancestry became invalid: %v", err)
	}
	state.nodes["grandchild"].node.Lifecycle = agentgraph.LifecycleCompleted
	state.beginRootTurn("turn-3")
	if state.nodes["child"] != nil || state.nodes["grandchild"] != nil {
		t.Fatal("older terminal cohort survived after all descendants drained")
	}
}

func TestStatusBeforeMetadataIsGenerationScopedAndAppliedAfterParentage(t *testing.T) {
	observer, key := fixtureObserver(t)
	observer.pendingStatuses = make(map[string]rpcStatus)
	observer.applyNotificationLocked(rpcNotification{Generation: 1, Method: "thread/status/changed", Params: mustJSON(t, map[string]any{
		"threadId": "late-child", "status": map[string]any{"type": "active", "activeFlags": []string{"waitingOnUserInput"}},
	})})
	if len(observer.pendingStatuses) != 1 {
		t.Fatalf("pre-metadata status was not retained: %#v", observer.pendingStatuses)
	}
	observer.applyNotificationLocked(rpcNotification{Generation: 1, Method: "thread/started", Params: mustJSON(t, map[string]any{
		"thread": map[string]any{"id": "late-child", "parentThreadId": fixtureRoot, "agentNickname": "Late"},
	})})
	observation := observer.roots[key].observation
	assertNode(t, observation, "late-child", fixtureRoot, "Late", "", agentgraph.RuntimeActive, agentgraph.AttentionUserInput, agentgraph.LifecycleUnknown)
	if len(observer.pendingStatuses) != 0 {
		t.Fatalf("applied pending status was retained: %#v", observer.pendingStatuses)
	}
}

func TestMissingParentIsRejectedAndUnknownDiagnosticIsRateLimited(t *testing.T) {
	state := newGraphState(
		rpcThread{ID: "root", Status: rpcStatus{Type: "idle"}},
		[]rpcThread{{ID: "orphan", Status: rpcStatus{Type: "active"}}},
		32,
	)
	if _, err := state.observation(fixtureNow, time.Minute); err == nil {
		t.Fatal("child without explicit parent was accepted")
	}

	observer, _ := fixtureObserver(t)
	diagnostics := 0
	observer.config.Diagnostic = func(message string) {
		if message == "" {
			t.Fatal("empty diagnostic")
		}
		diagnostics++
	}
	params := mustJSON(t, map[string]any{"threadId": fixtureRoot, "status": map[string]any{"type": "futureRuntime"}})
	observer.handleNotification(rpcNotification{Generation: 1, Method: "thread/status/changed", Params: params})
	observer.handleNotification(rpcNotification{Generation: 1, Method: "thread/status/changed", Params: params})
	if diagnostics != 1 {
		t.Fatalf("unknown enum diagnostics = %d, want one per rate-limit window", diagnostics)
	}
}

func TestObservationCloneIsImmutable(t *testing.T) {
	state := newGraphState(
		rpcThread{ID: "root", Status: rpcStatus{Type: "idle"}},
		[]rpcThread{{ID: "child", ParentThreadID: "root", AgentNickname: "original", Status: rpcStatus{Type: "active"}}},
		32,
	)
	observation, err := state.observation(fixtureNow, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	clone := observation.Clone()
	clone.Nodes[1].Nickname = "mutated"
	if observation.Nodes[1].Nickname != "original" {
		t.Fatal("observation clone shares mutable node storage")
	}
}

func TestEveryCodexEnumMapsToNeutralAxisAndTerminalCapIsBounded(t *testing.T) {
	for input, want := range map[string]agentgraph.RuntimeState{
		"notLoaded":   agentgraph.RuntimeNotLoaded,
		"idle":        agentgraph.RuntimeIdle,
		"active":      agentgraph.RuntimeActive,
		"systemError": agentgraph.RuntimeSystemError,
		"future":      agentgraph.RuntimeUnknown,
	} {
		if got := mapRuntime(input); got != want {
			t.Errorf("mapRuntime(%q) = %q, want %q", input, got, want)
		}
	}
	for input, want := range map[string]agentgraph.LifecycleState{
		"pendingInit": agentgraph.LifecyclePending,
		"running":     agentgraph.LifecycleRunning,
		"completed":   agentgraph.LifecycleCompleted,
		"interrupted": agentgraph.LifecycleInterrupted,
		"errored":     agentgraph.LifecycleErrored,
		"shutdown":    agentgraph.LifecycleShutdown,
		"notFound":    agentgraph.LifecycleNotFound,
		"future":      agentgraph.LifecycleUnknown,
	} {
		if got := mapLifecycle(input); got != want {
			t.Errorf("mapLifecycle(%q) = %q, want %q", input, got, want)
		}
	}

	state := newGraphState(rpcThread{ID: "root", Status: rpcStatus{Type: "idle"}}, []rpcThread{
		{ID: "old", ParentThreadID: "root", UpdatedAt: 1},
		{ID: "new", ParentThreadID: "root", UpdatedAt: 2},
		{ID: "live", ParentThreadID: "root", UpdatedAt: 3},
	}, 1)
	state.nodes["old"].node.Lifecycle = agentgraph.LifecycleCompleted
	state.nodes["new"].node.Lifecycle = agentgraph.LifecycleCompleted
	state.nodes["live"].node.Lifecycle = agentgraph.LifecycleRunning
	state.boundTerminals(1)
	if state.nodes["old"] != nil || state.nodes["new"] == nil || state.nodes["live"] == nil {
		t.Fatalf("terminal cap retained wrong cohort: %#v", state.nodes)
	}
	state.ensureNode("nested-live", "new").node.Lifecycle = agentgraph.LifecycleRunning
	state.boundTerminals(0)
	if state.nodes["new"] == nil || state.nodes["nested-live"] == nil {
		t.Fatal("terminal cap removed ancestry required by a live node")
	}
}

func fixtureObserver(t *testing.T) (*Observer, provider.RootKey) {
	t.Helper()
	key := provider.RootKey{PID: 42, StartedAt: time.Unix(1, 0)}
	state := newGraphState(rpcThread{ID: fixtureRoot, Status: rpcStatus{Type: "idle"}}, nil, 32)
	observation, err := state.observation(fixtureNow, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	observer := &Observer{
		config: Config{Freshness: time.Minute, RecentTerminalLimit: 32, Now: func() time.Time { return fixtureNow }},
		queue:  provider.NewInvalidationQueue(64), roots: map[provider.RootKey]*rootRecord{
			key: {threadID: fixtureRoot, graph: state, observation: observation, generation: 1},
		},
		generation: 1, connected: true, pendingStatuses: make(map[string]rpcStatus),
	}
	return observer, key
}

func applyFixture(t *testing.T, observer *Observer, name string, limit int) {
	t.Helper()
	file, err := os.Open(filepath.Join("testdata", "captures", name))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	count := 0
	for scanner.Scan() {
		var envelope rpcEnvelope
		if err := json.Unmarshal(scanner.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Method == "" || envelope.Method == "thread/read" || envelope.Method == "thread/list" || envelope.Method == "initialize" {
			continue
		}
		observer.applyNotificationLocked(rpcNotification{Generation: 1, Method: envelope.Method, Params: envelope.Params})
		count++
		if limit >= 0 && count >= limit {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
}

func assertNode(t *testing.T, observation agentgraph.Observation, id, parent, nickname, role string, runtime agentgraph.RuntimeState, attention agentgraph.AttentionState, lifecycle agentgraph.LifecycleState) {
	t.Helper()
	node := findNode(observation, id)
	if node == nil {
		t.Fatalf("node %s missing from %#v", id, observation.Nodes)
	}
	if node.ParentID != parent || node.Nickname != nickname || node.Role != role || node.Runtime != runtime || node.Attention != attention || node.Lifecycle != lifecycle {
		t.Errorf("node %s = %#v", id, *node)
	}
}

func assertNodeAttention(t *testing.T, observation agentgraph.Observation, id string, want agentgraph.AttentionState) {
	t.Helper()
	if node := findNode(observation, id); node == nil || node.Attention != want {
		t.Fatalf("node %s attention = %#v, want %s", id, node, want)
	}
}

func findNode(observation agentgraph.Observation, id string) *agentgraph.Node {
	for i := range observation.Nodes {
		if observation.Nodes[i].ID == id {
			return &observation.Nodes[i]
		}
	}
	return nil
}
