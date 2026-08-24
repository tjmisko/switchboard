package agentgraph

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

var (
	testObserved = time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	testNow      = testObserved.Add(time.Second)
	testFresh    = testObserved.Add(time.Minute)
)

func TestReduceTruthTable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		root             Node
		children         []Node
		wantRuntime      RuntimeState
		wantAttention    AttentionState
		wantLegacy       string
		wantLiveChildren int
		wantWaiting      int
		wantApprovals    int
		wantUserInputs   int
		wantErrors       int
	}{
		{
			name:        "active root is working",
			root:        Node{Runtime: RuntimeActive},
			wantRuntime: RuntimeActive, wantAttention: AttentionNone,
			wantLegacy: LegacyWorking,
		},
		{
			name:        "idle root is idle",
			root:        Node{Runtime: RuntimeIdle},
			wantRuntime: RuntimeIdle, wantAttention: AttentionNone,
			wantLegacy: LegacyIdle,
		},
		{
			name:        "not loaded root without work is unknown",
			root:        Node{Runtime: RuntimeNotLoaded},
			wantRuntime: RuntimeNotLoaded, wantAttention: AttentionNone,
		},
		{
			name:        "unknown topology child is not live",
			root:        Node{Runtime: RuntimeIdle},
			children:    []Node{{ID: "child", Runtime: RuntimeUnknown, Lifecycle: LifecycleUnknown}},
			wantRuntime: RuntimeIdle, wantAttention: AttentionNone,
			wantLegacy: LegacyIdle,
		},
		{
			name:        "not loaded topology child is not live",
			root:        Node{Runtime: RuntimeIdle},
			children:    []Node{{ID: "child", Runtime: RuntimeNotLoaded, Lifecycle: LifecycleUnknown}},
			wantRuntime: RuntimeIdle, wantAttention: AttentionNone,
			wantLegacy: LegacyIdle,
		},
		{
			name:        "idle root plus running child delegates",
			root:        Node{Runtime: RuntimeIdle},
			children:    []Node{{ID: "child", Lifecycle: LifecycleRunning}},
			wantRuntime: RuntimeIdle, wantAttention: AttentionNone,
			wantLegacy: LegacyDelegating, wantLiveChildren: 1,
		},
		{
			name:        "unknown root plus pending child delegates",
			root:        Node{Runtime: RuntimeUnknown},
			children:    []Node{{ID: "child", Lifecycle: LifecyclePending}},
			wantRuntime: RuntimeUnknown, wantAttention: AttentionNone,
			wantLegacy: LegacyDelegating, wantLiveChildren: 1,
		},
		{
			name:        "idle root plus active child delegates",
			root:        Node{Runtime: RuntimeIdle},
			children:    []Node{{ID: "child", Runtime: RuntimeActive}},
			wantRuntime: RuntimeIdle, wantAttention: AttentionNone,
			wantLegacy: LegacyDelegating, wantLiveChildren: 1,
		},
		{
			name:        "idle child is live but not working",
			root:        Node{Runtime: RuntimeIdle},
			children:    []Node{{ID: "child", Runtime: RuntimeIdle, Lifecycle: LifecycleUnknown}},
			wantRuntime: RuntimeIdle, wantAttention: AttentionNone,
			wantLegacy: LegacyIdle, wantLiveChildren: 1,
		},
		{
			name:        "actionable unknown child is live",
			root:        Node{Runtime: RuntimeIdle},
			children:    []Node{{ID: "child", Runtime: RuntimeUnknown, Attention: AttentionApproval, Lifecycle: LifecycleUnknown}},
			wantRuntime: RuntimeIdle, wantAttention: AttentionApproval,
			wantLegacy: LegacyPermission, wantLiveChildren: 1, wantWaiting: 1, wantApprovals: 1,
		},
		{
			name:        "completed child does not delegate",
			root:        Node{Runtime: RuntimeIdle},
			children:    []Node{{ID: "child", Runtime: RuntimeActive, Lifecycle: LifecycleCompleted}},
			wantRuntime: RuntimeIdle, wantAttention: AttentionNone,
			wantLegacy: LegacyIdle,
		},
		{
			name:        "interrupted child does not delegate",
			root:        Node{Runtime: RuntimeIdle},
			children:    []Node{{ID: "child", Runtime: RuntimeActive, Lifecycle: LifecycleInterrupted}},
			wantRuntime: RuntimeIdle, wantAttention: AttentionNone,
			wantLegacy: LegacyIdle,
		},
		{
			name:        "shutdown child does not delegate",
			root:        Node{Runtime: RuntimeIdle},
			children:    []Node{{ID: "child", Runtime: RuntimeActive, Lifecycle: LifecycleShutdown}},
			wantRuntime: RuntimeIdle, wantAttention: AttentionNone,
			wantLegacy: LegacyIdle,
		},
		{
			name:        "not found child does not delegate",
			root:        Node{Runtime: RuntimeIdle},
			children:    []Node{{ID: "child", Runtime: RuntimeActive, Lifecycle: LifecycleNotFound}},
			wantRuntime: RuntimeIdle, wantAttention: AttentionNone,
			wantLegacy: LegacyIdle,
		},
		{
			name:        "errored child is terminal and counted",
			root:        Node{Runtime: RuntimeIdle},
			children:    []Node{{ID: "child", Lifecycle: LifecycleErrored}},
			wantRuntime: RuntimeIdle, wantAttention: AttentionNone,
			wantLegacy: LegacyIdle, wantErrors: 1,
		},
		{
			name:        "child system error is not permission",
			root:        Node{Runtime: RuntimeIdle},
			children:    []Node{{ID: "child", Runtime: RuntimeSystemError, Lifecycle: LifecycleRunning}},
			wantRuntime: RuntimeIdle, wantAttention: AttentionNone,
			wantLegacy: LegacyDelegating, wantLiveChildren: 1, wantErrors: 1,
		},
		{
			name:        "root system error is explicit and not permission",
			root:        Node{Runtime: RuntimeSystemError},
			wantRuntime: RuntimeSystemError, wantAttention: AttentionNone,
			wantErrors: 1,
		},
		{
			name:        "approval beats active root",
			root:        Node{Runtime: RuntimeActive},
			children:    []Node{{ID: "child", Attention: AttentionApproval, Lifecycle: LifecycleRunning}},
			wantRuntime: RuntimeActive, wantAttention: AttentionApproval,
			wantLegacy: LegacyPermission, wantLiveChildren: 1, wantWaiting: 1, wantApprovals: 1,
		},
		{
			name:        "user input beats active root",
			root:        Node{Runtime: RuntimeActive},
			children:    []Node{{ID: "child", Attention: AttentionUserInput, Lifecycle: LifecycleRunning}},
			wantRuntime: RuntimeActive, wantAttention: AttentionUserInput,
			wantLegacy: LegacyPermission, wantLiveChildren: 1, wantWaiting: 1, wantUserInputs: 1,
		},
		{
			name:        "waiting child beats root system error",
			root:        Node{Runtime: RuntimeSystemError},
			children:    []Node{{ID: "child", Attention: AttentionUserInput, Lifecycle: LifecycleRunning}},
			wantRuntime: RuntimeSystemError, wantAttention: AttentionUserInput,
			wantLegacy: LegacyPermission, wantLiveChildren: 1, wantWaiting: 1,
			wantUserInputs: 1, wantErrors: 1,
		},
		{
			name:        "active root beats running descendant",
			root:        Node{Runtime: RuntimeActive},
			children:    []Node{{ID: "child", Runtime: RuntimeActive, Lifecycle: LifecycleRunning}},
			wantRuntime: RuntimeActive, wantAttention: AttentionNone,
			wantLegacy: LegacyWorking, wantLiveChildren: 1,
		},
		{
			name:        "approval is compact priority while both kinds are preserved",
			root:        Node{Runtime: RuntimeIdle, Attention: AttentionUserInput},
			children:    []Node{{ID: "child", Attention: AttentionApproval, Lifecycle: LifecycleRunning}},
			wantRuntime: RuntimeIdle, wantAttention: AttentionApproval,
			wantLegacy: LegacyPermission, wantLiveChildren: 1, wantWaiting: 2,
			wantApprovals: 1, wantUserInputs: 1,
		},
		{
			name: "two simultaneous waiting children are counted",
			root: Node{Runtime: RuntimeIdle},
			children: []Node{
				{ID: "approval", Attention: AttentionApproval, Lifecycle: LifecycleRunning},
				{ID: "question", Attention: AttentionUserInput, Lifecycle: LifecycleRunning},
			},
			wantRuntime: RuntimeIdle, wantAttention: AttentionApproval,
			wantLegacy: LegacyPermission, wantLiveChildren: 2, wantWaiting: 2,
			wantApprovals: 1, wantUserInputs: 1,
		},
		{
			name:        "terminal child stale wait is ignored",
			root:        Node{Runtime: RuntimeIdle},
			children:    []Node{{ID: "child", Attention: AttentionApproval, Lifecycle: LifecycleCompleted}},
			wantRuntime: RuntimeIdle, wantAttention: AttentionNone,
			wantLegacy: LegacyIdle,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			obs := observation(tt.root, tt.children...)
			got := Reduce(obs, Summary{}, testNow)
			want := Summary{
				Runtime: tt.wantRuntime, Attention: tt.wantAttention,
				LegacyStatus: tt.wantLegacy, LiveChildren: tt.wantLiveChildren,
				WaitingNodes: tt.wantWaiting, ApprovalNodes: tt.wantApprovals,
				UserInputNodes: tt.wantUserInputs, ErrorNodes: tt.wantErrors,
				Since: testNow,
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Reduce() = %#v, want %#v", got, want)
			}
		})
	}
}

func TestReduceAllLifecycleValuesAreRepresented(t *testing.T) {
	t.Parallel()

	values := []LifecycleState{
		LifecycleUnknown,
		LifecyclePending,
		LifecycleRunning,
		LifecycleCompleted,
		LifecycleInterrupted,
		LifecycleErrored,
		LifecycleShutdown,
		LifecycleNotFound,
	}
	for _, lifecycle := range values {
		if !lifecycle.Valid() {
			t.Errorf("%q should be a valid lifecycle", lifecycle)
		}
	}
}

func TestNeutralEnumWireValues(t *testing.T) {
	t.Parallel()

	assertValues := func(name string, got, want []string) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s values = %v, want %v", name, got, want)
		}
	}
	assertValues("provider", []string{string(ProviderClaude), string(ProviderCodex)}, []string{"claude", "codex"})
	assertValues("runtime", []string{
		string(RuntimeUnknown), string(RuntimeNotLoaded), string(RuntimeIdle),
		string(RuntimeActive), string(RuntimeSystemError),
	}, []string{"unknown", "not_loaded", "idle", "active", "system_error"})
	assertValues("attention", []string{
		string(AttentionNone), string(AttentionApproval), string(AttentionUserInput),
	}, []string{"none", "approval", "user_input"})
	assertValues("lifecycle", []string{
		string(LifecycleUnknown), string(LifecyclePending), string(LifecycleRunning),
		string(LifecycleCompleted), string(LifecycleInterrupted), string(LifecycleErrored),
		string(LifecycleShutdown), string(LifecycleNotFound),
	}, []string{"unknown", "pending", "running", "completed", "interrupted", "errored", "shutdown", "not_found"})
	assertValues("source", []string{
		string(SourceCodexAppServer), string(SourceHook), string(SourceClaudeTranscript),
		string(SourceCodexRollout), string(SourceRestoredLastKnown),
	}, []string{"codex_app_server", "hook", "claude_transcript", "codex_rollout", "restored_last_known"})
}

func TestReduceFreshnessAndSource(t *testing.T) {
	t.Parallel()

	t.Run("deadline is exclusive", func(t *testing.T) {
		obs := observation(Node{Runtime: RuntimeActive})
		got := Reduce(obs, Summary{}, obs.FreshUntil)
		if got.LegacyStatus != "" || got.Runtime != RuntimeUnknown {
			t.Fatalf("expired Reduce() = %#v, want unknown", got)
		}
	})

	t.Run("future observation is not fresh", func(t *testing.T) {
		obs := observation(Node{Runtime: RuntimeActive})
		got := Reduce(obs, Summary{}, obs.ObservedAt.Add(-time.Nanosecond))
		if got.LegacyStatus != "" || got.Runtime != RuntimeUnknown {
			t.Fatalf("future Reduce() = %#v, want unknown", got)
		}
	})

	t.Run("restored last known remains authoritative until deadline", func(t *testing.T) {
		obs := observation(Node{Runtime: RuntimeActive})
		obs.Source = SourceRestoredLastKnown
		got := Reduce(obs, Summary{}, testNow)
		if got.LegacyStatus != LegacyWorking || got.Runtime != RuntimeActive {
			t.Fatalf("fresh restored Reduce() = %#v, want working", got)
		}
	})

	t.Run("source and completeness do not change reduction", func(t *testing.T) {
		want := Reduce(observation(Node{Runtime: RuntimeActive}), Summary{}, testNow)
		for _, source := range []SourceKind{SourceHook, SourceClaudeTranscript, SourceCodexRollout, SourceUnknown} {
			obs := observation(Node{Runtime: RuntimeActive})
			obs.Source = source
			obs.Complete = false
			got := Reduce(obs, Summary{}, testNow)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("source %q partial Reduce() = %#v, want %#v", source, got, want)
			}
		}
	})
}

func TestReduceIgnoresUsage(t *testing.T) {
	t.Parallel()

	without := observation(Node{Runtime: RuntimeIdle}, Node{ID: "child", Lifecycle: LifecycleRunning})
	with := without.Clone()
	with.Nodes[0].Usage = Usage{InputTokens: 10, OutputTokens: 20, TotalTokens: 30, ModelContextWindow: 100}
	with.Nodes[1].Usage = Usage{CachedInputTokens: 5, ReasoningOutputTokens: 7}
	if got, want := Reduce(with, Summary{}, testNow), Reduce(without, Summary{}, testNow); !reflect.DeepEqual(got, want) {
		t.Fatalf("usage changed reduction: got %#v, want %#v", got, want)
	}
}

func TestReduceSince(t *testing.T) {
	t.Parallel()

	started := testNow.Add(-10 * time.Second)
	prior := Summary{Runtime: RuntimeIdle, Attention: AttentionNone, LegacyStatus: LegacyIdle, Since: started}
	got := Reduce(observation(Node{Runtime: RuntimeIdle}), prior, testNow)
	if !got.Since.Equal(started) {
		t.Fatalf("identical summary Since = %v, want %v", got.Since, started)
	}

	changed := Reduce(observation(Node{Runtime: RuntimeActive}), got, testNow.Add(time.Second))
	if !changed.Since.Equal(testNow.Add(time.Second)) {
		t.Fatalf("transition Since = %v, want transition time", changed.Since)
	}

	countChangedObs := observation(Node{Runtime: RuntimeIdle}, Node{ID: "child", Lifecycle: LifecycleRunning})
	countChanged := Reduce(countChangedObs, prior, testNow.Add(2*time.Second))
	if !countChanged.Since.Equal(testNow.Add(2 * time.Second)) {
		t.Fatalf("count transition Since = %v, want transition time", countChanged.Since)
	}
}

func TestNormalizeValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		obs  Observation
		kind error
	}{
		{
			name: "missing root",
			obs:  Observation{RootID: "root", Nodes: []Node{{ID: "child", ParentID: "root"}}},
			kind: ErrMissingRoot,
		},
		{
			name: "duplicate id",
			obs:  Observation{RootID: "root", Nodes: []Node{{ID: "root"}, {ID: "root"}}},
			kind: ErrDuplicateNode,
		},
		{
			name: "orphan",
			obs:  Observation{RootID: "root", Nodes: []Node{{ID: "root"}, {ID: "child", ParentID: "missing"}}},
			kind: ErrOrphanNode,
		},
		{
			name: "cycle",
			obs: Observation{RootID: "root", Nodes: []Node{
				{ID: "root"}, {ID: "a", ParentID: "b"}, {ID: "b", ParentID: "a"},
			}},
			kind: ErrCycle,
		},
		{
			name: "second root",
			obs:  Observation{RootID: "root", Nodes: []Node{{ID: "root"}, {ID: "other"}}},
			kind: ErrOrphanNode,
		},
		{
			name: "root parent",
			obs:  Observation{RootID: "root", Nodes: []Node{{ID: "root", ParentID: "parent"}}},
			kind: ErrRootHasParent,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Normalize(tt.obs)
			if !errors.Is(err, tt.kind) {
				t.Fatalf("Normalize() error = %v, want %v", err, tt.kind)
			}
			if got.Diagnostic == "" {
				t.Fatal("Normalize() did not produce diagnostic")
			}
			if len(got.Nodes) != 0 {
				t.Fatalf("invalid Normalize() returned %d usable nodes", len(got.Nodes))
			}
		})
	}
}

func TestNormalizeDeterministicDepthFirstOrder(t *testing.T) {
	t.Parallel()

	input := Observation{
		RootID: "root",
		Nodes: []Node{
			{ID: "z-child", ParentID: "root", Nickname: "zeta"},
			{ID: "a-grandchild", ParentID: "a-child", Nickname: "first"},
			{ID: "root"},
			{ID: "b-grandchild", ParentID: "a-child", Nickname: "second"},
			{ID: "a-child", ParentID: "root", Nickname: "alpha"},
			{ID: "a2-child", ParentID: "root", Nickname: "alpha", Role: "z-role"},
		},
	}
	want := []string{"root", "a-child", "a-grandchild", "b-grandchild", "a2-child", "z-child"}

	for i := 0; i < 10; i++ {
		got, err := Normalize(input)
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]string, len(got.Nodes))
		for j := range got.Nodes {
			ids[j] = got.Nodes[j].ID
		}
		if !reflect.DeepEqual(ids, want) {
			t.Fatalf("Normalize() order = %v, want %v", ids, want)
		}
	}
}

func TestNormalizeAndCloneDoNotAliasNodeSlices(t *testing.T) {
	t.Parallel()

	original := observation(Node{Runtime: RuntimeIdle}, Node{ID: "child", Lifecycle: LifecycleRunning})
	normalized, err := Normalize(original)
	if err != nil {
		t.Fatal(err)
	}
	original.Nodes[1].Nickname = "mutated-original"
	if normalized.Nodes[1].Nickname == "mutated-original" {
		t.Fatal("normalized snapshot aliases source nodes")
	}

	cloned := normalized.Clone()
	cloned.Nodes[1].Nickname = "mutated-clone"
	if normalized.Nodes[1].Nickname == "mutated-clone" {
		t.Fatal("Clone aliases source nodes")
	}
}

func TestPruneTerminal(t *testing.T) {
	t.Parallel()

	obs := observation(
		Node{Runtime: RuntimeIdle},
		Node{ID: "old", Lifecycle: LifecycleCompleted},
		Node{ID: "current", Lifecycle: LifecycleCompleted},
		Node{ID: "live", Lifecycle: LifecycleRunning},
		Node{ID: "terminal-parent", Lifecycle: LifecycleCompleted},
		Node{ID: "live-grandchild", ParentID: "terminal-parent", Lifecycle: LifecycleRunning},
	)
	retainCurrentCohort := map[string]struct{}{"current": {}}
	got, err := PruneTerminal(obs, retainCurrentCohort)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"root", "current", "live", "terminal-parent", "live-grandchild"}
	ids := make([]string, len(got.Nodes))
	for i := range got.Nodes {
		ids[i] = got.Nodes[i].ID
	}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("PruneTerminal() IDs = %v, want %v", ids, want)
	}
	if len(obs.Nodes) != 6 {
		t.Fatalf("PruneTerminal mutated input: %d nodes", len(obs.Nodes))
	}
	delete(retainCurrentCohort, "current")
	retainCurrentCohort["old"] = struct{}{}
	if len(got.Nodes) != len(want) {
		t.Fatal("PruneTerminal retained the caller's cohort map")
	}
}

func observation(root Node, children ...Node) Observation {
	root.ID = "root"
	root.ParentID = ""
	if root.Lifecycle == "" {
		root.Lifecycle = LifecycleUnknown
	}
	if root.Attention == "" {
		root.Attention = AttentionNone
	}
	if root.Runtime == "" {
		root.Runtime = RuntimeUnknown
	}
	nodes := []Node{root}
	for _, child := range children {
		if child.ParentID == "" {
			child.ParentID = "root"
		}
		if child.Lifecycle == "" {
			child.Lifecycle = LifecycleUnknown
		}
		if child.Attention == "" {
			child.Attention = AttentionNone
		}
		if child.Runtime == "" {
			child.Runtime = RuntimeUnknown
		}
		nodes = append(nodes, child)
	}
	return Observation{
		Provider: ProviderCodex, RootID: "root", Nodes: nodes,
		Source: SourceCodexAppServer, ObservedAt: testObserved,
		FreshUntil: testFresh, Complete: true,
	}
}
