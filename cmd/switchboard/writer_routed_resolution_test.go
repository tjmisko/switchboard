package main

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/transcript"
)

// T9 (docs/subagent-permission-plan.md §3.3, docs/subagent-permission-oscillation.md
// §3.5 defect 4): the reconciler must resolve each pending prompt against the
// transcript of the WRITER that raised it.
//
// Defect 4 is the reason the project's goal was not met. ResolveKind reports
// ResolutionResumed for any assistant message dated after the prompt, so asking
// the MAIN transcript about a teammate's prompt means a main thread that merely
// keeps working emits a stream of "the prompt resolved" evidence. The target
// behavior — a subagent prompt turns the chip red even while the main thread is
// working — is impossible until that routing is fixed, which is what the first
// test below pins.

const routedSID = "5318eb5b-79df-4dee-a9f8-c80df4eca79e"

// routedTeammate is the agent id of the writer that raised the prompt in the
// 2026-08-05 incident (`escalate-cleanup`).
const routedTeammate = "af5bd126402ac16c7"

// at renders an entry timestamp offset from the prompt's onset.
func at(since time.Time, d time.Duration) string {
	return since.Add(d).UTC().Format(time.RFC3339Nano)
}

// tToolUse is the assistant tool_use a writer emits at dispatch — the last thing a
// writer blocked on a permission prompt ever writes. Fixtures date it BEFORE the
// prompt's onset, which is its measured shape: the pending tool_use entry is dated
// 6-374 ms before its PermissionRequest hook (plan §9.7), and the hook stamps
// PendingPrompt.Since from the wall clock precisely so a turn's own entries cannot
// read as post-prompt signals (H7/H8).
func tToolUse(ts string) string {
	return `{"type":"assistant","timestamp":"` + ts + `","message":{"role":"assistant","stop_reason":"tool_use","content":[{"type":"tool_use","id":"toolu_gated","name":"Bash"}]}}`
}

// routedBlocked is a writer parked on its own prompt: it dispatched a tool just
// before the prompt was raised and has written nothing since.
func routedBlocked(since time.Time) string {
	return tToolUse(at(since, -400*time.Millisecond)) + "\n"
}

// routedWorking is a writer advancing its own thread: assistant messages dated
// well after the prompt's onset. Against its OWN prompt this is resolution;
// against anyone else's it is noise.
func routedWorking(since time.Time) string {
	return tAssistant(at(since, 5*time.Second)) + "\n" +
		tAssistant(at(since, 20*time.Second)) + "\n" +
		tResult(at(since, 25*time.Second)) + "\n"
}

// routedFixture lays out one session's transcript tree — <sid>.jsonl beside
// <sid>/subagents/agent-<id>.jsonl — and stamps every file's mtime, so T10's
// quiescence clock is the test's to control rather than the wall clock's. A writer
// with an empty body gets no file at all (the "writer is gone" shape).
func routedFixture(t *testing.T, bodies map[string]string, mtime time.Time) string {
	t.Helper()
	main := hydrateFixture(t, routedSID, bodies)
	for writer, body := range bodies {
		if body == "" {
			continue
		}
		path := transcript.SubagentPath(main, writer)
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatalf("chtimes %s: %v", path, err)
		}
	}
	return main
}

// routedSession builds the locked session map the reconcile Apply hands
// selfHealStaleAttention: one red chip whose Pending map names `writers`, every
// prompt dated `since`.
func routedSession(mainTranscript string, since time.Time, writers ...string) map[int]*state.Session {
	info := &state.AgentInfo{
		SessionID:   routedSID,
		Transcript:  mainTranscript,
		Status:      state.StatusPermission,
		StatusSince: since,
	}
	for _, w := range writers {
		info.SetPending(w, state.PendingPrompt{Tool: "Bash", InputHash: "cafe1234", Since: since})
	}
	return map[int]*state.Session{100: {PID: 100, Agent: state.AgentKindClaude, CWD: "/home/u/proj", Claude: info}}
}

func TestSelfHealRoutesResolutionToTheRaisingWriter(t *testing.T) {
	since := mustParse(t, "2026-08-05T19:38:13Z")
	now := since.Add(time.Minute)

	t.Run("should keep a teammate's prompt red when the main thread keeps working", func(t *testing.T) {
		// THE deliverable (defect 4). The main thread is advancing — three entries
		// dated after the prompt, any one of which ResolveKind calls
		// ResolutionResumed — while the teammate that raised the prompt has been
		// silent since it dispatched. Routed to the main file this exits to green
		// within one tick; routed to the raising writer's own file it holds.
		main := routedFixture(t, map[string]string{
			"":             routedWorking(since),
			routedTeammate: routedBlocked(since),
		}, since)
		m := routedSession(main, since, routedTeammate)
		m[100].Claude.InFlightSubagents = 4

		selfHealStaleAttention(m, now, testTune, nil)

		if got := m[100].Claude.Status; got != state.StatusPermission {
			t.Errorf("status = %q, want permission — main-thread activity is not evidence about a teammate's prompt", got)
		}
		if want := []string{routedTeammate}; !equalStrings(pendingWriterSet(m[100].Claude), want) {
			t.Errorf("pending writers = %v, want %v", pendingWriterSet(m[100].Claude), want)
		}
	})

	t.Run("should keep a teammate's prompt red however long the main thread keeps working", func(t *testing.T) {
		// The incident's cadence: the main thread (or its teammates) keeps writing
		// every few seconds for minutes. Every tick re-reads, so holding once is not
		// enough — it must hold on every one of them.
		main := routedFixture(t, map[string]string{
			"":             routedWorking(since),
			routedTeammate: routedBlocked(since),
		}, since)
		m := routedSession(main, since, routedTeammate)
		m[100].Claude.InFlightSubagents = 4

		for tick := 5 * time.Second; tick <= 95*time.Second; tick += 5 * time.Second {
			selfHealStaleAttention(m, since.Add(tick), testTune, nil)
			if got := m[100].Claude.Status; got != state.StatusPermission {
				t.Fatalf("status = %q at tick %v, want permission for the whole wait", got, tick)
			}
		}
	})

	t.Run("should clear a teammate's prompt when that teammate's own transcript resumes", func(t *testing.T) {
		// The mirror image of the hold, and the measured resolution signal: the
		// quiescent writer resumes. The main thread is silent here, so the ONLY
		// evidence is the teammate's own file.
		main := routedFixture(t, map[string]string{
			"":             routedBlocked(since),
			routedTeammate: routedWorking(since),
		}, since)
		m := routedSession(main, since, routedTeammate)
		m[100].Claude.InFlightSubagents = 4

		selfHealStaleAttention(m, now, testTune, nil)

		if got := m[100].Claude.Status; got != state.StatusWorking {
			t.Errorf("status = %q, want working (approved → the raising writer resumed → green)", got)
		}
		if len(m[100].Claude.Pending) != 0 {
			t.Errorf("pending writers = %v, want none", pendingWriterSet(m[100].Claude))
		}
	})

	t.Run("should keep the main thread's prompt red when only a teammate is advancing", func(t *testing.T) {
		// Routing runs both ways. A teammate's activity is not evidence about the
		// main thread's prompt either, and crossing the two here would clear a red
		// the user is still looking at.
		main := routedFixture(t, map[string]string{
			"":             routedBlocked(since),
			routedTeammate: routedWorking(since),
		}, since)
		m := routedSession(main, since, "")
		m[100].Claude.InFlightSubagents = 2

		selfHealStaleAttention(m, now, testTune, nil)

		if got := m[100].Claude.Status; got != state.StatusPermission {
			t.Errorf("status = %q, want permission", got)
		}
		if want := []string{state.PendingWriterMain}; !equalStrings(pendingWriterSet(m[100].Claude), want) {
			t.Errorf("pending writers = %v, want %v", pendingWriterSet(m[100].Claude), want)
		}
	})

	t.Run("should keep the main thread's prompt red when a teammate is interrupted", func(t *testing.T) {
		// An interrupt notice is the strongest resolution signal there is — and it
		// still only resolves the writer whose file carries it.
		main := routedFixture(t, map[string]string{
			"":             routedBlocked(since),
			routedTeammate: tInterrupt(at(since, 10*time.Second)) + "\n",
		}, since)
		m := routedSession(main, since, "")

		selfHealStaleAttention(m, now, testTune, nil)

		if got := m[100].Claude.Status; got != state.StatusPermission {
			t.Errorf("status = %q, want permission — a teammate's Esc did not answer the main thread's prompt", got)
		}
	})

	t.Run("should keep the chip red when one of two blocked writers resolves", func(t *testing.T) {
		// Case 18. Both the main thread and a teammate are blocked; the teammate's
		// prompt is answered and its thread resumes. One answer, two prompts — the
		// chip must stay red, and the surviving entry must be the main thread's.
		var buf bytes.Buffer
		defer log.SetOutput(log.Writer())
		log.SetOutput(&buf)

		main := routedFixture(t, map[string]string{
			"":             routedBlocked(since),
			routedTeammate: routedWorking(since),
		}, since)
		m := routedSession(main, since, "", routedTeammate)
		m[100].Claude.InFlightSubagents = 4

		selfHealStaleAttention(m, now, testTune, nil)

		if got := m[100].Claude.Status; got != state.StatusPermission {
			t.Errorf("status = %q, want permission — the main thread is still blocked", got)
		}
		if want := []string{state.PendingWriterMain}; !equalStrings(pendingWriterSet(m[100].Claude), want) {
			t.Errorf("pending writers = %v, want %v (only the resolved writer may be dropped)", pendingWriterSet(m[100].Claude), want)
		}
		if !strings.Contains(buf.String(), "rule=case18-hold-other-writers") {
			t.Errorf("missing the case-18 hold line in:\n%s", buf.String())
		}
	})

	t.Run("should exit permission when the second of two blocked writers finally resolves", func(t *testing.T) {
		// The other half of case 18: the chip leaves red only once the map is empty,
		// and the exit color comes from the last prompt's resolution kind.
		main := routedFixture(t, map[string]string{
			"":             routedWorking(since),
			routedTeammate: routedWorking(since),
		}, since)
		m := routedSession(main, since, "", routedTeammate)

		selfHealStaleAttention(m, now, testTune, nil)

		if got := m[100].Claude.Status; got != state.StatusWorking {
			t.Errorf("status = %q, want working", got)
		}
		if len(m[100].Claude.Pending) != 0 {
			t.Errorf("pending writers = %v, want none", pendingWriterSet(m[100].Claude))
		}
	})

	t.Run("should keep a teammate's prompt when that teammate's transcript is unreadable", func(t *testing.T) {
		// No agent-<id>.jsonl at all. Unreadable is not evidence of resolution, and
		// case 15's 30s TTL deliberately does NOT reach a subagent writer: an absent
		// agent file is the normal state for a just-spawned teammate and for any id
		// the mapping cannot resolve, so releasing red on it would be a missed RED.
		// The clock that DOES bound it is T10's cap, 60x longer.
		main := routedFixture(t, map[string]string{"": routedWorking(since)}, since)
		m := routedSession(main, since, routedTeammate)

		selfHealStaleAttention(m, since.Add(2*testTune.PermissionDecayTTL), testTune, nil)

		if got := m[100].Claude.Status; got != state.StatusPermission {
			t.Errorf("status = %q, want permission (no evidence is not evidence of resolution)", got)
		}
		if want := []string{routedTeammate}; !equalStrings(pendingWriterSet(m[100].Claude), want) {
			t.Errorf("pending writers = %v, want %v", pendingWriterSet(m[100].Claude), want)
		}
	})

	t.Run("should still release a main-thread prompt when the main transcript is unreadable past the ttl", func(t *testing.T) {
		// Case 15 is unchanged for the writer it was written for. c.Transcript is the
		// file every other signal is derived from, so its unreadability is a
		// session-level fault with a session-level fail-soft.
		m := routedSession("/no/such/transcript.jsonl", since, "")

		selfHealStaleAttention(m, since.Add(2*testTune.PermissionDecayTTL), testTune, nil)

		if got := m[100].Claude.Status; got != state.StatusIdle {
			t.Errorf("status = %q, want idle (case-15 backstop)", got)
		}
	})

	t.Run("should record the released prompt's tool on the exit edge", func(t *testing.T) {
		// The exit is now decided AFTER the entries are removed (the chip cannot know
		// it is leaving red until the map is empty), so the tool name has to be
		// carried to the history event explicitly. Without it the one edge that
		// explains a released red records `pending=""`.
		histDir := t.TempDir()
		sink := history.NewSink(history.Config{Enabled: true, Detail: history.DetailFull, Dir: histDir})

		main := routedFixture(t, map[string]string{
			"":             routedBlocked(since),
			routedTeammate: routedWorking(since),
		}, since)
		m := routedSession(main, since, routedTeammate)

		selfHealStaleAttention(m, now, testTune, sink)
		sink.Close()

		edges := eventsOfType(readEvents(t, histDir), history.EventTransition)
		if len(edges) != 1 {
			t.Fatalf("recorded %d transition events, want 1", len(edges))
		}
		if edges[0].Pending != "Bash" {
			t.Errorf("exit edge pending = %q, want %q", edges[0].Pending, "Bash")
		}
	})

	t.Run("should resolve an ownerless red against the main transcript", func(t *testing.T) {
		// A permission chip with no Pending entry is a pre-T5 artifact. Treating it
		// as a main-thread prompt reproduces exactly the behavior this daemon had
		// before the map existed; skipping it would strand the chip red forever.
		main := routedFixture(t, map[string]string{"": routedWorking(since)}, since)
		m := routedSession(main, since)

		selfHealStaleAttention(m, now, testTune, nil)

		if got := m[100].Claude.Status; got != state.StatusWorking {
			t.Errorf("status = %q, want working", got)
		}
	})
}
