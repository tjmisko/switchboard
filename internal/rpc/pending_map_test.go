package rpc

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/statustune"
	"github.com/tjmisko/switchboard/internal/terminal"
	"github.com/tjmisko/switchboard/internal/wm"
)

// T5 (docs/subagent-permission-plan.md §3): a session is 1 + N concurrent writers,
// so the prompt state must be OWNED by the writer that raised it. The scalar this
// replaced could hold exactly one prompt, which is how a teammate's tool came to
// clear a prompt it had nothing to do with.

// pendingWriterNames renders a block's Pending key set for assertions, with the
// main thread's empty key spelled out.
func pendingWriterNames(info state.AgentInfo) []string {
	names := info.PendingWriterKeys()
	for i, n := range names {
		if n == "" {
			names[i] = state.PendingWriterMain
		}
	}
	sort.Strings(names)
	return names
}

func wantWriters(t *testing.T, info state.AgentInfo, want ...string) {
	t.Helper()
	got := pendingWriterNames(info)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("pending writers = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("pending writers = %v, want %v", got, want)
		}
	}
}

func TestHandleHookKeysPendingByTheRaisingWriter(t *testing.T) {
	t.Run("should key the prompt on the main thread when a PermissionRequest carries no agent_id", func(t *testing.T) {
		store := state.New("")
		store.Apply(func(m map[int]*state.Session) { m[42] = &state.Session{PID: 42, CWD: "/p"} })
		s := New(store, "", terminal.NewNone(), wm.NewNone())

		s.handleHook(Request{Cmd: "hook", Event: "PermissionRequest", PID: 42,
			ToolName: "AskUserQuestion", ToolInputHash: "cafe1234"})

		got := claudeStatus(t, store)
		wantWriters(t, got, state.PendingWriterMain)
		p := got.Pending[""]
		if p.Tool != "AskUserQuestion" || p.InputHash != "cafe1234" || p.Since.IsZero() {
			t.Errorf("prompt = %+v, want the tool, the input hash and an onset", p)
		}
	})

	t.Run("should key the prompt on the subagent when a PermissionRequest carries an agent_id", func(t *testing.T) {
		store := state.New("")
		store.Apply(func(m map[int]*state.Session) { m[42] = &state.Session{PID: 42, CWD: "/p"} })
		s := New(store, "", terminal.NewNone(), wm.NewNone())

		s.handleHook(Request{Cmd: "hook", Event: "PermissionRequest", PID: 42, ToolName: "Bash",
			AgentID: "af5bd126402ac16c7"})

		wantWriters(t, claudeStatus(t, store), "af5bd126402ac16c7")
	})

	t.Run("should key the prompt on the bare id when the hook sends the on-disk agent- spelling", func(t *testing.T) {
		// Normalization happens exactly once, at handleHook's entry, so the map joins
		// whatever spelling arrives with the prefix-stripped ids the fanout Observer
		// writes. Nothing downstream may strip again.
		store := state.New("")
		store.Apply(func(m map[int]*state.Session) { m[42] = &state.Session{PID: 42, CWD: "/p"} })
		s := New(store, "", terminal.NewNone(), wm.NewNone())

		s.handleHook(Request{Cmd: "hook", Event: "PermissionRequest", PID: 42, ToolName: "Bash",
			AgentID: "agent-af5bd126402ac16c7"})

		wantWriters(t, claudeStatus(t, store), "af5bd126402ac16c7")
	})

	t.Run("should record a second writer when a PermissionRequest arrives on an already-red chip", func(t *testing.T) {
		// The case the scalar could not represent at all: two writers blocked at
		// once. status == info.Status here, so the transition block is skipped and an
		// entry written there would simply be lost.
		store, s := redSession(t, "Bash", 4, siblingResultOnly)
		store.Apply(func(m map[int]*state.Session) {
			m[42].Claude.SetPending("af5bd126402ac16c7", state.PendingPrompt{Tool: "Bash", Since: promptSince})
		})

		s.handleHook(Request{Cmd: "hook", Event: "PermissionRequest", PID: 42, ToolName: "AskUserQuestion"})

		got := claudeStatus(t, store)
		if got.Status != state.StatusPermission {
			t.Errorf("status = %q, want permission", got.Status)
		}
		wantWriters(t, got, state.PendingWriterMain, "af5bd126402ac16c7")
		if !got.StatusSince.Equal(promptSince) {
			t.Errorf("StatusSince = %v, want the first prompt's onset %v (a second prompt must not reset the age)", got.StatusSince, promptSince)
		}
	})

	t.Run("should record no prompt for a codex PermissionRequest", func(t *testing.T) {
		// Codex records no approvals in its rollout and is exempt from the hold gate,
		// so an entry written for it would never be resolved by anything — and would
		// latch a hydrated red forever after the next restart.
		store := state.New("")
		store.Apply(func(m map[int]*state.Session) {
			m[7] = &state.Session{PID: 7, Agent: state.AgentKindCodex, CWD: "/p"}
		})
		s := New(store, "", terminal.NewNone(), wm.NewNone())

		s.handleHook(Request{Cmd: "hook", Agent: "codex", Event: "PermissionRequest", PID: 7, ToolName: "shell"})

		codex := store.Snapshot().Sessions[0].Codex
		if codex.Status != state.StatusPermission {
			t.Errorf("codex status = %q, want permission (the chip still turns red)", codex.Status)
		}
		if len(codex.Pending) != 0 {
			t.Errorf("codex Pending = %v, want empty", codex.Pending)
		}
	})
}

func TestHandleHookDropsPendingOnResolutionAndRotation(t *testing.T) {
	t.Run("should forget every owner when the chip leaves permission", func(t *testing.T) {
		store := state.New("")
		store.Apply(func(m map[int]*state.Session) { m[42] = &state.Session{PID: 42, CWD: "/p"} })
		s := New(store, "", terminal.NewNone(), wm.NewNone())

		s.handleHook(Request{Cmd: "hook", Event: "PermissionRequest", PID: 42, ToolName: "AskUserQuestion"})
		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "AskUserQuestion"})

		got := claudeStatus(t, store)
		if got.Status != state.StatusWorking {
			t.Errorf("status = %q, want working", got.Status)
		}
		if len(got.Pending) != 0 {
			t.Errorf("pending writers = %v, want none once the chip is out of red", pendingWriterNames(got))
		}
	})

	t.Run("should hold red and keep the owner when the gate refuses to clear", func(t *testing.T) {
		store, s := redSession(t, "Bash", 4, siblingResultOnly)
		store.Apply(func(m map[int]*state.Session) {
			m[42].Claude.SetPending("af5bd126402ac16c7", state.PendingPrompt{Tool: "Bash", Since: promptSince})
		})

		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "Bash",
			AgentID: "aa83942381ce15c04"}) // a teammate's Bash, not the blocked writer's

		got := claudeStatus(t, store)
		if got.Status != state.StatusPermission {
			t.Errorf("status = %q, want permission", got.Status)
		}
		wantWriters(t, got, "af5bd126402ac16c7")
	})

	t.Run("should drop every owner when the session rotates under a stable pid", func(t *testing.T) {
		// A /clear or a fork keeps the pid but takes a new session_id and a new
		// transcript. Nothing in the new session can ever resolve the retired
		// session's prompt, and T3's broadened hold covers SessionStart — which is
		// exactly the event a rotation announces itself with.
		store, s := redSession(t, "Bash", 0, siblingResultOnly)
		store.Apply(func(m map[int]*state.Session) {
			m[42].Claude.SetPending("", state.PendingPrompt{Tool: "Bash", Since: promptSince})
		})

		s.handleHook(Request{Cmd: "hook", Event: "SessionStart", PID: 42,
			SessionID: "6605c9cd-1111", Transcript: "/t/6605c9cd.jsonl"})

		got := claudeStatus(t, store)
		if len(got.Pending) != 0 {
			t.Errorf("pending writers = %v, want none — the prompts died with the session that raised them", pendingWriterNames(got))
		}
		if got.Status != state.StatusIdle {
			t.Errorf("status = %q, want idle (the rotated session's red is not this session's red)", got.Status)
		}
		if got.SessionID != "6605c9cd-1111" {
			t.Errorf("SessionID = %q, want the rotated id", got.SessionID)
		}
	})

	t.Run("should keep holding red when a hook repeats the SAME session id", func(t *testing.T) {
		// The rotation clause must not become a second, unlogged door out of red.
		store, s := redSession(t, "Bash", 0, siblingResultOnly)
		store.Apply(func(m map[int]*state.Session) {
			m[42].Claude.SetPending("", state.PendingPrompt{Tool: "Bash", Since: promptSince})
		})

		s.handleHook(Request{Cmd: "hook", Event: "SessionStart", PID: 42, SessionID: "5318eb5b-79df"})

		got := claudeStatus(t, store)
		if got.Status != state.StatusPermission {
			t.Errorf("status = %q, want permission", got.Status)
		}
		wantWriters(t, got, state.PendingWriterMain)
	})

	t.Run("should keep holding red when a hook carries no session id at all", func(t *testing.T) {
		// Most hooks do not, and an absent id is not a rotation.
		store, s := redSession(t, "Bash", 0, siblingResultOnly)
		store.Apply(func(m map[int]*state.Session) {
			m[42].Claude.SetPending("", state.PendingPrompt{Tool: "Bash", Since: promptSince})
		})

		s.handleHook(Request{Cmd: "hook", Event: "SessionStart", PID: 42})

		if got := claudeStatus(t, store); got.Status != state.StatusPermission {
			t.Errorf("status = %q, want permission", got.Status)
		}
	})
}

// Trap 2 (§9.6): a hydrated entry carries ownership and nothing else — no Tool, no
// InputHash — so it must resolve by transcript only. The temptation is to "fix"
// the unmatchable name by relaxing the rule to an agent_id-only match, which is
// the 2026-08-05 bug at a narrower radius: Claude Code emits parallel tool_use
// blocks in one assistant message, so a writer can complete an auto-approved
// sibling while its own prompt still waits.
func TestHandleHookHoldsRedForAHydratedEntry(t *testing.T) {
	// hydratedRed is a red chip whose owner survived a restart with no correlators,
	// as dropStaleSessions leaves it: Pending has the writer, PendingTool is empty.
	hydratedRed := func(t *testing.T, writer string) (*state.Store, *Server) {
		t.Helper()
		store, s := redSession(t, "", 0, siblingResultOnly)
		store.Apply(func(m map[int]*state.Session) {
			c := m[42].Claude
			c.ClearPending()
			c.SetPending(writer, state.PendingPrompt{Since: time.Now()})
		})
		return store, s
	}

	t.Run("should not clear a hydrated red on a PostToolUse from the owning writer carrying a different tool", func(t *testing.T) {
		store, s := hydratedRed(t, "af5bd126402ac16c7")
		buf := captureLog(t)

		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "Read",
			AgentID: "af5bd126402ac16c7"})

		got := claudeStatus(t, store)
		if got.Status != state.StatusPermission {
			t.Errorf("status = %q, want permission — same writer, wrong tool is not resolution", got.Status)
		}
		wantWriters(t, got, "af5bd126402ac16c7")
		if out := buf.String(); !strings.Contains(out, "rule="+statustune.RuleHoldBareResult) {
			t.Errorf("want the hold attributed to a bare result, got:\n%s", out)
		}
	})

	t.Run("should not clear a hydrated red on any PostToolUse tool name, since no tool was persisted", func(t *testing.T) {
		// The empty PendingTool is what makes the fast path unmatchable, and the
		// `toolName != ""` guard is what keeps two empties from reading as a match.
		for _, tool := range []string{"Bash", "AskUserQuestion", ""} {
			store, s := hydratedRed(t, "")

			s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: tool})

			if got := claudeStatus(t, store); got.Status != state.StatusPermission {
				t.Errorf("status after a %q PostToolUse = %q, want permission", tool, got.Status)
			}
		}
	})

	t.Run("should still clear a hydrated red once the transcript proves the turn resumed", func(t *testing.T) {
		// The cost §9.5 accepts: a hydrated entry loses the hook-speed early clear
		// and resolves on the transcript path instead — one tick, bounded.
		store, s := redSession(t, "", 0, turnResumed)
		store.Apply(func(m map[int]*state.Session) {
			c := m[42].Claude
			c.ClearPending()
			c.SetPending("af5bd126402ac16c7", state.PendingPrompt{Since: time.Now()})
		})

		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "Read"})

		got := claudeStatus(t, store)
		if got.Status != state.StatusWorking {
			t.Errorf("status = %q, want working (transcript shows the turn resumed)", got.Status)
		}
		if len(got.Pending) != 0 {
			t.Errorf("pending writers = %v, want none", pendingWriterNames(got))
		}
	})
}

// The decision line is the project's diagnostic backbone, and the whole incident
// was reconstructed from it. A map behind the chip must not cost it fidelity.
func TestDecisionLogReportsEveryPendingWriter(t *testing.T) {
	t.Run("should report the bare tool when exactly one writer is blocked", func(t *testing.T) {
		store, s := redSession(t, "Bash", 4, siblingResultOnly)
		store.Apply(func(m map[int]*state.Session) {
			m[42].Claude.SetPending("af5bd126402ac16c7", state.PendingPrompt{Tool: "Bash", Since: promptSince})
		})
		buf := captureLog(t)

		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "Bash"})

		rec := lastDecision(t, buf.String())
		if rec.Pending != "Bash" {
			t.Errorf("pending = %q, want Bash — ParseDecision and `switchboard-ctl diagnose` read this field", rec.Pending)
		}
	})

	t.Run("should report the main thread's tool and a count when several writers are blocked", func(t *testing.T) {
		store, s := redSession(t, "Bash", 4, siblingResultOnly)
		store.Apply(func(m map[int]*state.Session) {
			c := m[42].Claude
			c.SetPending("", state.PendingPrompt{Tool: "AskUserQuestion", Since: promptSince})
			c.SetPending("af5bd126402ac16c7", state.PendingPrompt{Tool: "Bash", Since: promptSince})
		})
		buf := captureLog(t)

		s.handleHook(Request{Cmd: "hook", Event: "Stop", PID: 42})

		rec := lastDecision(t, buf.String())
		if rec.Pending != "AskUserQuestion+1" {
			t.Errorf("pending = %q, want AskUserQuestion+1 — a two-writer red must not report as a one-writer red", rec.Pending)
		}
	})
}

// lastDecision returns the final parseable decision line in a captured log.
func lastDecision(t *testing.T, out string) statustune.Record {
	t.Helper()
	var rec statustune.Record
	found := false
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if r, ok := statustune.ParseDecision(line); ok {
			rec, found = r, true
		}
	}
	if !found {
		t.Fatalf("no parseable decision line in:\n%s", out)
	}
	return rec
}
