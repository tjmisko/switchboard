package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/history"
	claudeprovider "github.com/tjmisko/switchboard/internal/provider/claude"
	"github.com/tjmisko/switchboard/internal/state"
)

// Phase 0a of the status-evidence programme, at the landing point: observeAt
// records observe_error and then applies whatever the provider returned, so a
// failed fanout scan that came back newly dated renewed the published graph's
// authority on every tick.
func TestClaudeFanoutScanFailureAtTheCoordinator(t *testing.T) {
	t.Run("should not renew the published graph's freshness when a Claude fan-out scan fails", func(t *testing.T) {
		store := state.New("")
		ref := seedCoordinatorSession(store, 4931, time.Now().Add(-time.Hour), state.AgentKindClaude, "claude-root", "/project")
		ref.Transcript = seedCoordinatorTranscript(t, store, ref.PID, "claude-root")
		coordinator := newAgentCoordinator(store, history.NewSink(history.Config{}), claudeprovider.NewObserver(t.TempDir()), nil)
		coordinator.refreshTrackedRoots()
		defer coordinator.Close()

		// After the wall-clock restore the first hook triggers, so no tick below
		// is older than the adapter's own last observation.
		now := time.Now().Add(time.Second)
		claudeHook(t, coordinator, store, ref, "UserPromptSubmit", "", "", now)
		scannedAt := now.Add(time.Second)
		coordinator.observeAt(context.Background(), ref, scannedAt)
		graph := publishedGraph(t, store)
		if !graph.ObservedAt.Equal(scannedAt) || graph.Summary.Status != agentgraph.LegacyWorking {
			t.Fatalf("baseline graph = observed %v status %q, want a working scan dated %v", graph.ObservedAt, graph.Summary.Status, scannedAt)
		}
		deadline := graph.FreshUntil

		subagents := filepath.Join(filepath.Dir(ref.Transcript), ref.ProviderSessionID, "subagents")
		if err := os.Remove(subagents); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(subagents, nil, 0o644); err != nil {
			t.Fatal(err)
		}

		coordinator.observeAt(context.Background(), ref, scannedAt.Add(5*time.Second))
		if got := claudeDiagnosticCount(coordinator, "observe_error"); got != 1 {
			t.Fatalf("observe_error count = %d, want the one failed scan", got)
		}
		graph = publishedGraph(t, store)
		if !graph.ObservedAt.Equal(scannedAt) || !graph.FreshUntil.Equal(deadline) {
			t.Fatalf("failed scan republished the graph as [%v, %v), want the last scan's [%v, %v)",
				graph.ObservedAt, graph.FreshUntil, scannedAt, deadline)
		}
		if graph.Summary.Status != agentgraph.LegacyWorking {
			t.Fatalf("status inside the held window = %q, want working", graph.Summary.Status)
		}

		coordinator.observeAt(context.Background(), ref, deadline)
		graph = publishedGraph(t, store)
		if !graph.FreshUntil.Equal(deadline) || graph.Fresh(deadline) {
			t.Fatalf("graph at the original deadline = fresh until %v, want expired at %v", graph.FreshUntil, deadline)
		}
		if graph.Summary.Runtime != agentgraph.RuntimeUnknown || graph.Summary.Status != "" {
			t.Fatalf("summary at the original deadline = %+v, want unknown", graph.Summary)
		}
	})
}

func publishedGraph(t *testing.T, store *state.Store) *state.AgentGraph {
	t.Helper()
	sessions := store.Snapshot().Sessions
	if len(sessions) != 1 || sessions[0].AgentGraph == nil {
		t.Fatalf("snapshot has no agent graph: %+v", sessions)
	}
	return sessions[0].AgentGraph
}
