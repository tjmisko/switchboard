package state_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/state"
)

var (
	graphObserved = time.Date(2026, 8, 21, 16, 0, 0, 0, time.UTC)
	graphFresh    = graphObserved.Add(time.Minute)
)

func graphObservation() agentgraph.Observation {
	return agentgraph.Observation{
		Provider:   agentgraph.ProviderCodex,
		RootID:     "root-thread",
		Source:     agentgraph.SourceCodexAppServer,
		ObservedAt: graphObserved,
		FreshUntil: graphFresh,
		Complete:   true,
		Nodes: []agentgraph.Node{
			{
				ID:          "child-z",
				ParentID:    "root-thread",
				Nickname:    "tagging",
				Role:        "worker",
				Description: "safe display description",
				Runtime:     agentgraph.RuntimeActive,
				Attention:   agentgraph.AttentionNone,
				Lifecycle:   agentgraph.LifecycleRunning,
				StartedAt:   graphObserved.Add(-2 * time.Minute),
				UpdatedAt:   graphObserved.Add(-time.Second),
				Usage: agentgraph.Usage{
					InputTokens:       120,
					CachedInputTokens: 40,
					OutputTokens:      15,
					TotalTokens:       175,
				},
			},
			{
				ID:        "root-thread",
				Runtime:   agentgraph.RuntimeIdle,
				Attention: agentgraph.AttentionNone,
				Lifecycle: agentgraph.LifecycleRunning,
				StartedAt: graphObserved.Add(-10 * time.Minute),
				UpdatedAt: graphObserved,
			},
			{
				ID:        "child-a",
				ParentID:  "root-thread",
				Nickname:  "documents",
				Role:      "explorer",
				Runtime:   agentgraph.RuntimeIdle,
				Attention: agentgraph.AttentionUserInput,
				Lifecycle: agentgraph.LifecycleRunning,
				StartedAt: graphObserved.Add(-3 * time.Minute),
				UpdatedAt: graphObserved.Add(-2 * time.Second),
			},
		},
	}
}

func TestProjectAgentGraphIsDeterministicAndDetached(t *testing.T) {
	obs := graphObservation()
	graph, err := state.ProjectAgentGraph(obs, nil, graphObserved)
	if err != nil {
		t.Fatalf("ProjectAgentGraph: %v", err)
	}

	wantOrder := []string{"root-thread", "child-a", "child-z"}
	if len(graph.Nodes) != len(wantOrder) {
		t.Fatalf("nodes = %d, want %d", len(graph.Nodes), len(wantOrder))
	}
	for i, want := range wantOrder {
		if graph.Nodes[i].ID != want {
			t.Fatalf("node order = %v, want %v", []string{graph.Nodes[0].ID, graph.Nodes[1].ID, graph.Nodes[2].ID}, wantOrder)
		}
	}
	if graph.Summary.Status != state.StatusPermission || graph.Summary.Attention != agentgraph.AttentionUserInput {
		t.Fatalf("summary = %+v, want user-input permission", graph.Summary)
	}
	if graph.Summary.LiveChildren != 2 || graph.Summary.WaitingNodes != 1 || graph.Summary.UserInputNodes != 1 {
		t.Fatalf("summary counts = %+v", graph.Summary)
	}

	// The projection owns its slice and values; mutating provider memory after the
	// call cannot change state's public graph.
	obs.Nodes[0].Nickname = "mutated-provider-cache"
	obs.Nodes[0].Usage.InputTokens = 999
	if graph.Nodes[2].Nickname != "tagging" || graph.Nodes[2].Usage.InputTokens != 120 {
		t.Fatalf("projection shares provider memory: %+v", graph.Nodes[2])
	}

	a, err := json.Marshal(graph)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(graph.Clone())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("clone encoding differs:\n%s\n%s", a, b)
	}

	permuted := graphObservation()
	permuted.Nodes[0], permuted.Nodes[2] = permuted.Nodes[2], permuted.Nodes[0]
	permutedGraph, err := state.ProjectAgentGraph(permuted, nil, graphObserved)
	if err != nil {
		t.Fatal(err)
	}
	c, err := json.Marshal(permutedGraph)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, c) {
		t.Fatalf("provider node order changed deterministic JSON:\n%s\n%s", a, c)
	}
}

func TestSetAgentGraphProjectsLegacyWithoutDiscardingClaudeCompatibilityFields(t *testing.T) {
	obs := graphObservation()
	first, err := state.ProjectAgentGraph(obs, nil, graphObserved)
	if err != nil {
		t.Fatal(err)
	}
	sess := state.Session{
		PID:   7,
		Agent: state.AgentKindCodex,
		Codex: &state.AgentInfo{
			Status:            state.StatusIdle,
			StatusSince:       graphObserved.Add(-time.Hour),
			InFlightSubagents: 3,
			Workflows: []state.WorkflowStatus{{
				RunID: "wf-legacy", AgentsStarted: 3, AgentsDone: 1, InFlight: 2,
			}},
		},
	}
	sess.SetAgentGraph(first)
	if sess.AgentGraph == first {
		t.Fatal("SetAgentGraph retained caller-owned graph pointer")
	}
	first.Nodes[0].ID = "mutated-caller"
	if sess.AgentGraph.Nodes[0].ID != "root-thread" {
		t.Fatal("SetAgentGraph retained caller-owned node slice")
	}
	if sess.Codex.SessionID != "root-thread" || sess.Codex.Status != state.StatusPermission {
		t.Fatalf("legacy projection = %+v", sess.Codex)
	}
	if !sess.Codex.StatusSince.Equal(graphObserved) {
		t.Fatalf("status_since = %v, want %v", sess.Codex.StatusSince, graphObserved)
	}
	if sess.Codex.InFlightSubagents != 3 || len(sess.Codex.Workflows) != 1 {
		t.Fatalf("legacy Claude-compatible fields were discarded: %+v", sess.Codex)
	}
	if sess.Claude != nil {
		t.Fatalf("Codex graph created a Claude block: %+v", sess.Claude)
	}

	// A structured-summary transition (a child count changes) may reset the
	// graph summary's since, but status_since remains the start of the legacy
	// permission status until that status itself changes.
	priorLegacySince := sess.Codex.StatusSince
	obs.Nodes = append(obs.Nodes, agentgraph.Node{
		ID: "child-b", ParentID: obs.RootID, Runtime: agentgraph.RuntimeIdle,
		Attention: agentgraph.AttentionNone, Lifecycle: agentgraph.LifecycleRunning,
	})
	obs.ObservedAt = graphObserved.Add(10 * time.Second)
	obs.FreshUntil = obs.ObservedAt.Add(time.Minute)
	second, err := state.ProjectAgentGraph(obs, first, obs.ObservedAt)
	if err != nil {
		t.Fatal(err)
	}
	if second.Summary.Since.Equal(first.Summary.Since) {
		t.Fatal("structured graph summary transition did not reset graph summary since")
	}
	sess.SetAgentGraph(second)
	if !sess.Codex.StatusSince.Equal(priorLegacySince) {
		t.Fatalf("same legacy status moved status_since: got %v want %v", sess.Codex.StatusSince, priorLegacySince)
	}

	for i := range obs.Nodes {
		obs.Nodes[i].Attention = agentgraph.AttentionNone
		obs.Nodes[i].Runtime = agentgraph.RuntimeIdle
		if obs.Nodes[i].ID != obs.RootID {
			obs.Nodes[i].Lifecycle = agentgraph.LifecycleCompleted
		}
	}
	obs.ObservedAt = graphObserved.Add(20 * time.Second)
	obs.FreshUntil = obs.ObservedAt.Add(time.Minute)
	third, err := state.ProjectAgentGraph(obs, second, obs.ObservedAt)
	if err != nil {
		t.Fatal(err)
	}
	sess.SetAgentGraph(third)
	if sess.Codex.Status != state.StatusIdle || !sess.Codex.StatusSince.Equal(obs.ObservedAt) {
		t.Fatalf("real legacy transition = %+v, want idle since %v", sess.Codex, obs.ObservedAt)
	}
}

func TestStoreSnapshotDeepCopiesAgentGraph(t *testing.T) {
	graph, err := state.ProjectAgentGraph(graphObservation(), nil, graphObserved)
	if err != nil {
		t.Fatal(err)
	}
	store := state.New("")
	store.Apply(func(m map[int]*state.Session) {
		sess := &state.Session{PID: 1, Agent: state.AgentKindCodex, Codex: &state.AgentInfo{}}
		sess.SetAgentGraph(graph)
		m[1] = sess
	})

	first := store.Snapshot()
	first.Sessions[0].AgentGraph.Nodes[0].ID = "mutated-snapshot"
	first.Sessions[0].AgentGraph.Nodes = append(first.Sessions[0].AgentGraph.Nodes, state.AgentNode{ID: "injected"})
	first.Sessions[0].AgentGraph.Summary.Status = "mutated"

	second := store.Snapshot()
	got := second.Sessions[0].AgentGraph
	if got.Nodes[0].ID != "root-thread" || len(got.Nodes) != 3 || got.Summary.Status != state.StatusPermission {
		t.Fatalf("snapshot mutation reached store: %+v", got)
	}
}

func TestAgentGraphGolden(t *testing.T) {
	graph, err := state.ProjectAgentGraph(graphObservation(), nil, graphObserved)
	if err != nil {
		t.Fatal(err)
	}
	snap := state.Snapshot{
		Sessions: []state.Session{{
			PID:       42,
			CWD:       "/workspace",
			TTY:       "/dev/pts/42",
			StartedAt: graphObserved.Add(-time.Hour),
			Agent:     state.AgentKindCodex,
			Codex: &state.AgentInfo{
				SessionID: "root-thread", Status: state.StatusPermission,
				StatusSinceWire: timePtr(graphObserved),
			},
			AgentGraph: graph,
		}},
		UpdatedAt: graphObserved,
	}
	got, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	want, err := os.ReadFile(filepath.Join("testdata", "agent-graph.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("graph golden mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func timePtr(t time.Time) *time.Time { return &t }
