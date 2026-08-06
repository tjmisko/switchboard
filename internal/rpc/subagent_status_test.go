package rpc

import (
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

// T17 — defect 2 of docs/subagent-permission-oscillation.md: every subagent hook
// used to be attributed to the parent's chip, because switchboard-ctl identifies
// its caller with getppid() and subagents run in-process. A teammate's PostToolUse
// therefore painted the MAIN thread green every few seconds, which is the engine
// of the §3.4 limit cycle: the reconciler always had a working chip to demote.
// §2.4 is the proof it is the surviving engine — the 12:39:56 restart removed the
// stale anchor and the flap only slowed from the 5s tick to the 15s grace.
//
// The discriminator is agent_id (normalized once in handleHook): non-empty means
// the hook fired inside a subagent.

// teammate is a normalized agent_id off the incident: `wire-frontmatter`, one of
// the noisy teammates, NOT the blocked `escalate-cleanup`.
const teammate = "aa83942381ce15c04"

// chipSession seeds a live Claude session parked on `status` since `since`, with
// `subagents` teammates in flight and no transcript on disk — so any status the
// chip ends up with came from the hook path under test and not from a transcript
// read.
func chipSession(t *testing.T, status string, subagents int, since time.Time) (*state.Store, *Server) {
	t.Helper()
	store := state.New("")
	store.Apply(func(m map[int]*state.Session) {
		m[42] = &state.Session{PID: 42, CWD: "/home/u/proj", Agent: state.AgentKindClaude,
			Claude: &state.AgentInfo{
				SessionID: "5318eb5b-79df", Status: status, StatusSince: since,
				Transcript: filepath.Join(t.TempDir(), "absent.jsonl"), InFlightSubagents: subagents,
			}}
	})
	return store, New(store, "", terminal.NewNone(), wm.NewNone())
}

// The core of T17, stated as the two halves of one rule: a teammate's tool
// completion says nothing about the main thread, and a main thread's says
// everything.
func TestHandleHookIgnoresTeammateActivityOnTheParentChip(t *testing.T) {
	t.Run("should leave an idle chip idle when a teammate PostToolUse arrives", func(t *testing.T) {
		since := time.Now().Add(-time.Minute)
		store, s := chipSession(t, state.StatusIdle, 4, since)

		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "Bash", AgentID: teammate})

		got := claudeStatus(t, store)
		if got.Status != state.StatusIdle {
			t.Errorf("status = %q, want idle (a teammate's tool completion is not the main thread working)", got.Status)
		}
		if !got.StatusSince.Equal(since) {
			t.Errorf("StatusSince = %v, want the untouched %v — a suppressed event must not re-stamp the age the reconciler's grace is measured from",
				got.StatusSince, since)
		}
	})

	t.Run("should move an idle chip to working when a main-thread PostToolUse arrives", func(t *testing.T) {
		// The other half. Empty agent_id means MAIN THREAD, and the main thread's
		// own tool completion is exactly the signal the chip exists to show.
		store, s := chipSession(t, state.StatusIdle, 4, time.Now().Add(-time.Minute))

		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "Bash"})

		if got := claudeStatus(t, store); got.Status != state.StatusWorking {
			t.Errorf("status = %q, want working (the main thread's own PostToolUse still drives the chip)", got.Status)
		}
	})

	t.Run("should leave a delegating chip delegating when a teammate PostToolUse arrives", func(t *testing.T) {
		// This is the limit cycle, closed. Delegating is inert to case6-idle-title
		// (cmd/switchboard/main.go gates that demotion on working), so as long as a
		// teammate cannot knock the chip into working there is nothing for the
		// reconciler to demote and the orange/green cycle has no engine.
		store, s := chipSession(t, state.StatusDelegating, 4, time.Now().Add(-time.Minute))

		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "Bash", AgentID: teammate})

		if got := claudeStatus(t, store); got.Status != state.StatusDelegating {
			t.Errorf("status = %q, want delegating (teammate traffic must not convert delegation into main-thread work)", got.Status)
		}
	})

	t.Run("should leave a working chip working when a teammate Stop arrives", func(t *testing.T) {
		// The mirror hazard: a subagent's lifecycle event must not idle a parent
		// that is genuinely mid-turn. Stop maps to idle for whoever fired it, and
		// whoever fired it here is not the thread the chip represents.
		store, s := chipSession(t, state.StatusWorking, 4, time.Now().Add(-time.Minute))

		s.handleHook(Request{Cmd: "hook", Event: "Stop", PID: 42, AgentID: teammate})

		if got := claudeStatus(t, store); got.Status != state.StatusWorking {
			t.Errorf("status = %q, want working (a teammate finishing is not the main turn ending)", got.Status)
		}
	})

	t.Run("should still record the session identity when a teammate event is suppressed", func(t *testing.T) {
		// Suppression is narrow: it removes the STATUS the event would have driven,
		// not the enrichment it carries. Everything else handleHook does today must
		// still happen.
		store, s := chipSession(t, state.StatusIdle, 4, time.Now().Add(-time.Minute))

		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "Bash",
			AgentID: teammate, SessionID: "6605c9cd-1111", Transcript: "/t/6605c9cd.jsonl"})

		got := claudeStatus(t, store)
		if got.SessionID != "6605c9cd-1111" {
			t.Errorf("SessionID = %q, want the rotated id 6605c9cd-1111", got.SessionID)
		}
		if got.Transcript != "/t/6605c9cd.jsonl" {
			t.Errorf("Transcript = %q, want the refreshed path", got.Transcript)
		}
	})
}

// The carve-out that keeps the whole document's target behavior reachable: a
// subagent-raised prompt is blocking the USER, and the user has one chip per
// session to look at (status-color-state-model.md §5 case 16). Raising red is the
// one thing a teammate legitimately says about its parent.
func TestHandleHookLetsATeammateStillRaiseRed(t *testing.T) {
	for _, from := range []string{state.StatusWorking, state.StatusIdle, state.StatusDelegating} {
		t.Run("should turn the chip red when a teammate PermissionRequest arrives on a "+from+" chip", func(t *testing.T) {
			store, s := chipSession(t, from, 4, time.Now().Add(-time.Minute))

			s.handleHook(Request{Cmd: "hook", Event: "PermissionRequest", PID: 42, ToolName: "Bash",
				AgentID: "af5bd126402ac16c7"}) // escalate-cleanup, the blocked one

			got := claudeStatus(t, store)
			if got.Status != state.StatusPermission {
				t.Errorf("status = %q, want permission (a subagent prompt blocks the user and must surface)", got.Status)
			}
			if got.PendingTool != "Bash" {
				t.Errorf("PendingTool = %q, want Bash (the prompt's tool must still be captured for the clear rule)", got.PendingTool)
			}
		})
	}
}

// The S dimension is what LEGITIMATELY paints a delegating parent green, so
// narrowing the status path must not starve it. SubagentStart/Stop already map to
// "" and are untouched by the narrowing; they still trigger the fanout re-scan
// that recomputes InFlightSubagents at hook speed.
func TestHandleHookKeepsFeedingFanoutFromTeammateEvents(t *testing.T) {
	// seedFanout lays out the subagents/ dir the Observer scans: one running
	// depth-0 teammate beside the main transcript.
	seedFanout := func(t *testing.T, sid string, running bool) string {
		t.Helper()
		base := t.TempDir()
		tpath := filepath.Join(base, sid+".jsonl")
		subdir := filepath.Join(base, sid, "subagents")
		if err := os.MkdirAll(subdir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(tpath, []byte(siblingResultOnly), 0o644); err != nil {
			t.Fatal(err)
		}
		meta := `{"agentType":"general-purpose","description":"wire-frontmatter","spawnDepth":1,"toolUseId":"toolu_a1"}`
		if err := os.WriteFile(filepath.Join(subdir, "agent-"+teammate+".meta.json"), []byte(meta), 0o644); err != nil {
			t.Fatal(err)
		}
		body := `{"type":"assistant","message":{"role":"assistant","stop_reason":null}}` + "\n"
		if !running {
			body = `{"type":"assistant","message":{"role":"assistant","stop_reason":"end_turn"}}` + "\n"
		}
		if err := os.WriteFile(filepath.Join(subdir, "agent-"+teammate+".jsonl"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return tpath
	}

	sid := "5318eb5b-79df-4dee-a9f8-c80df4eca79e"

	t.Run("should refresh the in-flight count when a teammate SubagentStart arrives on an idle chip", func(t *testing.T) {
		tpath := seedFanout(t, sid, true)
		store := state.New("")
		store.Apply(func(m map[int]*state.Session) {
			m[42] = &state.Session{PID: 42, CWD: "/p", Agent: state.AgentKindClaude, Claude: &state.AgentInfo{
				SessionID: sid, Status: state.StatusIdle, StatusSince: promptSince, Transcript: tpath,
			}}
		})
		s := New(store, "", terminal.NewNone(), wm.NewNone())
		s.SetFanout(fanout.NewObserver(t.TempDir()))

		s.handleHook(Request{Cmd: "hook", Event: "SubagentStart", PID: 42, SessionID: sid,
			Transcript: tpath, AgentID: teammate, AgentType: "general-purpose"})

		got := claudeStatus(t, store)
		if got.InFlightSubagents != 1 {
			t.Errorf("InFlightSubagents = %d, want 1 — the re-scan is how case5-delegating learns to paint the chip green, and it must survive the T17 narrowing",
				got.InFlightSubagents)
		}
		if got.Status != state.StatusIdle {
			t.Errorf("status = %q, want idle (the chip goes green from S on the reconcile tick, not from the lifecycle hook)", got.Status)
		}
	})

	t.Run("should drain the in-flight count when a teammate SubagentStop arrives", func(t *testing.T) {
		tpath := seedFanout(t, sid, false)
		store := state.New("")
		store.Apply(func(m map[int]*state.Session) {
			m[42] = &state.Session{PID: 42, CWD: "/p", Agent: state.AgentKindClaude, Claude: &state.AgentInfo{
				SessionID: sid, Status: state.StatusDelegating, StatusSince: promptSince, Transcript: tpath,
				InFlightSubagents: 1,
			}}
		})
		s := New(store, "", terminal.NewNone(), wm.NewNone())
		s.SetFanout(fanout.NewObserver(t.TempDir()))

		s.handleHook(Request{Cmd: "hook", Event: "SubagentStop", PID: 42, SessionID: sid,
			Transcript: tpath, AgentID: teammate, AgentType: "general-purpose"})

		if got := claudeStatus(t, store); got.InFlightSubagents != 0 {
			t.Errorf("InFlightSubagents = %d, want 0 (the finished teammate must still be counted out)", got.InFlightSubagents)
		}
	})
}

// The red chip keeps its own door. clearsPermission (T2/T3) is the single exit
// from permission and it already refuses teammate events with a logged reason;
// T17 deliberately does not open a second, silent one, so the hold and its
// forensics must read exactly as they did before.
func TestHandleHookLeavesTheRedHoldToTheGate(t *testing.T) {
	t.Run("should hold red when a teammate PostToolUse arrives while a prompt is pending", func(t *testing.T) {
		store, s := redSession(t, "Bash", 4, siblingResultOnly)
		buf := captureLog(t)

		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "Bash", AgentID: teammate})

		got := claudeStatus(t, store)
		if got.Status != state.StatusPermission {
			t.Errorf("status = %q, want permission", got.Status)
		}
		if !got.StatusSince.Equal(promptSince) {
			t.Errorf("StatusSince = %v, want the prompt's onset %v", got.StatusSince, promptSince)
		}
		if out := buf.String(); !strings.Contains(out, "rule="+statustune.RuleHoldTeammateCollision) {
			t.Errorf("the hold must still explain itself on a parseable decision line; got:\n%s", out)
		}
	})

	t.Run("should hold red when a teammate Stop arrives while a prompt is pending", func(t *testing.T) {
		store, s := redSession(t, "Bash", 4, siblingResultOnly)
		buf := captureLog(t)

		s.handleHook(Request{Cmd: "hook", Event: "Stop", PID: 42, AgentID: teammate})

		if got := claudeStatus(t, store); got.Status != state.StatusPermission {
			t.Errorf("status = %q, want permission", got.Status)
		}
		if out := buf.String(); !strings.Contains(out, "rule="+statustune.RuleHoldNonToolEvent) {
			t.Errorf("a non-tool event under the hold must still be routed through the gate; got:\n%s", out)
		}
	})
}

// Safe degradation: agent_id is the ONLY discriminator, so if it ever fails to
// arrive on a subagent path the daemon must land exactly on today's behavior
// rather than on a guess. Every event's empty-agent_id mapping, pinned.
func TestHandleHookIsUnchangedWhenAgentIDIsEmpty(t *testing.T) {
	cases := []struct {
		event string
		from  string
		want  string
	}{
		{event: "PostToolUse", from: state.StatusIdle, want: state.StatusWorking},
		{event: "PostToolUse", from: state.StatusDelegating, want: state.StatusWorking},
		{event: "UserPromptSubmit", from: state.StatusIdle, want: state.StatusWorking},
		{event: "Stop", from: state.StatusWorking, want: state.StatusIdle},
		{event: "SessionStart", from: state.StatusWorking, want: state.StatusIdle},
		{event: "PermissionRequest", from: state.StatusWorking, want: state.StatusPermission},
		// Unmapped for claude: no status, so the chip is left where it was.
		{event: "SubagentStart", from: state.StatusIdle, want: state.StatusIdle},
		{event: "SubagentStop", from: state.StatusWorking, want: state.StatusWorking},
	}
	for _, tc := range cases {
		t.Run("should map "+tc.event+" to "+tc.want+" when the hook carries no agent_id on a "+tc.from+" chip", func(t *testing.T) {
			store, s := chipSession(t, tc.from, 4, time.Now().Add(-time.Minute))

			s.handleHook(Request{Cmd: "hook", Event: tc.event, PID: 42, ToolName: "Bash"})

			if got := claudeStatus(t, store); got.Status != tc.want {
				t.Errorf("status = %q, want %q (an empty agent_id must behave exactly as it did before T17)", got.Status, tc.want)
			}
		})
	}
}
