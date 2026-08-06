package transcript

import (
	"os"
	"path/filepath"
	"testing"
)

// writeWriterTail drops one writer's jsonl fixture in a temp dir and returns its
// path. Unlike the line-joining writeTranscript helper it writes the body
// verbatim, so a test can pin an empty or deliberately malformed file.
func writeWriterTail(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "writer.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write transcript fixture: %v", err)
	}
	return path
}

const (
	// blockedTail is the shape a blocked writer leaves on disk, taken from the
	// incident's agent-af5bd126402ac16c7.jsonl: an assistant tool_use with
	// stop_reason "tool_use" and no tool_result for the whole wait.
	blockedTail = `{"type":"assistant","timestamp":"2026-06-22T10:50:30Z","message":{"role":"assistant","content":[{"type":"text","text":"running it"}]}}
{"type":"assistant","timestamp":"2026-06-22T10:50:31Z","message":{"role":"assistant","stop_reason":"tool_use","content":[{"type":"tool_use","id":"toolu_gated","name":"Bash"}]}}
`
	// resolvedTail is the same writer after the user answered: the tool ran and
	// its result landed.
	resolvedTail = blockedTail + `{"type":"user","timestamp":"2026-06-22T10:55:04Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_gated"}]}}
`
	// parallelTail is trap 3a, verbatim: one assistant message dispatches a gated
	// tool AND an auto-approved sibling, and only the sibling's result comes back.
	// The TRAILING tool_use is matched while the prompt is still waiting.
	parallelTail = `{"type":"assistant","timestamp":"2026-06-22T10:50:31Z","message":{"role":"assistant","stop_reason":null,"content":[{"type":"tool_use","id":"toolu_gated","name":"Bash"}]}}
{"type":"assistant","timestamp":"2026-06-22T10:50:31Z","message":{"role":"assistant","stop_reason":"tool_use","content":[{"type":"tool_use","id":"toolu_sibling","name":"Read"}]}}
{"type":"user","timestamp":"2026-06-22T10:50:33Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_sibling"}]}}
`
)

// The falsifier is sound only in one direction: absence of an unmatched tool_use
// proves the writer is no longer blocked, while its presence proves nothing (a
// dispatched tool covers "awaiting approval" AND "executing right now"). Every
// case below is therefore about what may be DROPPED.
func TestBlockedByPendingTool(t *testing.T) {
	t.Run("should report blocked when the writer's tail holds an unmatched tool_use", func(t *testing.T) {
		got, err := BlockedByPendingTool(writeWriterTail(t, blockedTail), DefaultTailBytes)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != BlockedYes {
			t.Errorf("evidence = %s, want blocked", got)
		}
	})

	t.Run("should report unblocked when every dispatched tool in the tail has its result", func(t *testing.T) {
		got, err := BlockedByPendingTool(writeWriterTail(t, resolvedTail), DefaultTailBytes)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != BlockedNo {
			t.Errorf("evidence = %s, want unblocked (the tool returned, so its gate opened)", got)
		}
	})

	t.Run("should report blocked when the trailing tool_use is matched but an earlier parallel sibling is not", func(t *testing.T) {
		// Trap 3a. A trailing-only test reads this tail as resolved and drops a red
		// that is still genuinely waiting — the missed RED the constraint exists to
		// prevent. Parallel dispatch is common, not exotic.
		got, err := BlockedByPendingTool(writeWriterTail(t, parallelTail), DefaultTailBytes)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != BlockedYes {
			t.Errorf("evidence = %s, want blocked — the gated tool_use is unmatched even though the LAST one is not", got)
		}
	})

	t.Run("should report unknown when the transcript cannot be read", func(t *testing.T) {
		for name, path := range map[string]string{
			"a missing file": filepath.Join(t.TempDir(), "nope.jsonl"),
			"an empty path":  "",
		} {
			got, err := BlockedByPendingTool(path, DefaultTailBytes)
			if err == nil {
				t.Errorf("%s: want a non-nil error for the caller's log", name)
			}
			if got != BlockedUnknown {
				t.Errorf("%s: evidence = %s, want unknown so the caller keeps the entry", name, got)
			}
		}
	})

	t.Run("should report unknown when the tail window holds no tool_use at all", func(t *testing.T) {
		// The pending tool_use may simply have scrolled past the window, and a
		// truncated tail is indistinguishable from a writer that dispatched nothing.
		// Both must fail closed — matching permissionExit's unreadable handling.
		for name, body := range map[string]string{
			"an empty file":           "",
			"conversation only":       `{"type":"assistant","timestamp":"2026-06-22T10:50:30Z","message":{"role":"assistant","content":[{"type":"text","text":"thinking"}]}}` + "\n",
			"an orphan tool_result":   `{"type":"user","timestamp":"2026-06-22T10:50:33Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_scrolled_away"}]}}` + "\n",
			"unparseable stray lines": "not json at all\n{\n",
		} {
			got, err := BlockedByPendingTool(writeWriterTail(t, body), DefaultTailBytes)
			if err != nil {
				t.Errorf("%s: unexpected error: %v", name, err)
			}
			if got != BlockedUnknown {
				t.Errorf("%s: evidence = %s, want unknown so the caller keeps the entry", name, got)
			}
		}
	})

	t.Run("should report unknown when the window is too small to reach the tool_use", func(t *testing.T) {
		// A tail bound that lands mid-file drops the partial first line, so a
		// tool_use just outside the window is invisible. That must never read as
		// "resolved".
		path := writeWriterTail(t, resolvedTail)
		got, err := BlockedByPendingTool(path, 160)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == BlockedNo {
			t.Errorf("evidence = %s from a window that cannot see the tool_use, want anything but unblocked", got)
		}
	})
}
