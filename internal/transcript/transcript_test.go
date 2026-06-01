package transcript

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Fixtures mirror the real Claude Code .jsonl shape: every entry has a top-level
// timestamp; a resolution is a user entry carrying a tool_result block.

func resultLine(ts string) string {
	return fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_X"}]}}`, ts)
}

func assistantUse(ts string) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_X","name":"AskUserQuestion"}]}}`, ts)
}

const noise = `{"type":"last-prompt","lastPrompt":"hi"}` + "\n" +
	`{"type":"custom-title","customTitle":"x"}` + "\n" +
	`{"type":"attachment"}`

func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse time %q: %v", s, err)
	}
	return ts
}

func TestResolutionState(t *testing.T) {
	since := mustTime(t, "2026-06-01T21:39:00Z")

	tests := []struct {
		name  string
		lines []string
		want  PromptState
	}{
		{
			name:  "should be resolved when a tool_result is dated after the prompt appeared (declined/answered)",
			lines: []string{assistantUse("2026-06-01T21:39:01Z"), resultLine("2026-06-01T21:42:30Z")},
			want:  StateResolved,
		},
		{
			name:  "should be pending when the newest tool_result predates the prompt (unflushed pending prompt)",
			lines: []string{resultLine("2026-06-01T21:36:45Z"), noise},
			want:  StatePending,
		},
		{
			name:  "should be pending when there is no tool_result at all (fresh prompt)",
			lines: []string{noise, `{"type":"assistant","timestamp":"2026-06-01T21:39:30Z","message":{"role":"assistant","content":[{"type":"text"}]}}`},
			want:  StatePending,
		},
		{
			name:  "should track the newest tool_result across several",
			lines: []string{resultLine("2026-06-01T21:30:00Z"), resultLine("2026-06-01T21:45:00Z"), resultLine("2026-06-01T21:31:00Z")},
			want:  StateResolved,
		},
		{
			name:  "should be pending when every tool_result predates the prompt",
			lines: []string{resultLine("2026-06-01T21:30:00Z"), resultLine("2026-06-01T21:38:59Z")},
			want:  StatePending,
		},
		{
			name:  "should tolerate blank, malformed, and timestamp-less lines",
			lines: []string{"", "not json", `{"type":"x"}`, resultLine("2026-06-01T21:40:00Z")},
			want:  StateResolved,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolutionState(writeTranscript(t, tt.lines...), since, DefaultTailBytes)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("ResolutionState = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResolutionStateFailsSoft(t *testing.T) {
	since := mustTime(t, "2026-06-01T21:39:00Z")
	t.Run("should return unknown and an error for an empty path", func(t *testing.T) {
		got, err := ResolutionState("", since, DefaultTailBytes)
		if got != StateUnknown || err == nil {
			t.Errorf("got (%v, %v), want (unknown, error)", got, err)
		}
	})
	t.Run("should return unknown and an error for a missing file", func(t *testing.T) {
		got, err := ResolutionState(filepath.Join(t.TempDir(), "nope.jsonl"), since, DefaultTailBytes)
		if got != StateUnknown || err == nil {
			t.Errorf("got (%v, %v), want (unknown, error)", got, err)
		}
	})
}

// TestResolutionStateTailWindow verifies the partial-first-line drop: with a
// tiny window the read starts mid-file, and the leading fragment must not break
// parsing of the genuine resolution at the end.
func TestResolutionStateTailWindow(t *testing.T) {
	since := mustTime(t, "2026-06-01T21:39:00Z")
	var lines []string
	for i := 0; i < 200; i++ {
		lines = append(lines, noise)
	}
	lines = append(lines, resultLine("2026-06-01T21:45:00Z"))
	path := writeTranscript(t, lines...)

	got, err := ResolutionState(path, since, 256) // window smaller than the file
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != StateResolved {
		t.Errorf("ResolutionState (small window) = %v, want resolved", got)
	}
}
