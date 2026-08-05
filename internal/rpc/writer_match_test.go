package rpc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/statustune"
	"github.com/tjmisko/switchboard/internal/terminal"
	"github.com/tjmisko/switchboard/internal/wm"
)

// T7 + the hook half of T9 (docs/subagent-permission-plan.md §3.3): a pending
// prompt is owned by the WRITER that raised it, and only that writer's evidence
// may resolve it —
//
//	agent_id mismatch                          → hold (rules teammates out)
//	agent_id + tool_name + input hash match     → clear at hook speed
//	agent_id + tool_name, hashes differ         → the writer's OWN transcript decides
//	agent_id + tool_name, no hash either side   → clear, unless T2's floor applies
//
// The 2026-08-05 incident is the first case verbatim: a teammate's Bash cleared a
// pending Bash prompt one second after the blunt guard correctly held it.

const (
	// pendingHash / siblingHash are two canonicalized tool_input digests for the
	// SAME tool kind by the SAME writer — the sibling-call collision that tool_name
	// alone cannot see.
	pendingHash = "c0ffee0011223344"
	siblingHash = "deadbeef55667788"

	// blockedWriter is the incident's blocked teammate, `escalate-cleanup`.
	blockedWriter = "af5bd126402ac16c7"
)

// fannedRedSession seeds a red chip whose transcript layout is the real one — a
// main <session>.jsonl beside a <session>/subagents/agent-<id>.jsonl per writer —
// so transcript.SubagentPath resolves to a file that actually exists. `bodies` maps
// a writer key ("" = main thread) to that writer's jsonl body; a writer absent from
// the map gets no file at all, which is how an unreadable tail is expressed.
func fannedRedSession(t *testing.T, subagents int, pending map[string]state.PendingPrompt, bodies map[string]string) (*state.Store, *Server) {
	t.Helper()
	base := t.TempDir()
	sid := "5318eb5b-79df-4dee-a9f8-c80df4eca79e"
	main := filepath.Join(base, sid+".jsonl")
	subdir := filepath.Join(base, sid, "subagents")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	for writer, body := range bodies {
		path := main
		if writer != "" {
			path = filepath.Join(subdir, "agent-"+writer+".jsonl")
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %q transcript: %v", writer, err)
		}
	}

	store := state.New("")
	store.Apply(func(m map[int]*state.Session) {
		info := &state.AgentInfo{
			SessionID: sid, Status: state.StatusPermission, StatusSince: promptSince,
			Transcript: main, InFlightSubagents: subagents,
		}
		for writer, p := range pending {
			info.SetPending(writer, p)
		}
		m[42] = &state.Session{PID: 42, CWD: "/home/u/proj", Agent: state.AgentKindClaude, Claude: info}
	})
	return store, New(store, "", terminal.NewNone(), wm.NewNone())
}

// blockedOn is the prompt a writer is parked on, dated at the incident's onset.
func blockedOn(tool, hash string) state.PendingPrompt {
	return state.PendingPrompt{Tool: tool, InputHash: hash, Since: promptSince}
}

func TestClearsPermissionRequiresWriterIdentity(t *testing.T) {
	t.Run("should hold red when a teammate completes the same tool with the same input hash", func(t *testing.T) {
		// The hard case for any correlator short of writer identity: everything
		// matches except WHO ran it. Two teammates running the same command produce
		// byte-identical tool_input, so only agent_id can tell them apart.
		store, s := fannedRedSession(t, 4,
			map[string]state.PendingPrompt{blockedWriter: blockedOn("Bash", pendingHash)},
			map[string]string{"": turnResumed}) // main thread busy — not evidence about blockedWriter
		buf := captureLog(t)

		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "Bash",
			ToolInputHash: pendingHash, AgentID: teammate})

		got := claudeStatus(t, store)
		if got.Status != state.StatusPermission {
			t.Errorf("status = %q, want permission (a teammate's identical call is not the blocked writer's approval)", got.Status)
		}
		wantWriters(t, got, blockedWriter)
		if out := buf.String(); !strings.Contains(out, "rule="+statustune.RuleHoldTeammateCollision) {
			t.Errorf("missing %s decision line in:\n%s", statustune.RuleHoldTeammateCollision, out)
		}
	})

	t.Run("should clear red at hook speed when the blocked writer completes the pending tool with the pending input hash", func(t *testing.T) {
		// The approve path, restored: with the writer named, the fast path is sound
		// again even with four teammates in flight. No transcript exists for the
		// blocked writer, so a clear can only have come from the correlator match.
		store, s := fannedRedSession(t, 4,
			map[string]state.PendingPrompt{blockedWriter: blockedOn("Bash", pendingHash)}, nil)
		buf := captureLog(t)

		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "Bash",
			ToolInputHash: pendingHash, AgentID: blockedWriter})

		got := claudeStatus(t, store)
		if got.Status != state.StatusWorking {
			t.Errorf("status = %q, want working (writer+tool+input is the approved call completing)", got.Status)
		}
		if len(got.Pending) != 0 {
			t.Errorf("pending writers = %v, want none", pendingWriterNames(got))
		}
		if out := buf.String(); !strings.Contains(out, "rule="+statustune.RuleApproveToolMatch) {
			t.Errorf("missing %s decision line in:\n%s", statustune.RuleApproveToolMatch, out)
		}
	})

	t.Run("should hold red when the main thread completes the pending tool but a subagent owns the prompt", func(t *testing.T) {
		// T9 on the hook path: info.Transcript is ALWAYS the parent's
		// (claude-code-hook-schema.md §3), so main-thread activity would otherwise
		// answer a question about a teammate. It is not evidence — defect 4.
		store, s := fannedRedSession(t, 1,
			map[string]state.PendingPrompt{blockedWriter: blockedOn("Bash", pendingHash)},
			map[string]string{"": turnResumed})

		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "Bash",
			ToolInputHash: pendingHash}) // empty agent_id: the MAIN thread

		got := claudeStatus(t, store)
		if got.Status != state.StatusPermission {
			t.Errorf("status = %q, want permission (a working main thread does not answer a teammate's prompt)", got.Status)
		}
		wantWriters(t, got, blockedWriter)
	})
}

func TestClearsPermissionTreatsTheInputHashAsEvidenceNotAsAGate(t *testing.T) {
	t.Run("should fall through to the writer's transcript when the same writer's tool carries a different input hash", func(t *testing.T) {
		// The approve paths REWRITE tool_input before PostToolUse reports it
		// (`{...e,command:g}` on Bash, `{...e,path:r}` on a permission-root
		// relocation, a userModified edit), so a mismatch cannot latch red — but it
		// cannot clear on its own either, since it is equally a sibling call. The
		// blocked writer's own transcript is what settles it.
		store, s := fannedRedSession(t, 4,
			map[string]state.PendingPrompt{blockedWriter: blockedOn("Bash", pendingHash)},
			map[string]string{blockedWriter: turnResumed})
		buf := captureLog(t)

		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "Bash",
			ToolInputHash: siblingHash, AgentID: blockedWriter})

		got := claudeStatus(t, store)
		if got.Status != state.StatusWorking {
			t.Errorf("status = %q, want working (the writer's own transcript shows it resumed)", got.Status)
		}
		if out := buf.String(); !strings.Contains(out, "rule="+statustune.RuleApproveTranscript) {
			t.Errorf("a rewritten input must clear via the transcript, not the fast path; got:\n%s", out)
		}
	})

	t.Run("should hold red when the same writer's tool carries a different input hash and its transcript shows no resume", func(t *testing.T) {
		// The other half of the same ambiguity: a sibling call by the blocked writer
		// (Claude Code emits parallel tool_use blocks) must not clear the prompt it is
		// running alongside.
		store, s := fannedRedSession(t, 4,
			map[string]state.PendingPrompt{blockedWriter: blockedOn("Bash", pendingHash)},
			map[string]string{blockedWriter: siblingResultOnly, "": turnResumed})
		buf := captureLog(t)

		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "Bash",
			ToolInputHash: siblingHash, AgentID: blockedWriter})

		got := claudeStatus(t, store)
		if got.Status != state.StatusPermission {
			t.Errorf("status = %q, want permission (a sibling call is not the gated one completing)", got.Status)
		}
		wantWriters(t, got, blockedWriter)
		if out := buf.String(); !strings.Contains(out, "rule="+statustune.RuleHoldInputMismatch) {
			t.Errorf("missing %s decision line in:\n%s", statustune.RuleHoldInputMismatch, out)
		}
	})

	t.Run("should clear at hook speed on writer and tool_name alone when neither event carried an input hash", func(t *testing.T) {
		// An empty hash is NO SIGNAL (a no-arg or unparseable tool_input, or a ctl
		// predating the field), never a mismatch — so the decision falls back to
		// (agent_id, tool_name) rather than stranding every such call on the slow path.
		// No transcript exists here, so only the fast path can have cleared it.
		store, s := fannedRedSession(t, 4,
			map[string]state.PendingPrompt{blockedWriter: blockedOn("Bash", "")}, nil)

		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "Bash",
			AgentID: blockedWriter})

		if got := claudeStatus(t, store); got.Status != state.StatusWorking {
			t.Errorf("status = %q, want working (no hash on either side leaves writer+tool as the evidence)", got.Status)
		}
	})

	t.Run("should not treat two empty hashes as a match when the writer is a teammate", func(t *testing.T) {
		// The no-signal fallback is narrowed by writer identity, so it cannot become
		// the tool-kind guess it replaced.
		store, s := fannedRedSession(t, 4,
			map[string]state.PendingPrompt{blockedWriter: blockedOn("Bash", "")}, nil)

		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "Bash", AgentID: teammate})

		got := claudeStatus(t, store)
		if got.Status != state.StatusPermission {
			t.Errorf("status = %q, want permission", got.Status)
		}
		wantWriters(t, got, blockedWriter)
	})
}

// T2's guard survives as the floor beneath T7: an empty agent_id means "main
// thread" AND "a hook that carried no id", so while teammates are live it may never
// clear on a tool KIND alone. Fail closed, never open.
func TestClearsPermissionKeepsTheUnidentifiedWriterFloor(t *testing.T) {
	t.Run("should hold red when an empty agent_id matches the pending tool name with subagents in flight", func(t *testing.T) {
		store, s := fannedRedSession(t, 3,
			map[string]state.PendingPrompt{"": blockedOn("Bash", "")},
			map[string]string{"": siblingResultOnly})
		buf := captureLog(t)

		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "Bash"})

		got := claudeStatus(t, store)
		if got.Status != state.StatusPermission {
			t.Errorf("status = %q, want permission (tool_name alone is a KIND while any teammate can produce one)", got.Status)
		}
		wantWriters(t, got, state.PendingWriterMain)
		if out := buf.String(); !strings.Contains(out, "rule="+statustune.RuleHoldTeammateCollision) {
			t.Errorf("missing %s decision line in:\n%s", statustune.RuleHoldTeammateCollision, out)
		}
	})

	t.Run("should clear red for an empty agent_id with subagents in flight when the input hash also matches", func(t *testing.T) {
		// The floor blocks a name-ALONE clear. A hash match is not name alone, and
		// restoring the approve path for the main thread mid-fanout is the latency
		// win T7 buys back. No transcript exists, so this is the fast path.
		store, s := fannedRedSession(t, 3,
			map[string]state.PendingPrompt{"": blockedOn("Bash", pendingHash)}, nil)

		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "Bash",
			ToolInputHash: pendingHash})

		if got := claudeStatus(t, store); got.Status != state.StatusWorking {
			t.Errorf("status = %q, want working (writer+tool+input is not a kind guess)", got.Status)
		}
	})

	t.Run("should clear red for an empty agent_id on tool_name alone when no subagents are in flight", func(t *testing.T) {
		// With nothing that could collide, the pre-T7 fast path is preserved intact.
		store, s := fannedRedSession(t, 0,
			map[string]state.PendingPrompt{"": blockedOn("Bash", "")}, nil)

		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "Bash"})

		if got := claudeStatus(t, store); got.Status != state.StatusWorking {
			t.Errorf("status = %q, want working (no teammate can have produced this Bash)", got.Status)
		}
	})
}

// Case 18 (docs/status-color-state-model.md §5): the main thread and a teammate are
// both blocked. Resolving either one leaves the other's prompt outstanding, and the
// chip is red for as long as ANY writer is waiting — the fold the map exists for.
func TestClearsPermissionHoldsRedWhileASecondWriterIsStillBlocked(t *testing.T) {
	twoBlocked := func(t *testing.T) (*state.Store, *Server) {
		t.Helper()
		return fannedRedSession(t, 2, map[string]state.PendingPrompt{
			"":            blockedOn("AskUserQuestion", pendingHash),
			blockedWriter: blockedOn("Bash", siblingHash),
		}, map[string]string{"": turnResumed}) // the main thread's own prompt is answered
	}

	t.Run("should keep the chip red when the main thread's prompt resolves but a teammate is still blocked", func(t *testing.T) {
		store, s := twoBlocked(t)

		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42,
			ToolName: "AskUserQuestion", ToolInputHash: pendingHash})

		got := claudeStatus(t, store)
		if got.Status != state.StatusPermission {
			t.Errorf("status = %q, want permission — %s is still waiting", got.Status, blockedWriter)
		}
		wantWriters(t, got, blockedWriter)
		if !got.StatusSince.Equal(promptSince) {
			t.Errorf("StatusSince = %v, want the onset %v (a partial resolution is not a transition)", got.StatusSince, promptSince)
		}
	})

	t.Run("should keep the chip red when a teammate's prompt resolves but the main thread is still blocked", func(t *testing.T) {
		store, s := twoBlocked(t)

		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "Bash",
			ToolInputHash: siblingHash, AgentID: blockedWriter})

		got := claudeStatus(t, store)
		if got.Status != state.StatusPermission {
			t.Errorf("status = %q, want permission — the main thread is still waiting", got.Status)
		}
		wantWriters(t, got, state.PendingWriterMain)
	})

	t.Run("should leave red only once the last blocked writer resolves", func(t *testing.T) {
		store, s := twoBlocked(t)

		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "Bash",
			ToolInputHash: siblingHash, AgentID: blockedWriter})
		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42,
			ToolName: "AskUserQuestion", ToolInputHash: pendingHash})

		got := claudeStatus(t, store)
		if got.Status != state.StatusWorking {
			t.Errorf("status = %q, want working (no writer is blocked any more)", got.Status)
		}
		if len(got.Pending) != 0 {
			t.Errorf("pending writers = %v, want none", pendingWriterNames(got))
		}
	})

	t.Run("should attribute the held line to the writer still blocked when a partial resolution lands", func(t *testing.T) {
		// A permission==permission edge carrying an APPROVE rule reads as a
		// contradiction to `switchboard-ctl diagnose`, and case 18 has no other way to
		// announce itself. The reason names who is still waiting.
		_, s := twoBlocked(t)
		buf := captureLog(t)

		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "Bash",
			ToolInputHash: siblingHash, AgentID: blockedWriter})

		rec := lastDecision(t, buf.String())
		if !rec.Hold || rec.Rule != statustune.RuleHoldOtherWriter {
			t.Errorf("decision = %+v, want a hold under %s", rec, statustune.RuleHoldOtherWriter)
		}
		if !strings.Contains(rec.Raw, state.PendingWriterMain+" still blocked") {
			t.Errorf("the held line must name the writer still waiting; got:\n%s", rec.Raw)
		}
		if rec.Pending != "AskUserQuestion+1" {
			t.Errorf("pending = %q, want the pre-decision snapshot AskUserQuestion+1", rec.Pending)
		}
	})

	t.Run("should not let one writer's resolution drop another writer's entry", func(t *testing.T) {
		// The removal is per-writer (DropPending), not a whole-map wipe: the entry a
		// clear removes must be exactly the one the evidence named.
		store, s := twoBlocked(t)

		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "Bash",
			ToolInputHash: siblingHash, AgentID: blockedWriter})

		got := claudeStatus(t, store)
		if p, ok := got.Pending[""]; !ok || p.Tool != "AskUserQuestion" || p.InputHash != pendingHash {
			t.Errorf("main thread's prompt = %+v (present=%v), want it untouched", p, ok)
		}
	})
}

// Trap 2 (plan §9.6): a hydrated entry carries ownership and nothing else, so the
// `tool_name != ""` guard makes it unmatchable and it must resolve by transcript.
// Relaxing the rule to an agent_id-only match — "a blocked writer runs no tools" —
// is the 2026-08-05 bug at a narrower radius, because Claude Code emits parallel
// tool_use blocks and a writer can complete an auto-approved sibling while its own
// prompt still waits.
func TestClearsPermissionNeverFastPathsAHydratedEntry(t *testing.T) {
	cases := []struct {
		name string
		tool string
		hash string
	}{
		{name: "a matching-looking tool with an input hash", tool: "Bash", hash: pendingHash},
		{name: "a different tool", tool: "Read", hash: siblingHash},
		{name: "no tool name at all", tool: "", hash: ""},
		{name: "a tool with no input hash", tool: "Bash", hash: ""},
	}
	for _, tc := range cases {
		t.Run("should hold red when the owning writer sends "+tc.name+" against a hydrated entry", func(t *testing.T) {
			// Hydrated: ownership only. Since is the onset the daemon re-stamped at
			// startup; the writer's own jsonl is stale, so nothing resolves it here.
			store, s := fannedRedSession(t, 0,
				map[string]state.PendingPrompt{blockedWriter: {Since: promptSince}},
				map[string]string{blockedWriter: siblingResultOnly})

			s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42,
				ToolName: tc.tool, ToolInputHash: tc.hash, AgentID: blockedWriter})

			got := claudeStatus(t, store)
			if got.Status != state.StatusPermission {
				t.Errorf("status = %q, want permission (a hydrated entry has no correlators to match)", got.Status)
			}
			wantWriters(t, got, blockedWriter)
		})
	}

	t.Run("should still clear a hydrated entry once the owning writer's own transcript shows it resumed", func(t *testing.T) {
		// The cost §9.5 accepts: one transcript check instead of a hook-speed match —
		// and the transcript that answers it is the WRITER's, not the parent's.
		store, s := fannedRedSession(t, 0,
			map[string]state.PendingPrompt{blockedWriter: {Since: promptSince}},
			map[string]string{blockedWriter: turnResumed})

		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "Read",
			AgentID: blockedWriter})

		got := claudeStatus(t, store)
		if got.Status != state.StatusWorking {
			t.Errorf("status = %q, want working (the writer's own transcript resolved it)", got.Status)
		}
		if len(got.Pending) != 0 {
			t.Errorf("pending writers = %v, want none", pendingWriterNames(got))
		}
	})

	t.Run("should still clear a hydrated MAIN-thread entry once the main transcript shows the turn resumed", func(t *testing.T) {
		// Trap 3 of §9.6: main-thread entries are falsified against <session>.jsonl,
		// which SubagentPath returns unchanged for the empty key — so the routing adds
		// no branch and costs the common case nothing.
		store, s := fannedRedSession(t, 0,
			map[string]state.PendingPrompt{"": {Since: promptSince}},
			map[string]string{"": turnResumed, blockedWriter: siblingResultOnly})

		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "Read"})

		got := claudeStatus(t, store)
		if got.Status != state.StatusWorking {
			t.Errorf("status = %q, want working (the main thread's own transcript resolved its own prompt)", got.Status)
		}
		if len(got.Pending) != 0 {
			t.Errorf("pending writers = %v, want none", pendingWriterNames(got))
		}
	})
}
