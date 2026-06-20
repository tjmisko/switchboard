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
// timestamp. A permission prompt resolves when the main thread advances past it —
// an assistant message or a user interrupt notice — not when a bare tool_result
// (concurrent subagent / parallel-tool work) lands.

func resultLine(ts string) string {
	return fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_X"}]}}`, ts)
}

func assistantUse(ts string) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_X","name":"AskUserQuestion"}]}}`, ts)
}

const noise = `{"type":"last-prompt","lastPrompt":"hi"}` + "\n" +
	`{"type":"custom-title","customTitle":"x"}` + "\n" +
	`{"type":"attachment"}`

func assistantText(ts string) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`, ts)
}

func interruptLine(ts string) string {
	return fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user]"}]}}`, ts)
}

// userStringContent is a user entry whose content is a bare string, not an array
// of blocks — a shape Claude Code uses (e.g. command caveats) that the tail
// parser must tolerate rather than skip.
func userStringContent(ts, s string) string {
	return fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":%q}}`, ts, s)
}

// systemLine is a timestamped system entry with no message.role — ancillary, so
// it must not count as conversational activity.
func systemLine(ts string) string {
	return fmt.Sprintf(`{"type":"system","timestamp":%q,"content":"hook output"}`, ts)
}

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
			name:  "should be resolved when an assistant message advances past the prompt (approved → turn resumed)",
			lines: []string{assistantText("2026-06-01T21:36:45Z"), assistantUse("2026-06-01T21:42:30Z")},
			want:  StateResolved,
		},
		{
			name:  "should be resolved when a user interrupt notice is dated after the prompt (declined/interrupted)",
			lines: []string{assistantText("2026-06-01T21:38:00Z"), interruptLine("2026-06-01T21:42:30Z")},
			want:  StateResolved,
		},
		{
			// The regression: while the prompt is genuinely pending, a background
			// teammate/subagent (or a sibling auto-approved tool) keeps flushing
			// tool_results dated after it. None of that is the user's decision, so
			// the chip must stay red.
			name:  "should stay pending when only tool_results land after the prompt (concurrent subagent/parallel work)",
			lines: []string{assistantText("2026-06-01T21:38:00Z"), resultLine("2026-06-01T21:42:30Z"), resultLine("2026-06-01T21:43:00Z")},
			want:  StatePending,
		},
		{
			// A prior prompt's interrupt notice predates the current one; concurrent
			// tool_results post-date it. Neither resolves the current prompt.
			name:  "should stay pending when an old interrupt predates the prompt and only tool_results follow",
			lines: []string{interruptLine("2026-06-01T21:38:00Z"), resultLine("2026-06-01T21:43:00Z")},
			want:  StatePending,
		},
		{
			name:  "should be pending when the newest resolution entry predates the prompt (unflushed pending prompt)",
			lines: []string{assistantText("2026-06-01T21:36:45Z"), noise},
			want:  StatePending,
		},
		{
			name:  "should be pending when there is no resolution entry at all (fresh prompt)",
			lines: []string{noise, resultLine("2026-06-01T21:39:30Z")},
			want:  StatePending,
		},
		{
			name:  "should track the newest resolution entry across several",
			lines: []string{assistantText("2026-06-01T21:30:00Z"), interruptLine("2026-06-01T21:45:00Z"), assistantText("2026-06-01T21:31:00Z")},
			want:  StateResolved,
		},
		{
			name:  "should be pending when every resolution entry predates the prompt",
			lines: []string{assistantText("2026-06-01T21:30:00Z"), interruptLine("2026-06-01T21:38:59Z")},
			want:  StatePending,
		},
		{
			name:  "should tolerate blank, malformed, and timestamp-less lines",
			lines: []string{"", "not json", `{"type":"x"}`, interruptLine("2026-06-01T21:40:00Z")},
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

func TestNewestSignal(t *testing.T) {
	tests := []struct {
		name     string
		lines    []string
		wantKind Signal
		wantTS   string // "" means the zero time
	}{
		{
			name:     "should report activity when the newest entry is an assistant message",
			lines:    []string{assistantUse("2026-06-01T21:39:01Z"), assistantText("2026-06-01T21:39:05Z")},
			wantKind: SignalActivity,
			wantTS:   "2026-06-01T21:39:05Z",
		},
		{
			name:     "should report activity when the newest entry is a user tool_result",
			lines:    []string{resultLine("2026-06-01T21:40:00Z")},
			wantKind: SignalActivity,
			wantTS:   "2026-06-01T21:40:00Z",
		},
		{
			name:     "should report interrupt when the newest entry is an interrupt notice",
			lines:    []string{assistantText("2026-06-01T21:39:05Z"), interruptLine("2026-06-01T21:39:10Z")},
			wantKind: SignalInterrupt,
			wantTS:   "2026-06-01T21:39:10Z",
		},
		{
			name:     "should report activity when work resumes after an interrupt",
			lines:    []string{interruptLine("2026-06-01T21:39:10Z"), assistantText("2026-06-01T21:39:20Z")},
			wantKind: SignalActivity,
			wantTS:   "2026-06-01T21:39:20Z",
		},
		{
			name:     "should ignore timestamp-less metadata and ancillary system entries",
			lines:    []string{assistantText("2026-06-01T21:39:05Z"), noise, systemLine("2026-06-01T21:39:30Z")},
			wantKind: SignalActivity,
			wantTS:   "2026-06-01T21:39:05Z",
		},
		{
			name:     "should report none when no conversational entry exists",
			lines:    []string{noise, systemLine("2026-06-01T21:39:30Z")},
			wantKind: SignalNone,
			wantTS:   "",
		},
		{
			name:     "should tolerate a user entry whose content is a bare string",
			lines:    []string{userStringContent("2026-06-01T21:39:08Z", "a caveat")},
			wantKind: SignalActivity,
			wantTS:   "2026-06-01T21:39:08Z",
		},
		{
			name:     "should tolerate blank and malformed lines",
			lines:    []string{"", "not json", `{"type":"x"}`, interruptLine("2026-06-01T21:39:10Z")},
			wantKind: SignalInterrupt,
			wantTS:   "2026-06-01T21:39:10Z",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind, ts, err := NewestSignal(writeTranscript(t, tt.lines...), DefaultTailBytes)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if kind != tt.wantKind {
				t.Errorf("kind = %v, want %v", kind, tt.wantKind)
			}
			var want time.Time
			if tt.wantTS != "" {
				want = mustTime(t, tt.wantTS)
			}
			if !ts.Equal(want) {
				t.Errorf("ts = %v, want %v", ts, want)
			}
		})
	}
}

func TestNewestSignalFailsSoft(t *testing.T) {
	t.Run("should return none and an error for an empty path", func(t *testing.T) {
		kind, _, err := NewestSignal("", DefaultTailBytes)
		if kind != SignalNone || err == nil {
			t.Errorf("got (%v, %v), want (none, error)", kind, err)
		}
	})
	t.Run("should return none and an error for a missing file", func(t *testing.T) {
		kind, _, err := NewestSignal(filepath.Join(t.TempDir(), "nope.jsonl"), DefaultTailBytes)
		if kind != SignalNone || err == nil {
			t.Errorf("got (%v, %v), want (none, error)", kind, err)
		}
	})
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
	lines = append(lines, interruptLine("2026-06-01T21:45:00Z"))
	path := writeTranscript(t, lines...)

	got, err := ResolutionState(path, since, 256) // window smaller than the file
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != StateResolved {
		t.Errorf("ResolutionState (small window) = %v, want resolved", got)
	}
}
