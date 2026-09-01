package claude

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/provider"
	"github.com/tjmisko/switchboard/internal/statustune"
)

// The headline of Phase 4 (L1, the measured 17 s stale red). AskUserQuestion's
// PostToolUse input is a strict superset of the input its PermissionRequest
// carried, so the correlator hash NEVER matches and the hook-speed clear was
// unreachable for exactly the tool users wait on longest
// (askuserquestion-model-plan.md §1). Once the prompt has latched the call id
// off the writer's own transcript, the id names the same call whatever the hash
// did, and the red clears on the hook instead of on a transcript tick.
func TestQuestionWithRewrittenInputShouldClearAtHookSpeedWhenTheCallIDMatches(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})
	o.ApplyHook(HookSignal{
		Root: root, Event: "PermissionRequest", ToolName: "AskUserQuestion",
		ToolInputHash: "ask-before", At: now.Add(time.Second),
	})

	// PermissionRequest carries no tool_use_id (docs/claude-code-hook-schema.md
	// §2), so the id can only come from the transcript — where the pending
	// tool_use lands ~5 s after the hook and stays unmatched for the whole wait.
	// The Observe ticks after that flush are what bind it.
	writeToolUseLine(t, root.Transcript, dispatchedFor(now.Add(time.Second)), "AskUserQuestion", "toolu_ask")
	observeUntilLatched(t, o, root, now.Add(6*time.Second))

	result := o.ApplyHook(HookSignal{
		Root: root, Event: "PostToolUse", ToolName: "AskUserQuestion",
		ToolInputHash: "ask-after", ToolUseID: "toolu_ask", At: now.Add(9 * time.Second),
	})
	if result.Rule != statustune.RuleGraphCallMatchCleared || result.PromptDepth != 0 {
		t.Fatalf("answered question with a rewritten input = rule %q depth %d, want an id-matched clear",
			result.Rule, result.PromptDepth)
	}
	assertSummary(t, result.Observation, now.Add(9*time.Second), agentgraph.LegacyWorking, agentgraph.AttentionNone)
}

// An id mismatch is a REAL negative, unlike a hash mismatch: the two ids say
// outright that this completion is a different call. A sibling auto-approved
// call finishing while the question waits must therefore hold the red — and it
// must be held under its own rule id, so a diagnosis can tell "the correlator
// cannot name the call" from "it can, and this is not it".
func TestPromptShouldHoldWhenTheCompletionNamesADifferentCall(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})
	o.ApplyHook(HookSignal{
		Root: root, Event: "PermissionRequest", ToolName: "AskUserQuestion",
		ToolInputHash: "ask-before", At: now.Add(time.Second),
	})
	writeToolUseLine(t, root.Transcript, dispatchedFor(now.Add(time.Second)), "AskUserQuestion", "toolu_ask")
	observeUntilLatched(t, o, root, now.Add(6*time.Second))

	// The very hash the prompt was opened with, on a different call. Under the
	// shape rule alone this clears; under identity it cannot.
	result := o.ApplyHook(HookSignal{
		Root: root, Event: "PostToolUse", ToolName: "AskUserQuestion",
		ToolInputHash: "ask-before", ToolUseID: "toolu_sibling", At: now.Add(9 * time.Second),
	})
	if result.Rule != statustune.RuleGraphCallMismatchHeld || result.PromptDepth != 1 {
		t.Fatalf("sibling call completion = rule %q depth %d, want the red held", result.Rule, result.PromptDepth)
	}
	assertSummary(t, result.Observation, now.Add(9*time.Second), agentgraph.LegacyPermission, agentgraph.AttentionUserInput)
}

// Two unmatched calls of one tool are indistinguishable in the file, so the
// prompt must stay unbound rather than pick one: a wrong bind lets the sibling's
// own result clear a prompt nobody answered, which is a missed RED. The state is
// terminal — the candidate set shrinks as siblings complete, but nothing records
// WHICH one shrank, so a later unique read is exactly as likely to name the
// sibling.
func TestPromptShouldStayUnboundForeverWhenTheWritersTailHoldsTwoCallsOfTheSameTool(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})
	o.ApplyHook(HookSignal{
		Root: root, Event: "PermissionRequest", ToolName: "AskUserQuestion",
		ToolInputHash: "ask-before", At: now.Add(time.Second),
	})
	writeToolUseLine(t, root.Transcript, dispatchedFor(now.Add(time.Second)), "AskUserQuestion", "toolu_ask_a")
	writeToolUseLine(t, root.Transcript, dispatchedFor(now.Add(time.Second)), "AskUserQuestion", "toolu_ask_b")
	if _, err := o.Observe(context.Background(), root, now.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := o.DrainPromptDiagnostics(root.Key()); !slices.Contains(got, DiagnosticCallAmbiguous) {
		t.Fatalf("latch diagnostics = %v, want the ambiguity counted", got)
	}
	if prompt := o.Projection(root.Key()).Pending[""]; prompt.Latch != CallLatchAmbiguous || prompt.CallID != "" {
		t.Fatalf("ambiguous prompt = %+v, want no id and a terminal latch state", prompt)
	}

	// One candidate is answered, so the tail now names exactly one unmatched
	// call. It must STILL not bind: nothing says the survivor is this prompt's.
	writeToolResultLine(t, root.Transcript, now.Add(7*time.Second), "toolu_ask_a", "", false)
	if _, err := o.Observe(context.Background(), root, now.Add(8*time.Second)); err != nil {
		t.Fatal(err)
	}
	if prompt := o.Projection(root.Key()).Pending[""]; prompt.Latch != CallLatchAmbiguous || prompt.CallID != "" {
		t.Fatalf("prompt re-latched after the tail narrowed: %+v", prompt)
	}
	if got := o.DrainPromptDiagnostics(root.Key()); len(got) != 0 {
		t.Fatalf("terminal ambiguity re-counted itself: %v", got)
	}
}

// The tool_use reaches disk ~5 s after the hook, so the first tick after a
// prompt opens usually finds nothing. That is not ambiguity and must be retried,
// or the fast path would only ever work for a prompt that raced its own flush.
// The tick that first names a candidate only PROPOSES it; a second tick reading
// the same tail is what binds.
func TestPromptShouldLatchOnALaterTickWhenTheToolUseHasNotFlushedYet(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})
	o.ApplyHook(HookSignal{
		Root: root, Event: "PermissionRequest", ToolName: "AskUserQuestion",
		ToolInputHash: "ask-before", At: now.Add(time.Second),
	})
	if _, err := o.Observe(context.Background(), root, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if prompt := o.Projection(root.Key()).Pending[""]; prompt.Latch != CallLatchUnbound {
		t.Fatalf("pre-flush prompt = %+v, want it still unbound and retryable", prompt)
	}
	if got := o.DrainPromptDiagnostics(root.Key()); len(got) != 0 {
		t.Fatalf("an unflushed tool_use was counted as an outcome: %v", got)
	}

	writeToolUseLine(t, root.Transcript, dispatchedFor(now.Add(time.Second)), "AskUserQuestion", "toolu_ask")
	if _, err := o.Observe(context.Background(), root, now.Add(7*time.Second)); err != nil {
		t.Fatal(err)
	}
	prompt := o.Projection(root.Key()).Pending[""]
	if prompt.Latch != CallLatchProposed || prompt.CallID != "toolu_ask" {
		t.Fatalf("first post-flush tick = %+v, want toolu_ask proposed and not yet bound", prompt)
	}
	if got := o.DrainPromptDiagnostics(root.Key()); len(got) != 0 {
		t.Fatalf("a proposal was counted as an outcome: %v", got)
	}

	if _, err := o.Observe(context.Background(), root, now.Add(7*time.Second).Add(callLatchConfirmGrace)); err != nil {
		t.Fatal(err)
	}
	prompt = o.Projection(root.Key()).Pending[""]
	if prompt.Latch != CallLatchBound || prompt.CallID != "toolu_ask" {
		t.Fatalf("confirming tick = %+v, want it bound to toolu_ask", prompt)
	}
	if got := o.DrainPromptDiagnostics(root.Key()); !slices.Contains(got, DiagnosticCallLatched) {
		t.Fatalf("latch diagnostics = %v, want the bind counted once", got)
	}
}

// The confirmation the proposal exists for is a LATER VIEW of the file, not a
// second function call. ApplyHook signals the coordinator, so every hook edge
// from any writer schedules an Observe for this root, and a fanned-out session
// delivers them in bursts: two reads can land milliseconds apart, inside the
// 0.5–1.5 s gap between one assistant message's parallel tool_use entries. Both
// then see the gated call's auto-approved sibling ALONE and agree — for the same
// wrong reason — and a bind off that agreement is the missed RED the whole
// two-read rule exists to prevent, because the sibling's own result would clear
// a prompt nobody answered.
func TestLatchShouldRefuseAConfirmingReadTakenInsideTheFlushGap(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})
	gatedAt := now.Add(time.Second)
	o.ApplyHook(HookSignal{
		Root: root, Event: "PermissionRequest", ToolName: "Bash",
		ToolInputHash: "run-tests", At: gatedAt,
	})

	// Only the auto-approved sibling has reached disk; the gated call's own entry
	// is still inside the message's inter-entry gap.
	writeToolUseLine(t, root.Transcript, dispatchedFor(gatedAt), "Bash", "toolu_sibling")
	proposedAt := gatedAt.Add(5 * time.Second)
	if _, err := o.Observe(context.Background(), root, proposedAt); err != nil {
		t.Fatal(err)
	}
	if prompt := o.Projection(root.Key()).Pending[""]; prompt.Latch != CallLatchProposed {
		t.Fatalf("first read = %+v, want the sibling only proposed", prompt)
	}

	// A teammate's hook wakes the coordinator a few milliseconds later. Same file,
	// same answer, no new evidence.
	if _, err := o.Observe(context.Background(), root, proposedAt.Add(20*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	prompt := o.Projection(root.Key()).Pending[""]
	if prompt.Latch != CallLatchProposed || prompt.CallID != "toolu_sibling" {
		t.Fatalf("burst read = %+v, want the proposal neither confirmed nor withdrawn", prompt)
	}
	if got := o.DrainPromptDiagnostics(root.Key()); len(got) != 0 {
		t.Fatalf("a read inside the grace was counted as an outcome: %v", got)
	}

	// The gated entry lands before any read the grace allows, so the later read
	// sees both calls and withdraws the proposal as ambiguous.
	writeToolUseLine(t, root.Transcript, dispatchedFor(gatedAt), "Bash", "toolu_gated")
	if _, err := o.Observe(context.Background(), root, proposedAt.Add(callLatchConfirmGrace)); err != nil {
		t.Fatal(err)
	}
	if prompt := o.Projection(root.Key()).Pending[""]; prompt.Latch != CallLatchAmbiguous || prompt.CallID != "" {
		t.Fatalf("post-grace read = %+v, want the proposal withdrawn as ambiguous", prompt)
	}

	// The proof that matters: the sibling's own result cannot clear the red.
	writeToolResultLine(t, root.Transcript, gatedAt.Add(9*time.Second), "toolu_sibling", "ok", false)
	observation, err := o.Observe(context.Background(), root, gatedAt.Add(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	assertSummary(t, observation, gatedAt.Add(10*time.Second), agentgraph.LegacyPermission, agentgraph.AttentionApproval)
}

// A restored record stands for a writer's residual RED, not for one call: the
// persisted block carries one prompt per writer, so a writer that went down
// blocked on three calls comes back holding one. Binding an id to it would let
// that one call's completion clear a red two real calls are still holding — a
// missed RED manufactured by the restart itself.
func TestRestoredPromptShouldNeverBindACallID(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	restored := Compatibility{
		SessionID: root.ProviderSessionID, Transcript: root.Transcript,
		Status: agentgraph.LegacyPermission, StatusSince: now,
		Pending: map[string]PendingPrompt{"": {Tool: "AskUserQuestion", InputHash: "ask-before", Since: now}},
	}
	if _, err := o.Restore(root, restored, now); err != nil {
		t.Fatal(err)
	}
	writeToolUseLine(t, root.Transcript, dispatchedFor(now), "AskUserQuestion", "toolu_ask")
	observeUntilLatched(t, o, root, now.Add(6*time.Second))
	if prompt := o.Projection(root.Key()).Pending[""]; prompt.Latch != CallLatchUnbound || prompt.CallID != "" {
		t.Fatalf("restored prompt = %+v, want no call identity bound to it", prompt)
	}

	result := o.ApplyHook(HookSignal{
		Root: root, Event: "PostToolUse", ToolName: "AskUserQuestion",
		ToolInputHash: "ask-after", ToolUseID: "toolu_ask", At: now.Add(7 * time.Second),
	})
	if result.PromptDepth != 1 {
		t.Fatalf("an id cleared a restored red: depth %d, want the prompt held", result.PromptDepth)
	}
	assertSummary(t, result.Observation, now.Add(7*time.Second), agentgraph.LegacyPermission, agentgraph.AttentionUserInput)
}

// The second thing the id buys: an answer clears the red even when no
// PostToolUse hook arrives, one tick later instead of when the whole file
// advances. It must close exactly the answered call — the writer's other open
// call is still blocking, and blanket whole-file evidence cannot say that.
func TestAnsweredCallShouldClearOnlyItsOwnPromptWhenTheHookNeverArrives(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})
	o.ApplyHook(HookSignal{
		Root: root, Event: "PermissionRequest", ToolName: "AskUserQuestion",
		ToolInputHash: "ask-before", At: now.Add(time.Second),
	})
	o.ApplyHook(HookSignal{
		Root: root, Event: "PermissionRequest", ToolName: "Bash",
		ToolInputHash: "run-tests", At: now.Add(2 * time.Second),
	})
	writeToolUseLine(t, root.Transcript, dispatchedFor(now.Add(time.Second)), "AskUserQuestion", "toolu_ask")
	writeToolUseLine(t, root.Transcript, dispatchedFor(now.Add(2*time.Second)), "Bash", "toolu_bash")
	observeUntilLatched(t, o, root, now.Add(6*time.Second))

	// The question's own result lands 20–101 ms after the keypress. The Bash gate
	// is untouched, so the chip stays red under the approval it still holds.
	writeToolResultLine(t, root.Transcript, now.Add(9*time.Second), "toolu_ask", "chose option A", false)
	observation, err := o.Observe(context.Background(), root, now.Add(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if rule := o.DrainResolutionRule(root.Key()); rule != statustune.RuleGraphCallResolved {
		t.Fatalf("resolution rule = %q, want the id-matched clear", rule)
	}
	pending := o.Projection(root.Key()).Pending
	if len(pending) != 1 || pending[""].Tool != "Bash" {
		t.Fatalf("pending after the question was answered = %+v, want the Bash gate alone", pending)
	}
	assertSummary(t, observation, now.Add(10*time.Second), agentgraph.LegacyPermission, agentgraph.AttentionApproval)
}

// A declined call returns control to the user, so the chip exits to idle. An
// APPROVED call that merely failed did not: the turn resumed, and reading
// is_error alone as a decline would send every failing command to orange.
func TestDeclinedCallShouldExitToIdleWhileAFailedOneResumes(t *testing.T) {
	for name, tc := range map[string]struct {
		declined bool
		rule     string
		legacy   string
	}{
		"a user rejection": {true, statustune.RuleGraphCallDeclined, agentgraph.LegacyIdle},
		"a failed tool":    {false, statustune.RuleGraphCallResolved, agentgraph.LegacyWorking},
	} {
		t.Run("should exit to "+tc.legacy+" when the answer is "+name, func(t *testing.T) {
			o, root, now := newTestObserver(t)
			defer o.Close()
			o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})
			o.ApplyHook(HookSignal{
				Root: root, Event: "PermissionRequest", ToolName: "AskUserQuestion",
				ToolInputHash: "ask-before", At: now.Add(time.Second),
			})
			writeToolUseLine(t, root.Transcript, dispatchedFor(now.Add(time.Second)), "AskUserQuestion", "toolu_ask")
			observeUntilLatched(t, o, root, now.Add(6*time.Second))

			if tc.declined {
				writeDeclineLine(t, root.Transcript, now.Add(9*time.Second), "toolu_ask")
			} else {
				writeToolResultLine(t, root.Transcript, now.Add(9*time.Second), "toolu_ask", "Error: Exit code 1", true)
			}
			observation, err := o.Observe(context.Background(), root, now.Add(10*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if rule := o.DrainResolutionRule(root.Key()); rule != tc.rule {
				t.Fatalf("resolution rule = %q, want %q", rule, tc.rule)
			}
			assertSummary(t, observation, now.Add(10*time.Second), tc.legacy, agentgraph.AttentionNone)
		})
	}
}

// The oldest-match tiebreak is for SHAPES, which are guesses. An id is not, so
// it wins wherever it sits in the set — a writer that went ambiguous on one call
// and later identified another must resolve the one the completion names. Taking
// the older shape instead would close a prompt the signal said nothing about,
// leave the answered call red, and (with teammates in flight) hit the fanout
// floor on a prompt that has no id to lift it.
func TestExactCallMatchShouldOutrankAnOlderShapeMatchInTheSameSet(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	prompts := []PendingPrompt{
		{Tool: "Bash", InputHash: "same-shape", Since: now, Latch: CallLatchAmbiguous},
		{Tool: "Bash", InputHash: "other-shape", Since: now.Add(time.Second), CallID: "toolu_bash_b", Latch: CallLatchBound},
	}
	signal := HookSignal{
		Event: "PostToolUse", ToolName: "Bash",
		ToolInputHash: "same-shape", ToolUseID: "toolu_bash_b",
	}
	if got := matchingPromptIndex(prompts, signal); got != 1 {
		t.Fatalf("matched prompt %d, want the identified one at 1", got)
	}

	// With no id on the signal, the oldest shape match is still the answer.
	if got := matchingPromptIndex(prompts, HookSignal{
		Event: "PostToolUse", ToolName: "Bash", ToolInputHash: "same-shape",
	}); got != 0 {
		t.Fatalf("shape-only match = %d, want the oldest at 0", got)
	}
}

// The fanout floor exists because an empty agent_id with teammates in flight may
// be the main thread OR a teammate whose hook lost its writer, and a (tool, hash)
// match cannot separate them — the hash names a call SHAPE and teammates run
// byte-identical commands routinely. A call id can, so the floor applies to shape
// matches only. Both halves are pinned here: lifting it for shapes too would be
// the 2026-08-05 lost-RED regression.
func TestIDMatchedClearShouldLiftTheFanoutFloorWhileAShapeMatchStillHolds(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})
	subdir := filepath.Join(filepath.Dir(root.Transcript), root.ProviderSessionID, "subagents")
	writeClaudeChild(t, subdir, "live", "general-purpose", "work", "", now)
	if _, err := o.Observe(context.Background(), root, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	o.ApplyHook(HookSignal{
		Root: root, Event: "PermissionRequest", ToolName: "AskUserQuestion",
		ToolInputHash: "ask-before", At: now.Add(2 * time.Second),
	})
	// A shape match from an unidentifiable writer, with a teammate live. Held.
	result := o.ApplyHook(HookSignal{
		Root: root, Event: "PostToolUse", ToolName: "AskUserQuestion",
		ToolInputHash: "ask-before", At: now.Add(3 * time.Second),
	})
	if result.Rule != statustune.RuleGraphPromptHeld || result.PromptDepth != 1 {
		t.Fatalf("shape match under the floor = rule %q depth %d, want it held", result.Rule, result.PromptDepth)
	}

	writeToolUseLine(t, root.Transcript, dispatchedFor(now.Add(2*time.Second)), "AskUserQuestion", "toolu_ask")
	observeUntilLatched(t, o, root, now.Add(6*time.Second))
	if o.Projection(root.Key()).InFlightSubagents == 0 {
		t.Fatal("fixture no longer has a teammate in flight, so the floor is not under test")
	}
	result = o.ApplyHook(HookSignal{
		Root: root, Event: "PostToolUse", ToolName: "AskUserQuestion",
		ToolInputHash: "ask-after", ToolUseID: "toolu_ask", At: now.Add(9 * time.Second),
	})
	if result.Rule != statustune.RuleGraphCallMatchCleared || result.PromptDepth != 0 {
		t.Fatalf("id match under the floor = rule %q depth %d, want it cleared", result.Rule, result.PromptDepth)
	}
}

// P3's gate (askuserquestion-model-plan.md §5), and the precondition for the
// lifted floor above. The fixture is the realistic multi-writer shape, including
// the ONE duplication the corpus actually contains: a subagent's own file opens
// with a copy of the parent's launching Agent tool_use, so that id appears in two
// files. Measured over ~/.claude/projects on 2026-08-31 — 2361 sessions, 3963
// transcript files, 99,893 distinct tool_use ids — it is the only intra-session
// duplicate there is, and it is MATCHED in both files (the spawn ack answers it
// in each), so it is never a latch candidate at all.
func TestNoCallIDShouldBeClaimedByTwoWritersAcrossOneSession(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})
	subdir := filepath.Join(filepath.Dir(root.Transcript), root.ProviderSessionID, "subagents")
	writeClaudeChild(t, subdir, "child-a", "Explore", "work", "", now)
	writeClaudeChild(t, subdir, "child-b", "general-purpose", "work", "", now)
	if _, err := o.Observe(context.Background(), root, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	// Main thread: one gated call still waiting, one auto-approved sibling that
	// came back, and the Agent spawn whose ack has landed.
	writeToolUseLine(t, root.Transcript, dispatchedFor(now.Add(2*time.Second)), "Bash", "toolu_main_gated")
	writeToolUseLine(t, root.Transcript, now, "Read", "toolu_main_read")
	writeToolResultLine(t, root.Transcript, now, "toolu_main_read", "contents", false)
	writeToolUseLine(t, root.Transcript, now, "Agent", "toolu_spawn")
	writeToolResultLine(t, root.Transcript, now, "toolu_spawn", "Spawned successfully", false)

	// child-a's file opens with the parent's launching tool_use replayed, matched
	// by its own copy of the ack, then dispatches its own gated question.
	childA := filepath.Join(subdir, "agent-child-a.jsonl")
	writeToolUseLine(t, childA, now, "Agent", "toolu_spawn")
	writeToolResultLine(t, childA, now, "toolu_spawn", "Spawned successfully", false)
	writeToolUseLine(t, childA, dispatchedFor(now.Add(3*time.Second)), "AskUserQuestion", "toolu_a_question")

	childB := filepath.Join(subdir, "agent-child-b.jsonl")
	writeToolUseLine(t, childB, dispatchedFor(now.Add(4*time.Second)), "Bash", "toolu_b_gated")

	for i, writer := range []string{"", "child-a", "child-b"} {
		tool := "Bash"
		if writer == "child-a" {
			tool = "AskUserQuestion"
		}
		o.ApplyHook(HookSignal{
			Root: root, Event: "PermissionRequest", AgentID: writer, ToolName: tool,
			ToolInputHash: "shape-" + writer, At: now.Add(time.Duration(i+2) * time.Second),
		})
	}
	observeUntilLatched(t, o, root, now.Add(10*time.Second))

	diagnostics := o.DrainPromptDiagnostics(root.Key())
	if slices.Contains(diagnostics, DiagnosticCallIDCollision) {
		t.Fatalf("a call id was claimed by two writers: %v", diagnostics)
	}
	pending := o.Projection(root.Key()).Pending
	want := map[string]string{"": "toolu_main_gated", "child-a": "toolu_a_question", "child-b": "toolu_b_gated"}
	claimed := map[string]string{}
	for writer, id := range want {
		prompt := pending[writer]
		if prompt.Latch != CallLatchBound || prompt.CallID != id {
			t.Fatalf("writer %q bound %+v, want %q", writer, prompt, id)
		}
		if other, taken := claimed[prompt.CallID]; taken {
			t.Fatalf("call %q is bound to both %q and %q", prompt.CallID, other, writer)
		}
		claimed[prompt.CallID] = writer
	}
}

// The runtime half of the same gate. If the uniqueness above ever fails — the
// spawn echo unmatched in both files while both writers wait on that tool — the
// latch must refuse the id and count it, not pick a writer. Guessing here is the
// exact missed RED the lifted floor would otherwise let through.
func TestLatchShouldRefuseAndCountACallIDTwoWritersClaim(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})
	subdir := filepath.Join(filepath.Dir(root.Transcript), root.ProviderSessionID, "subagents")
	writeClaudeChild(t, subdir, "child-a", "Explore", "work", "", now)
	if _, err := o.Observe(context.Background(), root, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	// One instant for both prompts, because one id has to be plausibly each
	// writer's own: the collision under test is about identity, and dating the
	// shared call before one prompt and after the other would let the age rule
	// decide it instead.
	gatedAt := now.Add(3 * time.Second)
	writeToolUseLine(t, root.Transcript, dispatchedFor(gatedAt), "AskUserQuestion", "toolu_shared")
	writeToolUseLine(t, filepath.Join(subdir, "agent-child-a.jsonl"), dispatchedFor(gatedAt), "AskUserQuestion", "toolu_shared")
	for _, writer := range []string{"", "child-a"} {
		o.ApplyHook(HookSignal{
			Root: root, Event: "PermissionRequest", AgentID: writer, ToolName: "AskUserQuestion",
			ToolInputHash: "shape-" + writer, At: gatedAt,
		})
	}
	if _, err := o.Observe(context.Background(), root, now.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}

	if got := o.DrainPromptDiagnostics(root.Key()); !slices.Contains(got, DiagnosticCallIDCollision) {
		t.Fatalf("latch diagnostics = %v, want the collision counted", got)
	}
	for _, writer := range []string{"", "child-a"} {
		if prompt := o.Projection(root.Key()).Pending[writer]; prompt.Latch != CallLatchContested || prompt.CallID != "" {
			t.Fatalf("writer %q bound a contested call: %+v", writer, prompt)
		}
	}

	// ONE event, counted once. The contested state is terminal for the same reason
	// ambiguity is, and that is what keeps this counter comparable with
	// prompt_call_latched: a level counter ticking for every second of a 4-minute
	// wait would report one collision as ~48 and read as systemic failure.
	if _, err := o.Observe(context.Background(), root, now.Add(11*time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := o.DrainPromptDiagnostics(root.Key()); len(got) != 0 {
		t.Fatalf("a terminal collision re-counted itself: %v", got)
	}
}

// The FIRST latch attempt for every prompt runs milliseconds after the hook —
// ApplyHook signals the coordinator, which reconciles that root at once — and
// that is ~5 s before the prompt's own tool_use can reach disk. Every candidate
// visible at that instant is by construction some OTHER call, so a bind there is
// guaranteed wrong, and it is a missed RED rather than a stale one: the sibling
// comes back clean, its id-matched result closes the prompt, and the chip goes
// green while the user is still sitting at the approval dialog.
func TestLatchShouldRefuseACallThatWasAlreadyRunningWhenThePromptOpened(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})

	// An auto-approved Bash dispatched earlier in the turn, still executing and
	// still unmatched at EOF. The corpus makes this the COMMON case, not a corner
	// one: 1554 of the measured Bash dispatch moments had exactly one such call on
	// disk against 244 with two or more.
	writeToolUseLine(t, root.Transcript, now, "Bash", "toolu_sibling")
	gatedAt := now.Add(30 * time.Second)
	o.ApplyHook(HookSignal{
		Root: root, Event: "PermissionRequest", ToolName: "Bash",
		ToolInputHash: "run-tests", At: gatedAt,
	})

	if _, err := o.Observe(context.Background(), root, gatedAt.Add(100*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if prompt := o.Projection(root.Key()).Pending[""]; prompt.CallID != "" {
		t.Fatalf("post-hook tick = %+v, want nothing bound — the prompt's own call cannot be on disk yet", prompt)
	}

	// The gated call flushes and binds. The sibling was never a candidate.
	writeToolUseLine(t, root.Transcript, dispatchedFor(gatedAt), "Bash", "toolu_gated")
	observeUntilLatched(t, o, root, gatedAt.Add(6*time.Second))
	if prompt := o.Projection(root.Key()).Pending[""]; prompt.Latch != CallLatchBound || prompt.CallID != "toolu_gated" {
		t.Fatalf("flushed prompt = %+v, want it bound to its own gated call", prompt)
	}

	// The sibling returns clean. It answers a call nobody was waiting on, so the
	// approval dialog is still up and the chip stays red.
	writeToolResultLine(t, root.Transcript, gatedAt.Add(9*time.Second), "toolu_sibling", "ok", false)
	observation, err := o.Observe(context.Background(), root, gatedAt.Add(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if pending := o.Projection(root.Key()).Pending; len(pending) != 1 {
		t.Fatalf("pending after the sibling finished = %+v, want the gate still held", pending)
	}
	assertSummary(t, observation, gatedAt.Add(10*time.Second), agentgraph.LegacyPermission, agentgraph.AttentionApproval)
}

// One tick's uniqueness is not uniqueness. A single assistant message's parallel
// tool_use blocks are separate JSONL entries 0.5–1.5 s apart, so a tick landing
// inside that gap sees a gated call's auto-approved sibling ALONE and would read
// it as the one candidate — the exact wrong bind the ambiguity rule exists to
// refuse. A second tick a whole interval later sees both and refuses.
func TestLatchShouldRefuseACandidateTheNextTickNoLongerAgreesOn(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})
	gatedAt := now.Add(time.Second)
	o.ApplyHook(HookSignal{
		Root: root, Event: "PermissionRequest", ToolName: "Bash",
		ToolInputHash: "run-tests", At: gatedAt,
	})

	// The auto-approved sibling's line reaches disk first.
	writeToolUseLine(t, root.Transcript, dispatchedFor(gatedAt), "Bash", "toolu_sibling")
	if _, err := o.Observe(context.Background(), root, gatedAt.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if prompt := o.Projection(root.Key()).Pending[""]; prompt.Latch == CallLatchBound {
		t.Fatalf("prompt = %+v, want no bind off a single tick inside the flush gap", prompt)
	}

	// The gated line lands, and now nothing in the file separates the two.
	writeToolUseLine(t, root.Transcript, dispatchedFor(gatedAt), "Bash", "toolu_gated")
	if _, err := o.Observe(context.Background(), root, gatedAt.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}
	if prompt := o.Projection(root.Key()).Pending[""]; prompt.Latch != CallLatchAmbiguous || prompt.CallID != "" {
		t.Fatalf("prompt = %+v, want the proposal withdrawn as ambiguous", prompt)
	}

	// The proof that matters: the sibling's own result cannot clear the red.
	writeToolResultLine(t, root.Transcript, gatedAt.Add(7*time.Second), "toolu_sibling", "ok", false)
	observation, err := o.Observe(context.Background(), root, gatedAt.Add(8*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	assertSummary(t, observation, gatedAt.Add(8*time.Second), agentgraph.LegacyPermission, agentgraph.AttentionApproval)
}

// The exit colour describes the WRITER, not whichever prompt sat last in the
// slice. A tick that closes one rejected call and one approved one leaves the
// agent executing the approved tool, so the aggregate must not inherit the
// decline from slice position.
func TestDeclinedCallShouldSetTheExitOnlyWhenEveryClosedCallDeclined(t *testing.T) {
	for name, tc := range map[string]struct {
		declined []string
		rule     string
		legacy   string
	}{
		"the older call was rejected":    {[]string{"toolu_ask"}, statustune.RuleGraphCallResolved, agentgraph.LegacyWorking},
		"the newer call was rejected":    {[]string{"toolu_bash"}, statustune.RuleGraphCallResolved, agentgraph.LegacyWorking},
		"every closed call was rejected": {[]string{"toolu_ask", "toolu_bash"}, statustune.RuleGraphCallDeclined, agentgraph.LegacyIdle},
	} {
		t.Run("should exit to "+tc.legacy+" when "+name, func(t *testing.T) {
			o, root, now := newTestObserver(t)
			defer o.Close()
			o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})
			askAt, bashAt := now.Add(time.Second), now.Add(2*time.Second)
			o.ApplyHook(HookSignal{
				Root: root, Event: "PermissionRequest", ToolName: "AskUserQuestion",
				ToolInputHash: "ask-before", At: askAt,
			})
			o.ApplyHook(HookSignal{
				Root: root, Event: "PermissionRequest", ToolName: "Bash",
				ToolInputHash: "run-tests", At: bashAt,
			})
			writeToolUseLine(t, root.Transcript, dispatchedFor(askAt), "AskUserQuestion", "toolu_ask")
			writeToolUseLine(t, root.Transcript, dispatchedFor(bashAt), "Bash", "toolu_bash")
			observeUntilLatched(t, o, root, now.Add(7*time.Second))

			for _, callID := range []string{"toolu_ask", "toolu_bash"} {
				if slices.Contains(tc.declined, callID) {
					writeDeclineLine(t, root.Transcript, now.Add(10*time.Second), callID)
					continue
				}
				writeToolResultLine(t, root.Transcript, now.Add(10*time.Second), callID, "done", false)
			}
			observation, err := o.Observe(context.Background(), root, now.Add(11*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if pending := o.Projection(root.Key()).Pending; len(pending) != 0 {
				t.Fatalf("pending after both calls came back = %+v, want the red closed", pending)
			}
			if rule := o.DrainResolutionRule(root.Key()); rule != tc.rule {
				t.Fatalf("resolution rule = %q, want %q", rule, tc.rule)
			}
			assertSummary(t, observation, now.Add(11*time.Second), tc.legacy, agentgraph.AttentionNone)
		})
	}
}

// The whole-file rules date from the writer's newest prompt, and a call-scoped
// clear can now retire exactly that one. If the anchor followed the surviving
// set it would REGRESS, and the next tick would read the assistant entry that
// dispatched the call it just closed as fresh evidence — ResolveKind maps any
// assistant entry to "the writer resumed" — closing the older prompt whose call
// is still open and unanswered. Silent, and it lasts until the user next looks.
func TestRetiringTheNewestPromptShouldNotResolveAnOlderOneOnItsOwnDispatch(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})

	// One turn, two gated calls of different tools. No corpus entry carries more
	// than one tool_use block, so the two dispatches are separate assistant
	// entries about a second apart — which puts the question's entry BETWEEN the
	// two prompts.
	bashAt, askAt := now.Add(time.Second), now.Add(2*time.Second)
	writeToolUseLine(t, root.Transcript, dispatchedFor(bashAt), "Bash", "toolu_bash")
	o.ApplyHook(HookSignal{
		Root: root, Event: "PermissionRequest", ToolName: "Bash",
		ToolInputHash: "run-tests", At: bashAt,
	})
	writeToolUseLine(t, root.Transcript, dispatchedFor(askAt), "AskUserQuestion", "toolu_ask")
	o.ApplyHook(HookSignal{
		Root: root, Event: "PermissionRequest", ToolName: "AskUserQuestion",
		ToolInputHash: "ask-before", At: askAt,
	})
	observeUntilLatched(t, o, root, now.Add(8*time.Second))

	// The user answers the question first. Its own result closes that prompt and
	// nothing else; the Bash approval dialog is still on screen.
	writeToolResultLine(t, root.Transcript, now.Add(10*time.Second), "toolu_ask", "chose option A", false)
	if _, err := o.Observe(context.Background(), root, now.Add(11*time.Second)); err != nil {
		t.Fatal(err)
	}
	if pending := o.Projection(root.Key()).Pending; len(pending) != 1 || pending[""].Tool != "Bash" {
		t.Fatalf("pending after the question was answered = %+v, want the Bash gate alone", pending)
	}

	observation, err := o.Observe(context.Background(), root, now.Add(12*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if pending := o.Projection(root.Key()).Pending; len(pending) != 1 || pending[""].Tool != "Bash" {
		t.Fatalf("the next tick released the Bash gate: %+v", pending)
	}
	assertSummary(t, observation, now.Add(12*time.Second), agentgraph.LegacyPermission, agentgraph.AttentionApproval)
}

// P3's runtime half has two orderings and only one of them used to be counted.
// When the writers race in one tick the claim is visible on both sides; when they
// arrive in sequence the second writer's candidate is simply filtered out as
// "already owned", which looks exactly like "not flushed yet" — so the counter
// that exists to tell us whether the lifted fanout floor's premise still holds
// could not see the case where it fails.
func TestLatchShouldCountACollisionWhenTheContestedCallIsAlreadyBoundElsewhere(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})
	subdir := filepath.Join(filepath.Dir(root.Transcript), root.ProviderSessionID, "subagents")
	writeClaudeChild(t, subdir, "child-a", "Explore", "work", "", now)
	if _, err := o.Observe(context.Background(), root, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	mainAt := now.Add(2 * time.Second)
	writeToolUseLine(t, root.Transcript, dispatchedFor(mainAt), "AskUserQuestion", "toolu_shared")
	o.ApplyHook(HookSignal{
		Root: root, Event: "PermissionRequest", ToolName: "AskUserQuestion",
		ToolInputHash: "shape-main", At: mainAt,
	})
	observeUntilLatched(t, o, root, now.Add(7*time.Second))
	if prompt := o.Projection(root.Key()).Pending[""]; prompt.CallID != "toolu_shared" {
		t.Fatalf("main thread = %+v, want it bound first so the collision arrives second", prompt)
	}
	o.DrainPromptDiagnostics(root.Key())

	// child-a raises its own prompt, and its own file carries that same id
	// unmatched — the cross-file duplication P3 rules out.
	childAt := now.Add(10 * time.Second)
	writeToolUseLine(t, filepath.Join(subdir, "agent-child-a.jsonl"), dispatchedFor(childAt), "AskUserQuestion", "toolu_shared")
	o.ApplyHook(HookSignal{
		Root: root, Event: "PermissionRequest", AgentID: "child-a", ToolName: "AskUserQuestion",
		ToolInputHash: "shape-child-a", At: childAt,
	})
	if _, err := o.Observe(context.Background(), root, now.Add(11*time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := o.DrainPromptDiagnostics(root.Key()); !slices.Contains(got, DiagnosticCallIDCollision) {
		t.Fatalf("latch diagnostics = %v, want the sequential collision counted", got)
	}
	if prompt := o.Projection(root.Key()).Pending["child-a"]; prompt.Latch != CallLatchContested || prompt.CallID != "" {
		t.Fatalf("child-a = %+v, want the contested id refused rather than silently dropped", prompt)
	}
}

// writer_call_mismatch_held means "I know which call you finished, and it is not
// the one you are waiting on" — the knob table reads it as the identity guard
// working. A hold whose real cause was the fanout floor is a different thing
// entirely ("a shape matched and nothing identified the writer"), and labelling
// it as a call mismatch hides the case the table wants chased.
func TestFanoutFloorHoldShouldNotBeReportedAsACallMismatch(t *testing.T) {
	o, root, now := newTestObserver(t)
	defer o.Close()
	o.ApplyHook(HookSignal{Root: root, Event: "SessionStart", At: now})
	subdir := filepath.Join(filepath.Dir(root.Transcript), root.ProviderSessionID, "subagents")
	writeClaudeChild(t, subdir, "live", "general-purpose", "work", "", now)
	if _, err := o.Observe(context.Background(), root, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	firstAt := now.Add(2 * time.Second)
	writeToolUseLine(t, root.Transcript, dispatchedFor(firstAt), "Bash", "toolu_first")
	o.ApplyHook(HookSignal{
		Root: root, Event: "PermissionRequest", ToolName: "Bash",
		ToolInputHash: "build", At: firstAt,
	})
	observeUntilLatched(t, o, root, now.Add(7*time.Second))
	if o.Projection(root.Key()).InFlightSubagents == 0 {
		t.Fatal("fixture no longer has a teammate in flight, so the floor is not under test")
	}

	// A second gate opens with no identity of its own, and a completion arrives
	// that shape-matches it from an unidentifiable writer.
	o.ApplyHook(HookSignal{
		Root: root, Event: "PermissionRequest", ToolName: "Bash",
		ToolInputHash: "run-tests", At: now.Add(10 * time.Second),
	})
	result := o.ApplyHook(HookSignal{
		Root: root, Event: "PostToolUse", ToolName: "Bash",
		ToolInputHash: "run-tests", ToolUseID: "toolu_other", At: now.Add(11 * time.Second),
	})
	if result.Rule != statustune.RuleGraphPromptHeld || result.PromptDepth != 2 {
		t.Fatalf("floor-held shape match = rule %q depth %d, want writer_prompt_held", result.Rule, result.PromptDepth)
	}
}

// dispatchedFor is the instant the assistant entry for a call gated at `hookAt`
// carries. Claude Code dates it at GENERATION time, 6–374 ms before the
// PermissionRequest hook fires (docs/subagent-permission-plan.md §9.7), and the
// latch weighs that offset against callLatchSkewGrace — so a fixture that dates
// a prompt's own call an arbitrary distance in the past is not testing the rule
// the code implements.
func dispatchedFor(hookAt time.Time) time.Time {
	return hookAt.Add(-200 * time.Millisecond)
}

// observeUntilLatched runs the two Observe ticks a bind takes: the first names
// the candidate, the second confirms it against a fresh read of the tail
// (CallLatchProposed). Production pays one extra tick against 45–300 s of
// measured think-time; a test that wants an identified prompt pays it here.
// observeUntilLatched runs the two reads a bind needs: one to propose the
// candidate and one, a full callLatchConfirmGrace later, to confirm it. The
// spacing is not cosmetic — a confirming read inside that grace is refused,
// because two reads of the same partial file agree for the same wrong reason
// (see callLatchConfirmGrace).
func observeUntilLatched(t *testing.T, o *Observer, root provider.RootRef, at time.Time) {
	t.Helper()
	for _, offset := range []time.Duration{0, callLatchConfirmGrace + time.Second} {
		if _, err := o.Observe(context.Background(), root, at.Add(offset)); err != nil {
			t.Fatal(err)
		}
	}
}

// writeToolUseLine appends the assistant entry Claude Code writes when it
// dispatches a call: one tool_use, dated at generation time (before the hook
// fired) and flushed a beat later.
func writeToolUseLine(t *testing.T, path string, at time.Time, tool, callID string) {
	t.Helper()
	appendClaudeLine(t, path, `{"type":"assistant","timestamp":"`+at.Format(time.RFC3339Nano)+
		`","message":{"role":"assistant","content":[{"type":"tool_use","id":"`+callID+
		`","name":"`+tool+`","input":{}}]}}`)
}

// writeToolResultLine appends the user entry that answers one call.
func writeToolResultLine(t *testing.T, path string, at time.Time, callID, content string, isError bool) {
	t.Helper()
	flag := "false"
	if isError {
		flag = "true"
	}
	appendClaudeLine(t, path, `{"type":"user","timestamp":"`+at.Format(time.RFC3339Nano)+
		`","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"`+callID+
		`","is_error":`+flag+`,"content":"`+content+`"}]}}`)
}

// writeDeclineLine appends the corpus shape of a user rejection: is_error on the
// block plus the two entry-level fields that separate it from a tool that ran
// and failed.
func writeDeclineLine(t *testing.T, path string, at time.Time, callID string) {
	t.Helper()
	appendClaudeLine(t, path, `{"type":"user","timestamp":"`+at.Format(time.RFC3339Nano)+
		`","toolUseResult":"User rejected tool use","toolDenialKind":"user-rejected",`+
		`"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"`+callID+
		`","is_error":true,"content":"The user doesn't want to proceed with this tool use."}]}}`)
}
