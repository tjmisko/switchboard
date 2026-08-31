package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/provider"
	claudeprovider "github.com/tjmisko/switchboard/internal/provider/claude"
	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/statustune"
)

// Phase 3 of docs/askuserquestion-model-plan.md §4. Since 784e3a8 the provider
// graph decides every Claude edge and names the rule it decided by, and the
// coordinator threw that name away: every transition in the record read
// `agent_graph_authority`, which says only "the graph said so". These tests pin
// the two paths that produce a rule — the hook edge and the transcript
// resolution — to the `rule` actually written to the activity log, because
// nothing in phases 1, 2 or 4 is measurable from a record that cannot say why a
// red opened, held or closed.
func TestClaudeTransitionsRecordTheRuleThatDecidedThem(t *testing.T) {
	t.Run("should record the hook's own rule when a permission edge opens and clears a red", func(t *testing.T) {
		histDir := t.TempDir()
		// The minimal privacy tier, deliberately: a rule id is a bounded decision
		// name and must survive the scrub that drops the tool and the cwd.
		sink := history.NewSink(history.Config{Enabled: true, Detail: history.DetailMinimal, Dir: histDir})
		store := state.New("")
		ref := seedCoordinatorSession(store, 4901, time.Now().Add(-time.Hour), state.AgentKindClaude, "claude-root", "/project")
		coordinator := newAgentCoordinator(store, sink, claudeprovider.NewObserver(t.TempDir()), nil)
		coordinator.refreshTrackedRoots()
		defer coordinator.Close()

		now := time.Now()
		claudeHook(t, coordinator, store, ref, "PermissionRequest", "AskUserQuestion", "ask-1", now)
		claudeHook(t, coordinator, store, ref, "PostToolUse", "AskUserQuestion", "ask-1", now.Add(time.Second))
		sink.Close()

		edges := eventsOfType(readEvents(t, histDir), history.EventTransition)
		if len(edges) != 2 {
			t.Fatalf("recorded %d transitions, want the open and the clear: %+v", len(edges), edges)
		}
		if edges[0].To != state.StatusPermission || edges[0].Rule != statustune.RuleGraphPermissionRecorded {
			t.Errorf("open edge = %s->%s rule %q, want the permission open rule",
				edges[0].From, edges[0].To, edges[0].Rule)
		}
		if edges[1].To != state.StatusWorking || edges[1].Rule != statustune.RuleGraphToolMatchCleared {
			t.Errorf("clear edge = %s->%s rule %q, want the tool-match clear rule",
				edges[1].From, edges[1].To, edges[1].Rule)
		}
		assertRulesAreDiagnosable(t, edges)
	})

	t.Run("should record the transcript resolution's reason when an observe tick releases a red", func(t *testing.T) {
		histDir := t.TempDir()
		sink := history.NewSink(history.Config{Enabled: true, Detail: history.DetailFull, Dir: histDir})
		store := state.New("")
		ref := seedCoordinatorSession(store, 4902, time.Now().Add(-time.Hour), state.AgentKindClaude, "claude-root", "/project")
		ref.Transcript = seedCoordinatorTranscript(t, store, ref.PID, "claude-root")
		coordinator := newAgentCoordinator(store, sink, claudeprovider.NewObserver(t.TempDir()), nil)
		coordinator.refreshTrackedRoots()
		defer coordinator.Close()

		now := time.Now()
		claudeHook(t, coordinator, store, ref, "PermissionRequest", "Bash", "call-1", now.Add(-time.Minute))
		// No PostToolUse ever arrives; the writer's own transcript is what proves
		// the turn moved on, which is the path that produces a resolution reason.
		appendTranscriptLine(t, ref.Transcript,
			`{"type":"assistant","timestamp":"`+now.Add(-30*time.Second).Format(time.RFC3339Nano)+`","message":{"role":"assistant","content":[]}}`)
		coordinator.observe(context.Background(), ref)
		sink.Close()

		edges := eventsOfType(readEvents(t, histDir), history.EventTransition)
		if len(edges) != 2 {
			t.Fatalf("recorded %d transitions, want the open and the resolution: %+v", len(edges), edges)
		}
		if edges[1].To != state.StatusWorking || edges[1].Rule != statustune.RuleGraphWriterResumed {
			t.Errorf("resolution edge = %s->%s rule %q, want the resumed-writer rule",
				edges[1].From, edges[1].To, edges[1].Rule)
		}
		assertRulesAreDiagnosable(t, edges)
	})
}

// A rule id earns its place in the record only if a reader can act on it, so
// every id the daemon writes must resolve to a knob hint. RuleKnob's fallback
// ("unrecognized rule id (daemon version skew?)") is for version skew between a
// log and a binary, never for a rule this binary itself just emitted.
func assertRulesAreDiagnosable(t *testing.T, edges []history.Event) {
	t.Helper()
	for _, edge := range edges {
		if edge.Rule == "" {
			t.Errorf("transition %s->%s recorded no rule at all", edge.From, edge.To)
			continue
		}
		if statustune.RuleKnob(edge.Rule).What == "" {
			t.Errorf("rule %q has no ruleKnobs entry, so `switchboard-ctl diagnose` reports it as version skew", edge.Rule)
		}
	}
}

func claudeHook(t *testing.T, coordinator *agentCoordinator, store *state.Store, ref provider.RootRef, event, tool, hash string, at time.Time) {
	t.Helper()
	sess, ok := sessionForKey(store.Snapshot(), ref.Key())
	if !ok {
		t.Fatalf("Claude root discovery was lost before the %q hook", event)
	}
	coordinator.HandleHook(rpc.Request{
		Agent: state.AgentKindClaude, Event: event, SessionID: ref.ProviderSessionID,
		Transcript: ref.Transcript, ToolName: tool, ToolInputHash: hash, ObservedAt: at,
	}, sess)
}

// seedCoordinatorTranscript gives a seeded session the on-disk layout the
// Claude observer expects — an empty main transcript beside the session's
// subagent directory — and returns the transcript path.
func seedCoordinatorTranscript(t *testing.T, store *state.Store, pid int, sessionID string) string {
	t.Helper()
	base := t.TempDir()
	path := filepath.Join(base, sessionID+".jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, sessionID, "subagents"), 0o755); err != nil {
		t.Fatal(err)
	}
	store.Apply(func(sessions map[int]*state.Session) {
		sessions[pid].AgentBlock(state.AgentKindClaude).Transcript = path
	})
	return path
}

func appendTranscriptLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}
