package rpc

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/fanout"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/statustune"
	"github.com/tjmisko/switchboard/internal/terminal"
	"github.com/tjmisko/switchboard/internal/wm"
)

// The 2026-08-05 incident, in fixtures (docs/subagent-permission-oscillation.md):
// a subagent raised a Bash approval prompt and blocked, while three teammates kept
// running their own Bash calls. `promptSince` is when the chip went red; the
// transcript fixtures below are dated around it.
var promptSince = time.Date(2026, 6, 22, 10, 50, 41, 0, time.UTC)

const (
	// siblingResultOnly: the prompt's own turn predates the prompt, and the only
	// thing newer is a bare tool_result from a concurrent tool. Not a resolution.
	siblingResultOnly = `{"type":"assistant","timestamp":"2026-06-22T10:50:30Z","message":{"role":"assistant","content":[{"type":"text","text":"let me ask"}]}}
{"type":"user","timestamp":"2026-06-22T10:50:42Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_sibling"}]}}
`
	// turnResumed: an assistant message after the prompt — the turn resumed, so
	// the prompt was answered.
	turnResumed = `{"type":"assistant","timestamp":"2026-06-22T10:50:30Z","message":{"role":"assistant","content":[{"type":"text","text":"let me ask"}]}}
{"type":"assistant","timestamp":"2026-06-22T10:50:55Z","message":{"role":"assistant","content":[{"type":"text","text":"thanks, continuing"}]}}
`
)

// redSession seeds a session already parked on a permission prompt for `pending`,
// with `subagents` teammates in flight and the given transcript body (empty body =>
// no transcript file at all, so the fallback read fails and the gate must hold).
func redSession(t *testing.T, pending string, subagents int, body string) (*state.Store, *Server) {
	t.Helper()
	tpath := filepath.Join(t.TempDir(), "transcript.jsonl")
	if body != "" {
		if err := os.WriteFile(tpath, []byte(body), 0o644); err != nil {
			t.Fatalf("write transcript fixture: %v", err)
		}
	}
	store := state.New("")
	store.Apply(func(m map[int]*state.Session) {
		m[42] = &state.Session{PID: 42, CWD: "/home/u/proj", Claude: &state.AgentInfo{
			SessionID: "5318eb5b-79df", Status: state.StatusPermission, StatusSince: promptSince,
			Transcript: tpath, PendingTool: pending, InFlightSubagents: subagents,
		}}
	})
	return store, New(store, "", terminal.NewNone(), wm.NewNone())
}

// captureLog redirects the standard logger into a buffer for the duration of the
// test, so a decision line can be asserted on.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

func claudeStatus(t *testing.T, store *state.Store) state.AgentInfo {
	t.Helper()
	sessions := store.Snapshot().Sessions
	if len(sessions) != 1 || sessions[0].Claude == nil {
		t.Fatalf("want exactly one session with a claude block, got %+v", sessions)
	}
	return *sessions[0].Claude
}

// T2 — the 12:38:21 edge, verbatim. A teammate's Bash PostToolUse carries the same
// tool_name as the pending Bash prompt; with subagents in flight that is a tool
// KIND collision, not the approved tool completing, so the fast path must be
// refused and the (non-resolving) transcript fallback must keep the chip red.
func TestHandleHookHoldsRedOnTeammateToolNameCollision(t *testing.T) {
	t.Run("should hold red when a matching tool_name arrives while subagents are in flight", func(t *testing.T) {
		store, s := redSession(t, "Bash", 4, siblingResultOnly)
		buf := captureLog(t)

		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "Bash",
			AgentID: "aa83942381ce15c04"}) // wire-frontmatter, not the blocked escalate-cleanup

		got := claudeStatus(t, store)
		if got.Status != state.StatusPermission {
			t.Errorf("status = %q, want permission (a teammate's Bash must not clear a pending Bash prompt)", got.Status)
		}
		if got.PendingTool != "Bash" {
			t.Errorf("PendingTool = %q, want Bash (still pending)", got.PendingTool)
		}
		if out := buf.String(); !strings.Contains(out, "rule="+statustune.RuleHoldTeammateCollision) {
			t.Errorf("missing %s decision line in:\n%s", statustune.RuleHoldTeammateCollision, out)
		}
	})

	t.Run("should hold red when a matching tool_name arrives with no agent_id and subagents are in flight", func(t *testing.T) {
		// The R1 floor: if agent_id ever arrives empty on a teammate's tool event,
		// the guard must still hold — it keys off the in-flight count alone.
		store, s := redSession(t, "Bash", 1, siblingResultOnly)

		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "Bash"})

		if got := claudeStatus(t, store); got.Status != state.StatusPermission {
			t.Errorf("status = %q, want permission", got.Status)
		}
	})

	t.Run("should clear red at hook speed when a matching tool_name arrives with no subagents in flight", func(t *testing.T) {
		// The Phase A latency win: with S == 0 a tool-name match is nearly sound,
		// and it must still clear without waiting for the transcript. No transcript
		// file exists here, so a clear can only have come from the fast path.
		store, s := redSession(t, "Bash", 0, "")
		buf := captureLog(t)

		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "Bash"})

		got := claudeStatus(t, store)
		if got.Status != state.StatusWorking {
			t.Errorf("status = %q, want working (tool-name match with S==0 clears at hook speed)", got.Status)
		}
		if got.PendingTool != "" {
			t.Errorf("PendingTool = %q, want cleared on leaving red", got.PendingTool)
		}
		if out := buf.String(); !strings.Contains(out, "rule="+statustune.RuleApproveToolMatch) {
			t.Errorf("missing %s decision line in:\n%s", statustune.RuleApproveToolMatch, out)
		}
	})

	t.Run("should still clear red via the transcript when subagents are in flight and the turn resumed", func(t *testing.T) {
		// Refusing the fast path must not disable the fallback beneath it.
		store, s := redSession(t, "Bash", 4, turnResumed)
		buf := captureLog(t)

		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "Bash"})

		if got := claudeStatus(t, store); got.Status != state.StatusWorking {
			t.Errorf("status = %q, want working (transcript shows the turn resumed)", got.Status)
		}
		if out := buf.String(); !strings.Contains(out, "rule="+statustune.RuleApproveTranscript) {
			t.Errorf("want the clear attributed to the transcript fallback, got:\n%s", out)
		}
	})
}

// T3 — defect 5: the hold gate used to guard only PostToolUse, so every other
// event walked the chip out of permission unguarded. While a prompt is pending,
// no event may repaint the chip except through clearsPermission.
func TestHandleHookHoldsRedAcrossEveryHookEvent(t *testing.T) {
	// UserPromptSubmit is deliberately included (plan Q6): queueing a message
	// while a prompt waits is common, so typing is not treated as an answer.
	events := []string{"Stop", "UserPromptSubmit", "SessionStart"}
	for _, ev := range events {
		for _, subagents := range []int{0, 4} {
			name := "should hold red when a main-thread " + ev + " arrives while a prompt is pending"
			if subagents > 0 {
				name += " with teammates in flight"
			}
			t.Run(name, func(t *testing.T) {
				store, s := redSession(t, "Bash", subagents, siblingResultOnly)
				buf := captureLog(t)

				s.handleHook(Request{Cmd: "hook", Event: ev, PID: 42})

				got := claudeStatus(t, store)
				if got.Status != state.StatusPermission {
					t.Errorf("status after %s = %q, want permission", ev, got.Status)
				}
				if !got.StatusSince.Equal(promptSince) {
					t.Errorf("StatusSince = %v, want the prompt's onset %v (a held event must not re-stamp the age)", got.StatusSince, promptSince)
				}
				if out := buf.String(); !strings.Contains(out, "rule="+statustune.RuleHoldNonToolEvent) {
					t.Errorf("missing %s decision line in:\n%s", statustune.RuleHoldNonToolEvent, out)
				}
			})
		}
	}

	t.Run("should let a Stop close the chip when the transcript shows the turn resumed", func(t *testing.T) {
		// clearsPermission stays the single door out — including for non-tool
		// events, so an answered prompt whose turn ends without another tool call
		// still lands on the event's own status rather than waiting for the
		// reconciler.
		store, s := redSession(t, "AskUserQuestion", 0, turnResumed)
		buf := captureLog(t)

		s.handleHook(Request{Cmd: "hook", Event: "Stop", PID: 42})

		if got := claudeStatus(t, store); got.Status != state.StatusIdle {
			t.Errorf("status = %q, want idle (Stop after a resumed turn)", got.Status)
		}
		if out := buf.String(); !strings.Contains(out, "permission->idle") {
			t.Errorf("want a permission->idle decision line, got:\n%s", out)
		}
	})
}

// A fresh PermissionRequest maps to "permission" itself, so it can never move the
// chip off red and must not be swallowed by the generalized gate: it still lands,
// and it still stamps the tool the prompt was raised for.
func TestHandleHookAcceptsPermissionRequestUnderTheHold(t *testing.T) {
	t.Run("should stamp the pending tool when a PermissionRequest lands on a working chip", func(t *testing.T) {
		store := state.New("")
		store.Apply(func(m map[int]*state.Session) {
			m[42] = &state.Session{PID: 42, CWD: "/p", Claude: &state.AgentInfo{
				Status: state.StatusWorking, InFlightSubagents: 4,
			}}
		})
		s := New(store, "", terminal.NewNone(), wm.NewNone())

		s.handleHook(Request{Cmd: "hook", Event: "PermissionRequest", PID: 42, ToolName: "Bash",
			AgentID: "af5bd126402ac16c7"})

		got := claudeStatus(t, store)
		if got.Status != state.StatusPermission {
			t.Errorf("status = %q, want permission", got.Status)
		}
		if got.PendingTool != "Bash" {
			t.Errorf("PendingTool = %q, want Bash", got.PendingTool)
		}
	})

	t.Run("should keep the chip red when a second PermissionRequest arrives during a pending prompt", func(t *testing.T) {
		store, s := redSession(t, "Bash", 4, turnResumed) // resumable transcript: a gated event WOULD clear
		buf := captureLog(t)

		s.handleHook(Request{Cmd: "hook", Event: "PermissionRequest", PID: 42, ToolName: "Edit"})

		got := claudeStatus(t, store)
		if got.Status != state.StatusPermission {
			t.Errorf("status = %q, want permission", got.Status)
		}
		if strings.Contains(buf.String(), "rule=") {
			t.Errorf("a PermissionRequest must not be routed through the hold gate, got:\n%s", buf.String())
		}
	})
}

// Codex records no approvals in its rollout, so there is nothing to resolve a
// codex red against — it stays exempt from the hold and advances on its own hooks.
func TestHandleHookLeavesCodexExemptFromTheHold(t *testing.T) {
	t.Run("should advance a codex chip out of permission when its next hook arrives", func(t *testing.T) {
		store := state.New("")
		store.Apply(func(m map[int]*state.Session) {
			m[7] = &state.Session{PID: 7, Agent: state.AgentKindCodex, CWD: "/p", Codex: &state.AgentInfo{
				Status: state.StatusPermission, StatusSince: promptSince, PendingTool: "shell",
			}}
		})
		s := New(store, "", terminal.NewNone(), wm.NewNone())

		s.handleHook(Request{Cmd: "hook", Agent: "codex", Event: "Stop", PID: 7})

		if got := store.Snapshot().Sessions[0].Codex.Status; got != state.StatusIdle {
			t.Errorf("codex status = %q, want idle (codex is exempt from the permission hold)", got)
		}
	})
}

// SubagentStart/Stop map to "" (status unchanged), so they fall past the hold gate
// to the fanout re-scan — a red chip must keep learning how many teammates are in
// flight, since that count is what the hold itself keys off.
func TestHandleHookTriggersFanoutRescanWhileRed(t *testing.T) {
	t.Run("should refresh the in-flight count when a SubagentStart arrives while the chip is red", func(t *testing.T) {
		base := t.TempDir()
		sid := "5318eb5b-79df-4dee-a9f8-c80df4eca79e"
		tpath := filepath.Join(base, sid+".jsonl")
		subdir := filepath.Join(base, sid, "subagents")
		if err := os.MkdirAll(subdir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(tpath, []byte(siblingResultOnly), 0o644); err != nil {
			t.Fatal(err)
		}
		meta := `{"agentType":"general-purpose","description":"escalate-cleanup","spawnDepth":1,"toolUseId":"toolu_a1"}`
		if err := os.WriteFile(filepath.Join(subdir, "agent-af5bd126.meta.json"), []byte(meta), 0o644); err != nil {
			t.Fatal(err)
		}
		running := `{"type":"assistant","message":{"role":"assistant","stop_reason":null}}` + "\n"
		if err := os.WriteFile(filepath.Join(subdir, "agent-af5bd126.jsonl"), []byte(running), 0o644); err != nil {
			t.Fatal(err)
		}

		store := state.New("")
		store.Apply(func(m map[int]*state.Session) {
			m[42] = &state.Session{PID: 42, CWD: "/p", Claude: &state.AgentInfo{
				SessionID: sid, Status: state.StatusPermission, StatusSince: promptSince,
				Transcript: tpath, PendingTool: "Bash",
			}}
		})
		s := New(store, "", terminal.NewNone(), wm.NewNone())
		s.SetFanout(fanout.NewObserver(t.TempDir()))

		s.handleHook(Request{Cmd: "hook", Event: "SubagentStart", PID: 42, SessionID: sid,
			Transcript: tpath, AgentID: "af5bd126", AgentType: "general-purpose"})

		got := claudeStatus(t, store)
		if got.Status != state.StatusPermission {
			t.Errorf("status = %q, want permission (a lifecycle hook must not repaint a red chip)", got.Status)
		}
		if got.InFlightSubagents != 1 {
			t.Errorf("InFlightSubagents = %d, want 1 (the fanout re-scan must still run under the hold)", got.InFlightSubagents)
		}
	})
}

// T1 — instrumentation. agent_id is the field that will key the per-writer prompt
// map (plan T5), and it has never been observed live. Log it exactly where it
// decides something, on its own line so `switchboard-ctl diagnose` (which parses
// the `status: pid=` shape) is unaffected.
func TestHandleHookLogsIncomingAgentID(t *testing.T) {
	t.Run("should log the agent_id when a PermissionRequest arrives", func(t *testing.T) {
		store := state.New("")
		store.Apply(func(m map[int]*state.Session) {
			m[42] = &state.Session{PID: 42, CWD: "/p"}
		})
		s := New(store, "", terminal.NewNone(), wm.NewNone())
		buf := captureLog(t)

		s.handleHook(Request{Cmd: "hook", Event: "PermissionRequest", PID: 42, ToolName: "Bash",
			SessionID: "5318eb5b-79df", AgentID: "af5bd126402ac16c7", AgentType: "general-purpose"})

		out := buf.String()
		if !strings.Contains(out, `hook-identity: pid=42`) || !strings.Contains(out, `agent_id="af5bd126402ac16c7"`) {
			t.Errorf("missing hook-identity line carrying the agent_id in:\n%s", out)
		}
		if !strings.Contains(out, "event=PermissionRequest") {
			t.Errorf("hook-identity line must name the event, got:\n%s", out)
		}
	})

	t.Run("should log the agent_id on a PostToolUse only while the chip is red", func(t *testing.T) {
		_, s := redSession(t, "Bash", 4, siblingResultOnly)
		buf := captureLog(t)

		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "Bash",
			AgentID: "aa83942381ce15c04"})
		if !strings.Contains(buf.String(), `agent_id="aa83942381ce15c04"`) {
			t.Errorf("want the teammate's agent_id logged during a pending prompt, got:\n%s", buf.String())
		}

		// The same hook on a working chip is the high-volume case and must stay quiet.
		store2 := state.New("")
		store2.Apply(func(m map[int]*state.Session) {
			m[42] = &state.Session{PID: 42, CWD: "/p", Claude: &state.AgentInfo{Status: state.StatusWorking}}
		})
		s2 := New(store2, "", terminal.NewNone(), wm.NewNone())
		buf2 := captureLog(t)

		s2.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "Bash", AgentID: "aa83942381ce15c04"})
		if strings.Contains(buf2.String(), "hook-identity") {
			t.Errorf("PostToolUse on a non-red chip must not log identity (volume), got:\n%s", buf2.String())
		}
	})
}

// The decision lines the hold emits must remain parseable by the forensic tooling —
// `switchboard-ctl diagnose` reads them back through statustune.ParseDecision, and
// the held events are exactly the lines an investigator greps for.
func TestHoldDecisionLinesRemainParseable(t *testing.T) {
	t.Run("should emit a parseable decision line when an event is held", func(t *testing.T) {
		_, s := redSession(t, "Bash", 4, siblingResultOnly)
		buf := captureLog(t)

		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "Bash"})

		var rec statustune.Record
		found := false
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if r, ok := statustune.ParseDecision(line); ok {
				rec, found = r, true
			}
		}
		if !found {
			t.Fatalf("no parseable decision line in:\n%s", buf.String())
		}
		if !rec.Hold || rec.From != state.StatusPermission || rec.To != state.StatusPermission {
			t.Errorf("decision = %+v, want a permission==permission hold", rec)
		}
		if rec.Rule != statustune.RuleHoldTeammateCollision || rec.Subagents != 4 || rec.Pending != "Bash" {
			t.Errorf("decision = %+v, want rule=%s S=4 pending=Bash", rec, statustune.RuleHoldTeammateCollision)
		}
	})
}
