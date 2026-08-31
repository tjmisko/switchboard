package claude

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/provider"
)

var _ provider.Observer = (*Observer)(nil)

func TestHookRootTransitionsAndQuestionAttention(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()

	result := o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})
	assertSummary(t, result.Observation, now, agentgraph.LegacyIdle, agentgraph.AttentionNone)

	result = o.ApplyHook(HookSignal{Root: root, Event: "UserPromptSubmit", At: now.Add(time.Second)})
	assertSummary(t, result.Observation, now.Add(time.Second), agentgraph.LegacyWorking, agentgraph.AttentionNone)

	result = o.ApplyHook(HookSignal{
		Root: root, Event: "PermissionRequest", ToolName: "AskUserQuestion",
		ToolInputHash: "ask-1", At: now.Add(2 * time.Second),
	})
	assertSummary(t, result.Observation, now.Add(2*time.Second), agentgraph.LegacyPermission, agentgraph.AttentionUserInput)
	if got := result.Observation.Nodes[0].Attention; got != agentgraph.AttentionUserInput {
		t.Fatalf("root attention = %q, want user_input", got)
	}

	result = o.ApplyHook(HookSignal{
		Root: root, Event: "PostToolUse", ToolName: "AskUserQuestion",
		ToolInputHash: "ask-1", At: now.Add(3 * time.Second),
	})
	assertSummary(t, result.Observation, now.Add(3*time.Second), agentgraph.LegacyWorking, agentgraph.AttentionNone)
	projection := result.Projection
	if projection.Status != agentgraph.LegacyWorking || len(projection.Pending) != 0 || projection.PendingTool != "" {
		t.Fatalf("projection after resolution = %+v", projection)
	}
}

func TestHookSignalsCoalescedUpdateAndProjectionIsDetached(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	result := o.ApplyHook(HookSignal{Root: root, Event: "PermissionRequest", ToolName: "Edit", At: now})
	if !result.Applied {
		t.Fatal("recognized hook was not applied")
	}
	select {
	case key := <-o.Updates():
		if key != root.Key() {
			t.Fatalf("update key = %+v, want %+v", key, root.Key())
		}
	default:
		t.Fatal("hook did not signal an observation update")
	}
	result.Projection.Pending[""] = PendingPrompt{Tool: "mutated"}
	result.Projection.PendingWriters[0] = "mutated"
	projection := o.Projection(root.Key())
	if projection.Pending[""].Tool != "Edit" || !reflect.DeepEqual(projection.PendingWriters, []string{PendingWriterMain}) {
		t.Fatalf("caller mutated compatibility cache: %+v", projection)
	}
}

func TestObserveRequiresExactClaudeSessionBinding(t *testing.T) {
	o := NewObserver(t.TempDir())
	defer o.Close()
	now := time.Now()
	if _, err := o.Observe(context.Background(), provider.RootRef{Provider: agentgraph.ProviderClaude}, now); !errors.Is(err, ErrMissingSession) {
		t.Fatalf("missing session error = %v", err)
	}
	if _, err := o.Observe(context.Background(), provider.RootRef{Provider: agentgraph.ProviderKind("other"), ProviderSessionID: "thread"}, now); !errors.Is(err, ErrWrongProvider) {
		t.Fatalf("wrong provider error = %v", err)
	}
}

func TestChildAttentionOwnershipAndWriterCollision(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})

	// Normalize exactly one on-disk prefix and keep prompt ownership on the child.
	o.ApplyHook(HookSignal{
		Root: root, Event: "PermissionRequest", AgentID: "agent-child-a",
		AgentType: "Explore", ToolName: "Bash", ToolInputHash: "same",
		At: now.Add(time.Second),
	})
	result := o.ApplyHook(HookSignal{
		Root: root, Event: "PermissionRequest", AgentID: "child-b",
		AgentType: "general-purpose", ToolName: "AskUserQuestion",
		At: now.Add(2 * time.Second),
	})
	if !result.Projection.StatusSince.Equal(now.Add(time.Second)) {
		t.Fatalf("second blocked writer moved legacy StatusSince to %v", result.Projection.StatusSince)
	}
	if result.Observation.Nodes[0].Attention != agentgraph.AttentionNone {
		t.Fatalf("child prompt leaked onto root node: %+v", result.Observation.Nodes[0])
	}
	byID := graphNodesByID(result.Observation.Nodes)
	if byID["child-a"].Attention != agentgraph.AttentionApproval || byID["child-b"].Attention != agentgraph.AttentionUserInput {
		t.Fatalf("child attention = a:%q b:%q", byID["child-a"].Attention, byID["child-b"].Attention)
	}
	assertSummary(t, result.Observation, now.Add(2*time.Second), agentgraph.LegacyPermission, agentgraph.AttentionApproval)

	// Same tool/hash from a teammate cannot clear child-a's prompt.
	result = o.ApplyHook(HookSignal{
		Root: root, Event: "PostToolUse", AgentID: "child-b", ToolName: "Bash",
		ToolInputHash: "same", At: now.Add(3 * time.Second),
	})
	if got := sortedPendingWriters(result.Projection.Pending); !reflect.DeepEqual(got, []string{"child-a", "child-b"}) {
		t.Fatalf("teammate collision changed pending writers: %v", got)
	}

	// Correct-writer completion removes only that writer; the other question
	// continues to make the aggregate summary red.
	result = o.ApplyHook(HookSignal{
		Root: root, Event: "PostToolUse", AgentID: "child-a", ToolName: "Bash",
		ToolInputHash: "same", At: now.Add(4 * time.Second),
	})
	if got := sortedPendingWriters(result.Projection.Pending); !reflect.DeepEqual(got, []string{"child-b"}) {
		t.Fatalf("writer-local clear pending writers = %v, want child-b", got)
	}
	assertSummary(t, result.Observation, now.Add(4*time.Second), agentgraph.LegacyPermission, agentgraph.AttentionUserInput)
}

// Two gated calls dispatched in one assistant turn are two prompts, not one.
// Answering either must hold the chip red while the other still blocks the
// agent: a chip that goes green here is a missed RED — silent, and it costs the
// user the entire remaining wait (askuserquestion-model-plan.md §2.2, M2).
func TestParallelPromptsFromOneWriterHoldRedUntilEveryCallIsAnswered(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})

	first := o.ApplyHook(HookSignal{
		Root: root, Event: "PermissionRequest", ToolName: "Bash",
		ToolInputHash: "call-a", At: now.Add(time.Second),
	})
	if first.PromptDepth != 1 {
		t.Fatalf("first prompt depth = %d, want 1", first.PromptDepth)
	}
	second := o.ApplyHook(HookSignal{
		Root: root, Event: "PermissionRequest", ToolName: "Bash",
		ToolInputHash: "call-b", At: now.Add(2 * time.Second),
	})
	if second.PromptDepth != 2 {
		t.Fatalf("parallel prompt depth = %d, want 2", second.PromptDepth)
	}
	if !second.Projection.StatusSince.Equal(now.Add(time.Second)) {
		t.Fatalf("second parallel prompt restarted the episode clock at %v", second.Projection.StatusSince)
	}

	// The user answers the second call. The first is still blocking.
	result := o.ApplyHook(HookSignal{
		Root: root, Event: "PostToolUse", ToolName: "Bash",
		ToolInputHash: "call-b", At: now.Add(3 * time.Second),
	})
	if result.Rule != "writer_tool_match_cleared" || result.PromptDepth != 1 {
		t.Fatalf("answering one parallel call = rule %q depth %d, want one prompt still open", result.Rule, result.PromptDepth)
	}
	if got := sortedPendingWriters(result.Projection.Pending); !reflect.DeepEqual(got, []string{""}) {
		t.Fatalf("first parallel prompt lost its owner: %v", got)
	}
	assertSummary(t, result.Observation, now.Add(3*time.Second), agentgraph.LegacyPermission, agentgraph.AttentionApproval)

	// Only the last answer may take the chip out of red.
	result = o.ApplyHook(HookSignal{
		Root: root, Event: "PostToolUse", ToolName: "Bash",
		ToolInputHash: "call-a", At: now.Add(4 * time.Second),
	})
	if result.PromptDepth != 0 || len(result.Projection.Pending) != 0 || result.Projection.PendingTool != "" {
		t.Fatalf("last answer did not clear the writer: depth %d projection %+v", result.PromptDepth, result.Projection)
	}
	assertSummary(t, result.Observation, now.Add(4*time.Second), agentgraph.LegacyWorking, agentgraph.AttentionNone)
}

// A writer's node attention is the fold over its open prompts and must use the
// reducer's own precedence: approval outranks user_input, so the folded value
// only relaxes as prompts are actually answered.
func TestWriterAttentionFoldsApprovalAheadOfUserInputWhilePromptsAreOpen(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})
	o.ApplyHook(HookSignal{
		Root: root, Event: "PermissionRequest", ToolName: "AskUserQuestion",
		ToolInputHash: "ask-1", At: now.Add(time.Second),
	})
	result := o.ApplyHook(HookSignal{
		Root: root, Event: "PermissionRequest", ToolName: "Bash",
		ToolInputHash: "call-a", At: now.Add(2 * time.Second),
	})
	if got := result.Observation.Nodes[0].Attention; got != agentgraph.AttentionApproval {
		t.Fatalf("folded attention with an open approval = %q, want approval", got)
	}

	result = o.ApplyHook(HookSignal{
		Root: root, Event: "PostToolUse", ToolName: "Bash",
		ToolInputHash: "call-a", At: now.Add(3 * time.Second),
	})
	if got := result.Observation.Nodes[0].Attention; got != agentgraph.AttentionUserInput {
		t.Fatalf("folded attention after the approval closed = %q, want user_input", got)
	}
	if got := result.Projection.PendingTool; got != "AskUserQuestion" {
		t.Fatalf("projected pending tool = %q, want the surviving question", got)
	}

	result = o.ApplyHook(HookSignal{
		Root: root, Event: "PostToolUse", ToolName: "AskUserQuestion",
		ToolInputHash: "ask-1", At: now.Add(4 * time.Second),
	})
	if got := result.Observation.Nodes[0].Attention; got != agentgraph.AttentionNone {
		t.Fatalf("attention after every prompt closed = %q, want none", got)
	}
}

func TestParallelPromptSetIsBoundedAndDedupesRedeliveredHooks(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})

	// A verbatim redelivery of one hook edge is the same prompt, not a second.
	signal := HookSignal{
		Root: root, Event: "PermissionRequest", ToolName: "Bash",
		ToolInputHash: "call-0", At: now.Add(time.Second),
	}
	o.ApplyHook(signal)
	repeat := o.ApplyHook(signal)
	if repeat.Changed || repeat.PromptDepth != 1 {
		t.Fatalf("redelivered hook = changed %v depth %d, want the same single prompt", repeat.Changed, repeat.PromptDepth)
	}

	var result HookResult
	for i := 1; i <= maxPendingPromptsPerWriter; i++ {
		result = o.ApplyHook(HookSignal{
			Root: root, Event: "PermissionRequest", ToolName: "Bash",
			ToolInputHash: "call-" + strconv.Itoa(i), At: now.Add(time.Duration(i+1) * time.Second),
		})
	}
	if result.PromptDepth != maxPendingPromptsPerWriter {
		t.Fatalf("prompt depth = %d, want the cap %d", result.PromptDepth, maxPendingPromptsPerWriter)
	}
	// The oldest record is the one dropped, so its answer no longer matches.
	result = o.ApplyHook(HookSignal{
		Root: root, Event: "PostToolUse", ToolName: "Bash",
		ToolInputHash: "call-0", At: now.Add(time.Minute),
	})
	if result.Rule != "writer_prompt_held" || result.PromptDepth != maxPendingPromptsPerWriter {
		t.Fatalf("overflow eviction = rule %q depth %d, want the oldest dropped", result.Rule, result.PromptDepth)
	}
}

// The overflow bound must sit far outside real dispatch width. The corpus
// maximum of 8 parallel calls is an observed maximum, not a limit Claude Code
// enforces, so a cap set at 8 would evict a LIVE prompt from the first 9-way
// turn and go green with that call still blocking — a missed RED. Nine parallel
// calls must all stay open, and only the last answer may leave red.
func TestEveryCallStaysOpenWhenOneTurnDispatchesPastTheMeasuredParallelMaximum(t *testing.T) {
	const observedCorpusMaximum = 8
	if maxPendingPromptsPerWriter <= observedCorpusMaximum {
		t.Fatalf("cap %d leaves no headroom over the observed corpus maximum %d",
			maxPendingPromptsPerWriter, observedCorpusMaximum)
	}

	o, root, now := newTestObserver(t)
	defer o.Close()
	o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})

	const parallel = observedCorpusMaximum + 1
	for i := 1; i <= parallel; i++ {
		result := o.ApplyHook(HookSignal{
			Root: root, Event: "PermissionRequest", ToolName: "Bash",
			ToolInputHash: "call-" + strconv.Itoa(i), At: now.Add(time.Duration(i) * time.Millisecond),
		})
		if result.PromptDepth != i {
			t.Fatalf("prompt %d of a %d-way dispatch = depth %d, want %d (a live call was evicted)",
				i, parallel, result.PromptDepth, i)
		}
	}

	// Answered newest-first, which is the order that would expose an eviction of
	// the oldest: every answer but the last must hold the chip red.
	answeredAt := now.Add(time.Second)
	for i := parallel; i > 1; i-- {
		answeredAt = answeredAt.Add(time.Second)
		result := o.ApplyHook(HookSignal{
			Root: root, Event: "PostToolUse", ToolName: "Bash",
			ToolInputHash: "call-" + strconv.Itoa(i), At: answeredAt,
		})
		if result.Rule != "writer_tool_match_cleared" || result.PromptDepth != i-1 {
			t.Fatalf("answering call %d = rule %q depth %d, want %d still open",
				i, result.Rule, result.PromptDepth, i-1)
		}
		assertSummary(t, result.Observation, answeredAt, agentgraph.LegacyPermission, agentgraph.AttentionApproval)
	}

	answeredAt = answeredAt.Add(time.Second)
	result := o.ApplyHook(HookSignal{
		Root: root, Event: "PostToolUse", ToolName: "Bash",
		ToolInputHash: "call-1", At: answeredAt,
	})
	if result.PromptDepth != 0 || len(result.Projection.Pending) != 0 {
		t.Fatalf("last answer left depth %d projection %+v", result.PromptDepth, result.Projection)
	}
	assertSummary(t, result.Observation, answeredAt, agentgraph.LegacyWorking, agentgraph.AttentionNone)
}

// The 2026-08-05 invariant (docs/subagent-permission-plan.md) restated for the
// container: a writer routes evidence, so a teammate's PostToolUse may not close
// ANY of another writer's prompts — not the one it happens to match by tool and
// hash, and not the set. The single-prompt case is pinned by
// TestChildAttentionOwnershipAndWriterCollision; this is the parallel case, where
// a match-over-all-writers regression would take one call out of a set that must
// stay whole.
func TestTeammateHookCannotCloseAnyOfAnotherWritersParallelPrompts(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})

	for i, hash := range []string{"call-a", "call-b", "call-c"} {
		o.ApplyHook(HookSignal{
			Root: root, Event: "PermissionRequest", AgentID: "child-a", AgentType: "Explore",
			ToolName: "Bash", ToolInputHash: hash, At: now.Add(time.Duration(i+1) * time.Second),
		})
	}
	o.ApplyHook(HookSignal{
		Root: root, Event: "PermissionRequest", AgentID: "child-b",
		AgentType: "general-purpose", ToolName: "Bash", ToolInputHash: "call-b",
		At: now.Add(4 * time.Second),
	})

	// child-b answers its own byte-identical call. Only its own prompt closes.
	result := o.ApplyHook(HookSignal{
		Root: root, Event: "PostToolUse", AgentID: "child-b", ToolName: "Bash",
		ToolInputHash: "call-b", At: now.Add(5 * time.Second),
	})
	if result.Rule != "writer_tool_match_cleared" || result.PromptDepth != 0 {
		t.Fatalf("teammate's own answer = rule %q depth %d", result.Rule, result.PromptDepth)
	}
	if got := sortedPendingWriters(result.Projection.Pending); !reflect.DeepEqual(got, []string{"child-a"}) {
		t.Fatalf("pending writers after the teammate answered = %v, want child-a", got)
	}

	// The main thread answers the same call shape. It owns nothing, so nothing moves.
	result = o.ApplyHook(HookSignal{
		Root: root, Event: "PostToolUse", ToolName: "Bash",
		ToolInputHash: "call-b", At: now.Add(6 * time.Second),
	})
	if result.Rule != "non_owner_prompt_held" {
		t.Fatalf("non-owner answer = rule %q, want non_owner_prompt_held", result.Rule)
	}

	// child-a's set is untouched: all three calls still close, one at a time.
	for i, hash := range []string{"call-a", "call-b", "call-c"} {
		result = o.ApplyHook(HookSignal{
			Root: root, Event: "PostToolUse", AgentID: "child-a", ToolName: "Bash",
			ToolInputHash: hash, At: now.Add(time.Duration(7+i) * time.Second),
		})
		if result.Rule != "writer_tool_match_cleared" || result.PromptDepth != 2-i {
			t.Fatalf("child-a answering %q = rule %q depth %d, want %d still open",
				hash, result.Rule, result.PromptDepth, 2-i)
		}
	}
	if len(result.Projection.Pending) != 0 {
		t.Fatalf("owner's own answers did not empty its set: %+v", result.Projection.Pending)
	}
}

func TestMainToolMatchHoldsWithLiveTeammateAndHashMismatch(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})
	subdir := filepath.Join(filepath.Dir(root.Transcript), root.ProviderSessionID, "subagents")
	writeClaudeChild(t, subdir, "live", "general-purpose", "work", "", now)
	if _, err := o.Observe(context.Background(), root, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	o.ApplyHook(HookSignal{
		Root: root, Event: "PermissionRequest", ToolName: "Bash",
		ToolInputHash: "pending-call", At: now.Add(2 * time.Second),
	})
	result := o.ApplyHook(HookSignal{
		Root: root, Event: "PostToolUse", ToolName: "Bash",
		ToolInputHash: "pending-call", At: now.Add(3 * time.Second),
	})
	if result.Rule != "writer_prompt_held" || len(result.Projection.Pending) != 1 {
		t.Fatalf("unidentified writer with teammate = rule %q pending %+v, want hold", result.Rule, result.Projection.Pending)
	}
	assertSummary(t, result.Observation, now.Add(3*time.Second), agentgraph.LegacyPermission, agentgraph.AttentionApproval)

	// Even with an identified child writer, a changed input hash is ambiguous
	// (approval can rewrite input) and must await transcript evidence.
	o.ApplyHook(HookSignal{
		Root: root, Event: "PermissionRequest", AgentID: "live", ToolName: "Edit",
		ToolInputHash: "before", At: now.Add(4 * time.Second),
	})
	result = o.ApplyHook(HookSignal{
		Root: root, Event: "PostToolUse", AgentID: "live", ToolName: "Edit",
		ToolInputHash: "after", At: now.Add(5 * time.Second),
	})
	if _, ok := result.Projection.Pending["live"]; !ok {
		t.Fatalf("hash mismatch cleared child prompt: %+v", result.Projection.Pending)
	}
}

func TestObserveResolvesEachPromptAgainstItsWritersTranscript(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})
	subdir := filepath.Join(filepath.Dir(root.Transcript), root.ProviderSessionID, "subagents")
	writeClaudeChild(t, subdir, "child", "Explore", "work", "", now)
	if _, err := o.Observe(context.Background(), root, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	o.ApplyHook(HookSignal{Root: root, Event: "PermissionRequest", AgentID: "child", ToolName: "Bash", At: now.Add(2 * time.Second)})

	// Main-thread activity is deliberately irrelevant to a child-owned prompt.
	appendClaudeLine(t, root.Transcript, `{"type":"assistant","timestamp":"`+now.Add(3*time.Second).Format(time.RFC3339Nano)+`","message":{"role":"assistant","content":[]}}`)
	observation, err := o.Observe(context.Background(), root, now.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if graphNodesByID(observation.Nodes)["child"].Attention != agentgraph.AttentionApproval {
		t.Fatalf("main activity cleared child attention: %+v", observation.Nodes)
	}

	// The child's own resumed assistant entry resolves it and only it.
	childPath := filepath.Join(subdir, "agent-child.jsonl")
	appendClaudeLine(t, childPath, `{"type":"assistant","timestamp":"`+now.Add(5*time.Second).Format(time.RFC3339Nano)+`","message":{"role":"assistant","content":[]}}`)
	observation, err = o.Observe(context.Background(), root, now.Add(6*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if graphNodesByID(observation.Nodes)["child"].Attention != agentgraph.AttentionNone {
		t.Fatalf("child resume did not clear its attention: %+v", observation.Nodes)
	}
}

func TestObserveProjectsWorkflowProgressAndDrain(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})

	runID := "wf_adapter-1"
	runDir := filepath.Join(filepath.Dir(root.Transcript), root.ProviderSessionID, "subagents", "workflows", runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	scripts := filepath.Join(filepath.Dir(root.Transcript), root.ProviderSessionID, "workflows", "scripts")
	if err := os.MkdirAll(scripts, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scripts, "review-"+runID+".js"), []byte("export {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(runDir, "journal.jsonl")
	if err := os.WriteFile(journal, []byte(`{"type":"started","agentId":"workflow-child"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeWorkflowClaudeChild(t, runDir, "workflow-child", now)

	observation, err := o.Observe(context.Background(), root, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	node := graphNodesByID(observation.Nodes)["workflow-child"]
	if node.Role != "workflow-subagent" || node.Description != "review" || node.Lifecycle != agentgraph.LifecycleRunning {
		t.Fatalf("workflow node = %+v", node)
	}
	projection := o.Projection(root.Key())
	if projection.InFlightSubagents != 1 || len(projection.Workflows) != 1 || projection.Workflows[0].Name != "review" {
		t.Fatalf("workflow projection = %+v", projection)
	}

	appendClaudeLine(t, journal, `{"type":"result","agentId":"workflow-child","result":{"ok":true}}`)
	old := now.Add(-time.Hour)
	if err := os.Chtimes(journal, old, old); err != nil {
		t.Fatal(err)
	}
	observation, err = o.Observe(context.Background(), root, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if got := graphNodesByID(observation.Nodes)["workflow-child"].Lifecycle; got != agentgraph.LifecycleCompleted {
		t.Fatalf("drained workflow child lifecycle = %q", got)
	}
	if projection = o.Projection(root.Key()); projection.InFlightSubagents != 0 || len(projection.Workflows) != 0 {
		t.Fatalf("drained workflow projection = %+v", projection)
	}
}

func TestObserveProjectsStructuredFanoutAndPrunesOldTerminalCohorts(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})

	subdir := filepath.Join(filepath.Dir(root.Transcript), root.ProviderSessionID, "subagents")
	writeClaudeChild(t, subdir, "old", "Explore", "old work", "end_turn", now.Add(-time.Hour))
	writeClaudeChild(t, subdir, "live", "general-purpose", "current work", "", now)

	observation, err := o.Observe(context.Background(), root, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	summary := agentgraph.Reduce(observation, agentgraph.Summary{}, now.Add(time.Second))
	if summary.LegacyStatus != agentgraph.LegacyDelegating || summary.LiveChildren != 1 {
		t.Fatalf("fanout summary = %+v, want delegating with one live child", summary)
	}
	byID := graphNodesByID(observation.Nodes)
	if _, ok := byID["live"]; !ok {
		t.Fatalf("live child missing: %+v", observation.Nodes)
	}
	if _, ok := byID["old"]; !ok {
		t.Fatalf("most-recent terminal cohort should be retained on first observation: %+v", observation.Nodes)
	}
	if events := o.DrainLegacyEvents(root.Key()); len(events) != 3 {
		t.Fatalf("legacy fanout events = %+v, want two spawns and old's stop", events)
	}
	if events := o.DrainLegacyEvents(root.Key()); len(events) != 0 {
		t.Fatalf("legacy fanout events drained twice: %+v", events)
	}

	// A new parent turn replaces the retained terminal cohort. Append-only old
	// artifacts must not accumulate forever in the live graph.
	o.ApplyHook(HookSignal{Root: root, Event: "UserPromptSubmit", At: now.Add(2 * time.Second)})
	writeClaudeChild(t, subdir, "current-done", "Explore", "new work", "end_turn", now.Add(3*time.Second))
	observation, err = o.Observe(context.Background(), root, now.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	byID = graphNodesByID(observation.Nodes)
	if _, ok := byID["old"]; ok {
		t.Fatalf("old terminal survived new turn: %+v", observation.Nodes)
	}
	if _, ok := byID["current-done"]; !ok {
		t.Fatalf("current terminal missing: %+v", observation.Nodes)
	}
}

func TestFirstObservationUsesKnownParentTurnBoundaryForTerminalChildren(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	o.ApplyHook(HookSignal{Root: root, Event: "UserPromptSubmit", At: now})
	subdir := filepath.Join(filepath.Dir(root.Transcript), root.ProviderSessionID, "subagents")
	writeClaudeChild(t, subdir, "historical", "Explore", "old", "end_turn", now.Add(-time.Hour))
	writeClaudeChild(t, subdir, "current-a", "Explore", "new a", "end_turn", now.Add(time.Second))
	writeClaudeChild(t, subdir, "current-b", "Explore", "new b", "end_turn", now.Add(2*time.Second))

	observation, err := o.Observe(context.Background(), root, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	byID := graphNodesByID(observation.Nodes)
	if _, ok := byID["historical"]; ok {
		t.Fatalf("pre-turn terminal retained: %+v", observation.Nodes)
	}
	for _, id := range []string{"current-a", "current-b"} {
		if _, ok := byID[id]; !ok {
			t.Fatalf("current-turn terminal %q missing: %+v", id, observation.Nodes)
		}
	}
}

func TestObserveReturnsDeepCopiesAndLifecycleMethodsAreIdempotent(t *testing.T) {
	o, root, now := newTestObserver(t)
	o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})
	first, err := o.Observe(context.Background(), root, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	first.Nodes[0].ID = "mutated"
	second, err := o.Observe(context.Background(), root, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if second.RootID != root.ProviderSessionID || second.Nodes[0].ID != root.ProviderSessionID {
		t.Fatalf("caller mutation reached observer cache: %+v", second)
	}

	o.Forget(root.Key())
	o.Forget(root.Key())
	if got := o.Projection(root.Key()); got.SessionID != "" {
		t.Fatalf("forgotten projection = %+v", got)
	}
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Observe(context.Background(), root, now); err != ErrClosed {
		t.Fatalf("Observe after Close error = %v, want ErrClosed", err)
	}
}

func TestOlderObservationCannotOverwriteNewerHookState(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})
	if _, err := o.Observe(context.Background(), root, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	newer := o.ApplyHook(HookSignal{
		Root: root, Event: "PermissionRequest", AgentID: "child",
		ToolName: "Bash", At: now.Add(4 * time.Second),
	})
	late, err := o.Observe(context.Background(), root, now.Add(3*time.Second))
	if !errors.Is(err, ErrSuperseded) {
		t.Fatalf("late observation error = %v, want ErrSuperseded", err)
	}
	if got := graphNodesByID(late.Nodes)["child"].Attention; got != agentgraph.AttentionApproval {
		t.Fatalf("late observation overwrote newer attention: %q; newer=%+v late=%+v", got, newer.Observation, late)
	}
	if _, ok := o.Projection(root.Key()).Pending["child"]; !ok {
		t.Fatal("late observation removed newer pending writer")
	}
}

func TestRestorePreservesPersistedWriterOwnershipWithoutInventingCorrelators(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	statusSince := now.Add(-time.Minute)
	observation, err := o.Restore(root, Compatibility{
		Status: agentgraph.LegacyPermission, StatusSince: statusSince,
		PendingWriters:    []string{"child", PendingWriterMain},
		InFlightSubagents: 2,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if observation.Source != agentgraph.SourceRestoredLastKnown || observation.Complete {
		t.Fatalf("restored observation metadata = %+v", observation)
	}
	projection := o.Projection(root.Key())
	if projection.Status != agentgraph.LegacyPermission || projection.StatusSince != statusSince || projection.InFlightSubagents != 2 {
		t.Fatalf("restored projection = %+v", projection)
	}
	if got := sortedPendingWriters(projection.Pending); !reflect.DeepEqual(got, []string{"", "child"}) {
		t.Fatalf("restored writers = %q", got)
	}
	for writer, pending := range projection.Pending {
		if pending.Tool != "" || pending.InputHash != "" || pending.Attention != agentgraph.AttentionApproval || !pending.Since.Equal(now) {
			t.Errorf("restored pending[%q] = %+v", writer, pending)
		}
	}
	same := o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now.Add(time.Second)}).Projection
	if !same.StatusSince.Equal(statusSince) {
		t.Fatalf("same reduced state moved restored StatusSince from %v to %v", statusSince, same.StatusSince)
	}
}

func TestExactSessionRotationRetiresOldPromptOwnership(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	o.ApplyHook(HookSignal{Root: root, Event: "PermissionRequest", AgentID: "child", ToolName: "Bash", At: now})
	rotated := root
	rotated.ProviderSessionID = "claude-root-rotated"
	rotated.Transcript = filepath.Join(filepath.Dir(root.Transcript), rotated.ProviderSessionID+".jsonl")
	if err := os.WriteFile(rotated.Transcript, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	result := o.ApplyHook(HookSignal{Root: rotated, Event: "SessionStart", At: now.Add(time.Second)})
	if result.Observation.RootID != rotated.ProviderSessionID || len(result.Projection.Pending) != 0 || result.Projection.SessionID != rotated.ProviderSessionID {
		t.Fatalf("rotation retained old state: %+v", result)
	}
}

func TestObserverConcurrentApplyObserveForgetClose(t *testing.T) {
	o, root, now := newTestObserver(t)
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				at := now.Add(time.Duration(worker*100+i) * time.Microsecond)
				o.ApplyHook(HookSignal{Root: root, Event: "UserPromptSubmit", At: at})
				_, _ = o.Observe(context.Background(), root, at)
				_ = o.Projection(root.Key())
				if i%25 == 0 {
					o.Forget(provider.RootKey{PID: root.PID + worker + 1, StartedAt: root.StartedAt})
				}
			}
		}(worker)
	}
	wg.Wait()
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
}

func newTestObserver(t *testing.T) (*Observer, provider.RootRef, time.Time) {
	t.Helper()
	base := t.TempDir()
	sid := "claude-root-123"
	transcriptPath := filepath.Join(base, sid+".jsonl")
	if err := os.WriteFile(transcriptPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, sid, "subagents"), 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	root := provider.RootRef{
		PID: 42, StartedAt: now.Add(-time.Hour), Provider: agentgraph.ProviderClaude,
		ProviderSessionID: sid, Transcript: transcriptPath, CWD: "/project",
	}
	return NewObserver(t.TempDir(), WithFreshness(time.Minute)), root, now
}

func writeClaudeChild(t *testing.T, subdir, id, role, description, stopReason string, mod time.Time) {
	t.Helper()
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := []byte(`{"agentType":"` + role + `","description":"` + description + `","spawnDepth":1}`)
	metaPath := filepath.Join(subdir, "agent-"+id+".meta.json")
	if err := os.WriteFile(metaPath, meta, 0o644); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"assistant","message":{"role":"assistant","stop_reason":null}}` + "\n"
	if stopReason != "" {
		line = `{"type":"assistant","message":{"role":"assistant","stop_reason":"` + stopReason + `"}}` + "\n"
	}
	jsonlPath := filepath.Join(subdir, "agent-"+id+".jsonl")
	if err := os.WriteFile(jsonlPath, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(metaPath, mod, mod); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(jsonlPath, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func writeWorkflowClaudeChild(t *testing.T, runDir, id string, mod time.Time) {
	t.Helper()
	metaPath := filepath.Join(runDir, "agent-"+id+".meta.json")
	if err := os.WriteFile(metaPath, []byte(`{"agentType":"workflow-subagent","spawnDepth":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	jsonlPath := filepath.Join(runDir, "agent-"+id+".jsonl")
	if err := os.WriteFile(jsonlPath, []byte(`{"type":"assistant","message":{"role":"assistant","stop_reason":null}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{metaPath, jsonlPath} {
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
}

func appendClaudeLine(t *testing.T, path, line string) {
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

func graphNodesByID(nodes []agentgraph.Node) map[string]agentgraph.Node {
	byID := make(map[string]agentgraph.Node, len(nodes))
	for _, node := range nodes {
		byID[node.ID] = node
	}
	return byID
}

func assertSummary(t *testing.T, observation agentgraph.Observation, now time.Time, legacy string, attention agentgraph.AttentionState) {
	t.Helper()
	got := agentgraph.Reduce(observation, agentgraph.Summary{}, now)
	if got.LegacyStatus != legacy || got.Attention != attention {
		t.Fatalf("summary = %+v, want status=%q attention=%q; observation=%+v", got, legacy, attention, observation)
	}
}
