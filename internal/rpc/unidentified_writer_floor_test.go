package rpc

import (
	"strings"
	"testing"

	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/statustune"
)

// T21 — the T2 floor OUTRANKS the input-hash correlator.
//
// T7 restored the hook-speed approve path by matching on (agent_id, tool_name,
// tool_input hash), and exempted a hash match from the floor: an empty agent_id with
// teammates in flight could still clear red as long as the hash agreed. The stated
// reasoning was that a name AND a hash collision requires "two independent failures
// to coincide".
//
// They are not independent. hashToolInput (cmd/switchboard-ctl) digests tool_input
// and nothing else — no cwd, no session id, no writer — so the hash answers WHICH
// CALL and never WHO RAN IT. It is a second reading of the very axis agent_id
// covers, which makes it load-bearing in exactly the case where agent_id has already
// told us nothing: agent_id absent on tool events, the degradation plan T1 has not
// yet ruled out. And a byte-identical, same-tool call from a teammate is ordinary
// under fanout rather than exotic — N agents in N worktrees run one `go build ./...`,
// and a command pre-approved under one worktree's permission root while it prompts
// under another collides with no human having answered anything.
//
// So: unidentified writer + teammates live ⇒ never at hook speed, whatever matches.
// The cost is one reconcile tick (≤5s), in the degraded case only. A missed RED is
// the worst error in the model's ranking (status-color-state-model.md §4.1); a
// slow-but-correct clear is the cheapest.
func TestClearsPermissionFloorOutranksTheInputHash(t *testing.T) {
	t.Run("should hold red when an unidentified writer's input hash matches while subagents are in flight", func(t *testing.T) {
		// The reversal, stated directly. No transcript file exists for the main
		// thread, so nothing can resolve the prompt at hook speed and a clear could
		// only have come from the hash exemption T7 built.
		store, s := fannedRedSession(t, 3,
			map[string]state.PendingPrompt{"": blockedOn("Bash", pendingHash)}, nil)
		buf := captureLog(t)

		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "Bash",
			ToolInputHash: pendingHash}) // empty agent_id: unattributable

		got := claudeStatus(t, store)
		if got.Status != state.StatusPermission {
			t.Errorf("status = %q, want permission (a hash names a CALL, not a writer — a teammate running the identical command produces it)", got.Status)
		}
		wantWriters(t, got, state.PendingWriterMain)
		if out := buf.String(); !strings.Contains(out, "rule="+statustune.RuleHoldTeammateCollision) {
			t.Errorf("missing %s decision line in:\n%s", statustune.RuleHoldTeammateCollision, out)
		}
	})

	t.Run("should still clear red on the next reconcile-equivalent signal when the unidentified writer's own transcript shows the turn resumed", func(t *testing.T) {
		// The floor costs latency, never correctness: the fall-through is a HOLD at
		// hook speed, not a latch. Routed to the main .jsonl for the empty key, the
		// same transcript check that the reconcile tick would run clears it as soon
		// as the turn actually resumed.
		store, s := fannedRedSession(t, 3,
			map[string]state.PendingPrompt{"": blockedOn("Bash", pendingHash)},
			map[string]string{"": turnResumed})
		buf := captureLog(t)

		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "Bash",
			ToolInputHash: pendingHash})

		got := claudeStatus(t, store)
		if got.Status != state.StatusWorking {
			t.Errorf("status = %q, want working (the floor defers to the transcript, it does not latch red)", got.Status)
		}
		if len(got.Pending) != 0 {
			t.Errorf("pending writers = %v, want none", pendingWriterNames(got))
		}
		if out := buf.String(); !strings.Contains(out, "rule="+statustune.RuleApproveTranscript) {
			t.Errorf("the clear must be attributed to the transcript, not the fast path; got:\n%s", out)
		}
	})

	t.Run("should hold red when an unidentified writer's tool name matches with no hash on either side and subagents are in flight", func(t *testing.T) {
		// T2's original edge, unchanged by T21 — pinned here so the reordered switch
		// cannot regress it while the hash case is being tightened.
		store, s := fannedRedSession(t, 3,
			map[string]state.PendingPrompt{"": blockedOn("Bash", "")},
			map[string]string{"": siblingResultOnly})

		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "Bash"})

		got := claudeStatus(t, store)
		if got.Status != state.StatusPermission {
			t.Errorf("status = %q, want permission", got.Status)
		}
		wantWriters(t, got, state.PendingWriterMain)
	})
}

// The floor is about TEAMMATES, not about anonymity. With nothing in flight that
// could have produced a colliding call, an empty agent_id is simply the main thread
// and the hook-speed path is sound — so tightening must not cost the ordinary
// single-threaded session anything at all.
func TestClearsPermissionFloorAppliesOnlyWhileTeammatesAreInFlight(t *testing.T) {
	t.Run("should clear red at hook speed for an unidentified writer with a matching hash when no subagents are in flight", func(t *testing.T) {
		// No transcript file, so only the fast path can have cleared this.
		store, s := fannedRedSession(t, 0,
			map[string]state.PendingPrompt{"": blockedOn("Bash", pendingHash)}, nil)
		buf := captureLog(t)

		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "Bash",
			ToolInputHash: pendingHash})

		got := claudeStatus(t, store)
		if got.Status != state.StatusWorking {
			t.Errorf("status = %q, want working (S=0: no teammate exists to have produced this call)", got.Status)
		}
		if out := buf.String(); !strings.Contains(out, "rule="+statustune.RuleApproveToolMatch) {
			t.Errorf("missing %s decision line in:\n%s", statustune.RuleApproveToolMatch, out)
		}
	})

	t.Run("should clear red at hook speed for an unidentified writer on tool name alone when no subagents are in flight", func(t *testing.T) {
		store, s := fannedRedSession(t, 0,
			map[string]state.PendingPrompt{"": blockedOn("Bash", "")}, nil)

		if got := claudeStatus(t, store); got.Status != state.StatusPermission {
			t.Fatalf("fixture status = %q, want permission", got.Status)
		}
		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "Bash"})

		if got := claudeStatus(t, store); got.Status != state.StatusWorking {
			t.Errorf("status = %q, want working", got.Status)
		}
	})
}

// The identified-writer fast path is the whole latency win T7 bought, and T21 must
// not touch it: naming the writer is what makes a correlator match sound, so a hook
// that carries an agent_id keeps clearing at hook speed no matter how many teammates
// are live.
func TestClearsPermissionKeepsTheIdentifiedWriterFastPath(t *testing.T) {
	t.Run("should clear red at hook speed when an identified writer's tool and input hash match with subagents in flight", func(t *testing.T) {
		// The blocked writer has no transcript file, so the clear can only be the
		// correlator match.
		store, s := fannedRedSession(t, 4,
			map[string]state.PendingPrompt{blockedWriter: blockedOn("Bash", pendingHash)}, nil)
		buf := captureLog(t)

		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "Bash",
			ToolInputHash: pendingHash, AgentID: blockedWriter})

		got := claudeStatus(t, store)
		if got.Status != state.StatusWorking {
			t.Errorf("status = %q, want working (agent_id names the writer, so the match is about identity)", got.Status)
		}
		if len(got.Pending) != 0 {
			t.Errorf("pending writers = %v, want none", pendingWriterNames(got))
		}
		if out := buf.String(); !strings.Contains(out, "rule="+statustune.RuleApproveToolMatch) {
			t.Errorf("missing %s decision line in:\n%s", statustune.RuleApproveToolMatch, out)
		}
	})

	t.Run("should clear red at hook speed when an identified writer matches on tool name alone with subagents in flight", func(t *testing.T) {
		// An empty hash is no signal, and the floor keys on writer identity rather
		// than on hash presence — so a named writer keeps the no-signal fallback.
		store, s := fannedRedSession(t, 4,
			map[string]state.PendingPrompt{blockedWriter: blockedOn("Bash", "")}, nil)

		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "Bash",
			AgentID: blockedWriter})

		if got := claudeStatus(t, store); got.Status != state.StatusWorking {
			t.Errorf("status = %q, want working", got.Status)
		}
	})

	t.Run("should hold red when an identified teammate that owns no prompt matches the pending hash", func(t *testing.T) {
		// The floor is not what rules teammates out — writer ownership is, and it
		// still does, so the two guards do not overlap.
		store, s := fannedRedSession(t, 4,
			map[string]state.PendingPrompt{blockedWriter: blockedOn("Bash", pendingHash)},
			map[string]string{"": turnResumed})

		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "Bash",
			ToolInputHash: pendingHash, AgentID: teammate})

		got := claudeStatus(t, store)
		if got.Status != state.StatusPermission {
			t.Errorf("status = %q, want permission", got.Status)
		}
		wantWriters(t, got, blockedWriter)
	})
}

// Case 18 under the floor: an unidentified writer's hash match no longer resolves
// even its OWN entry at hook speed, so a second blocked writer's prompt is doubly
// safe — but the per-writer removal must still be the mechanism, not a whole-map
// wipe, once the transcript does resolve the main thread's entry.
func TestClearsPermissionFloorPreservesPerWriterRemoval(t *testing.T) {
	t.Run("should drop only the unidentified writer's entry when its transcript resolves it and a teammate is still blocked", func(t *testing.T) {
		store, s := fannedRedSession(t, 2, map[string]state.PendingPrompt{
			"":            blockedOn("AskUserQuestion", pendingHash),
			blockedWriter: blockedOn("Bash", siblingHash),
		}, map[string]string{"": turnResumed})
		buf := captureLog(t)

		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42,
			ToolName: "AskUserQuestion", ToolInputHash: pendingHash})

		got := claudeStatus(t, store)
		if got.Status != state.StatusPermission {
			t.Errorf("status = %q, want permission — %s is still waiting", got.Status, blockedWriter)
		}
		wantWriters(t, got, blockedWriter)
		if p, ok := got.Pending[blockedWriter]; !ok || p.Tool != "Bash" || p.InputHash != siblingHash {
			t.Errorf("teammate's prompt = %+v (present=%v), want it untouched", p, ok)
		}
		if rec := lastDecision(t, buf.String()); rec.Rule != statustune.RuleHoldOtherWriter {
			t.Errorf("decision rule = %q, want %s", rec.Rule, statustune.RuleHoldOtherWriter)
		}
	})
}
