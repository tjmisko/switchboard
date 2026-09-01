package transcript

import (
	"path/filepath"
	"testing"
	"time"
)

const (
	// questionTail is what a waiting AskUserQuestion leaves in the writer's own
	// file: the pending tool_use, flushed a beat after the hook and unmatched for
	// the whole wait, beside an auto-approved sibling that already came back.
	questionTail = `{"type":"assistant","timestamp":"2026-08-31T10:00:00Z","message":{"role":"assistant","stop_reason":null,"content":[{"type":"tool_use","id":"toolu_question","name":"AskUserQuestion","input":{}}]}}
{"type":"assistant","timestamp":"2026-08-31T10:00:00Z","message":{"role":"assistant","stop_reason":"tool_use","content":[{"type":"tool_use","id":"toolu_sibling","name":"Read","input":{}}]}}
{"type":"user","timestamp":"2026-08-31T10:00:02Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_sibling"}]}}
`
	// answeredQuestion is the same writer after the user picked an option: the
	// result lands 20–101 ms after the keypress and carries the call's own id.
	answeredQuestion = questionTail + `{"type":"user","timestamp":"2026-08-31T10:04:00Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_question","content":"chose option A"}]}}
`
	// declinedQuestion is the corpus shape of a rejection, verbatim from
	// ~/.claude/projects: is_error on the block, plus the two entry-level fields.
	declinedQuestion = questionTail + `{"type":"user","timestamp":"2026-08-31T10:04:00Z","toolUseResult":"User rejected tool use","toolDenialKind":"user-rejected","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_question","is_error":true,"content":"The user doesn't want to proceed with this tool use."}]}}
`
	// failedAfterApproval is the case is_error alone cannot tell from a decline:
	// the user APPROVED, the tool ran, and it exited non-zero. The turn resumed.
	failedAfterApproval = questionTail + `{"type":"user","timestamp":"2026-08-31T10:04:00Z","toolUseResult":"Error: Exit code 1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_question","is_error":true,"content":"Error: Exit code 1"}]}}
`
)

// The latch may bind an id only when the tail names exactly one candidate.
// Everything else — nothing yet, or two indistinguishable calls — has to reach
// the caller as a count, because binding the wrong id is a missed RED: the
// sibling's own result would then clear a prompt nobody answered.
func TestPendingCall(t *testing.T) {
	t.Run("should return the one unmatched call when the writer waits on a single question", func(t *testing.T) {
		got, err := PendingCall(writeWriterTail(t, questionTail), "AskUserQuestion", DefaultTailBytes)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 || got[0] != "toolu_question" {
			t.Fatalf("pending calls = %v, want just the gated question", got)
		}
	})

	t.Run("should ignore an unmatched call of a different tool when one waits", func(t *testing.T) {
		// The sibling Read is answered here, but the filter must be on the NAME:
		// a writer blocked on a question while a Bash still executes must not read
		// as ambiguous.
		body := questionTail + `{"type":"assistant","timestamp":"2026-08-31T10:00:03Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_bash","name":"Bash","input":{}}]}}` + "\n"
		got, err := PendingCall(writeWriterTail(t, body), "AskUserQuestion", DefaultTailBytes)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 || got[0] != "toolu_question" {
			t.Fatalf("pending calls = %v, want the question alone", got)
		}
	})

	t.Run("should return every candidate when one turn dispatched two calls of the same tool", func(t *testing.T) {
		body := questionTail + `{"type":"assistant","timestamp":"2026-08-31T10:00:01Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_question_b","name":"AskUserQuestion","input":{}}]}}` + "\n"
		got, err := PendingCall(writeWriterTail(t, body), "AskUserQuestion", DefaultTailBytes)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("pending calls = %v, want both candidates so the caller can refuse to guess", got)
		}
	})

	t.Run("should return nothing when the call already has its result", func(t *testing.T) {
		got, err := PendingCall(writeWriterTail(t, answeredQuestion), "AskUserQuestion", DefaultTailBytes)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("pending calls = %v, want none — an answered call must never be latched", got)
		}
	})

	t.Run("should return nothing when the tool_use has not been flushed yet", func(t *testing.T) {
		// The ~5 s flush lag. Nothing to bind yet; the caller retries next tick.
		got, err := PendingCall(writeWriterTail(t, ""), "AskUserQuestion", DefaultTailBytes)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("pending calls = %v, want none", got)
		}
	})

	t.Run("should return an error when the writer's transcript cannot be read", func(t *testing.T) {
		if _, err := PendingCall(filepath.Join(t.TempDir(), "nope.jsonl"), "AskUserQuestion", DefaultTailBytes); err == nil {
			t.Fatal("want a non-nil error so the caller keeps the prompt unbound")
		}
	})
}

// The id-matched tool_result is the second signal Phase 4 buys. It is decisive
// about the call it names and about nothing else.
func TestResolveKindFor(t *testing.T) {
	// Deliberately AFTER every entry in the fixtures: the id rule must not be
	// gated on it, and a since-gated implementation fails every case below.
	since := time.Date(2026, 8, 31, 23, 0, 0, 0, time.UTC)

	t.Run("should resolve resumed when the answered call's result is clean", func(t *testing.T) {
		got, err := ResolveKindFor(writeWriterTail(t, answeredQuestion), since, "toolu_question", DefaultTailBytes)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != ResolutionResumed {
			t.Fatalf("kind = %s, want resumed", got)
		}
	})

	t.Run("should resolve declined when the user rejected the call", func(t *testing.T) {
		got, err := ResolveKindFor(writeWriterTail(t, declinedQuestion), since, "toolu_question", DefaultTailBytes)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != ResolutionDeclined {
			t.Fatalf("kind = %s, want declined so the chip exits to idle rather than green", got)
		}
	})

	t.Run("should resolve resumed when the approved call ran and failed", func(t *testing.T) {
		// is_error is set here too. Reading it alone as a decline would send a
		// working session to idle on every failing command.
		got, err := ResolveKindFor(writeWriterTail(t, failedAfterApproval), since, "toolu_question", DefaultTailBytes)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != ResolutionResumed {
			t.Fatalf("kind = %s, want resumed — an approved tool that failed still resumed the turn", got)
		}
	})

	t.Run("should resolve nothing when only a sibling call has come back", func(t *testing.T) {
		// The bare-tool_result rule this package rejects would clear here. The id
		// is what makes the narrow rule safe.
		got, err := ResolveKindFor(writeWriterTail(t, questionTail), since, "toolu_question", DefaultTailBytes)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != ResolutionNone {
			t.Fatalf("kind = %s, want none while the question still waits", got)
		}
	})

	t.Run("should fall back to the timestamp rules when no call id is bound", func(t *testing.T) {
		// The callID == "" wrapper is every existing caller's path, so it must stay
		// exactly ResolveKind — including on a file whose id-matched result exists.
		early := time.Date(2026, 8, 31, 9, 0, 0, 0, time.UTC)
		for name, body := range map[string]string{
			"an answered question": answeredQuestion,
			"a declined question":  declinedQuestion,
		} {
			path := writeWriterTail(t, body)
			bare, err := ResolveKindFor(path, early, "", DefaultTailBytes)
			if err != nil {
				t.Fatalf("%s: unexpected error: %v", name, err)
			}
			wrapped, err := ResolveKind(path, early, DefaultTailBytes)
			if err != nil {
				t.Fatalf("%s: unexpected error: %v", name, err)
			}
			if bare != wrapped {
				t.Fatalf("%s: bare = %s, wrapper = %s; the wrapper must be byte-identical", name, bare, wrapped)
			}
			if bare == ResolutionDeclined {
				t.Fatalf("%s: an id-less caller must never see the declined kind", name)
			}
		}
	})

	t.Run("should return an error when the transcript cannot be read", func(t *testing.T) {
		got, err := ResolveKindFor(filepath.Join(t.TempDir(), "nope.jsonl"), since, "toolu_question", DefaultTailBytes)
		if err == nil {
			t.Fatal("want a non-nil error so the caller applies its backstop")
		}
		if got != ResolutionNone {
			t.Fatalf("kind = %s, want none on an unreadable file", got)
		}
	})
}
