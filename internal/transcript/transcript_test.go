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

// bashStdout is the synthetic user entry Claude Code writes for a `!` bash
// command's output. It runs with no agent turn (fires neither UserPromptSubmit
// nor Stop), so it must not read as conversational activity.
func bashStdout(ts, s string) string {
	return userStringContent(ts, "<bash-stdout>"+s+"</bash-stdout>")
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
			// The regression: a `!` bash command in an idle session flushes a
			// <bash-stdout> entry dated after the Stop, but runs no agent turn — it
			// must NOT read as activity, else the idle→working self-heal promotes a
			// correctly-orange chip back to green where it latches forever.
			name:     "should not treat a bash-command entry as activity",
			lines:    []string{assistantText("2026-06-01T21:39:05Z"), bashStdout("2026-06-01T21:39:40Z", "On branch main")},
			wantKind: SignalActivity,
			wantTS:   "2026-06-01T21:39:05Z",
		},
		{
			// With only local-command side-channel entries (a `!` bash command and a
			// `/` slash command) in the tail, there is no agent signal at all.
			name: "should report none when the tail holds only local-command entries",
			lines: []string{
				userStringContent("2026-06-01T21:39:30Z", "<bash-input>git status</bash-input>"),
				bashStdout("2026-06-01T21:39:31Z", "nothing to commit"),
				userStringContent("2026-06-01T21:39:35Z", "<command-name>clear</command-name>"),
				userStringContent("2026-06-01T21:39:36Z", "<local-command-stdout></local-command-stdout>"),
			},
			wantKind: SignalNone,
			wantTS:   "",
		},
		{
			// A purely-local slash command (/rename) starts no agent and fires no
			// UserPromptSubmit, so its <command-name>/<local-command-stdout> entries
			// must not promote an idle chip — same latch risk as a `!` command. An
			// agent-starting slash command is unaffected: it fires UserPromptSubmit, so
			// the chip is already working by reconcile and this branch never runs.
			name: "should not treat a local slash command as activity",
			lines: []string{
				assistantText("2026-06-01T21:39:05Z"),
				userStringContent("2026-06-01T21:39:50Z", "<command-name>rename</command-name>"),
				userStringContent("2026-06-01T21:39:51Z", "<local-command-stdout>renamed</local-command-stdout>"),
			},
			wantKind: SignalActivity,
			wantTS:   "2026-06-01T21:39:05Z",
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

// AnchorTime is the source-side fix for the hook-vs-transcript clock skew: it
// hands the hook handler the timestamp of the newest turn entry so a transition
// is dated from the event that triggered it, not from the later moment the hook
// is processed. See docs/timing-hazards.md.
func TestAnchorTime(t *testing.T) {
	t.Run("should return the newest turn entry timestamp", func(t *testing.T) {
		path := writeTranscript(t,
			assistantText("2026-06-01T21:38:00Z"),
			interruptLine("2026-06-01T21:39:30Z"),
		)
		ts, ok := AnchorTime(path, DefaultTailBytes)
		if !ok {
			t.Fatal("ok = false, want true")
		}
		if want := mustTime(t, "2026-06-01T21:39:30Z"); !ts.Equal(want) {
			t.Errorf("ts = %v, want newest entry %v", ts, want)
		}
	})

	t.Run("should ignore timestamp-less metadata when picking the newest", func(t *testing.T) {
		path := writeTranscript(t, assistantText("2026-06-01T21:38:00Z"), noise)
		ts, ok := AnchorTime(path, DefaultTailBytes)
		if !ok {
			t.Fatal("ok = false, want true")
		}
		if want := mustTime(t, "2026-06-01T21:38:00Z"); !ts.Equal(want) {
			t.Errorf("ts = %v, want %v (metadata carries no timestamp)", ts, want)
		}
	})

	t.Run("should report not-ok for an empty tail so the caller falls back to now", func(t *testing.T) {
		path := writeTranscript(t, noise)
		if _, ok := AnchorTime(path, DefaultTailBytes); ok {
			t.Error("ok = true, want false (no timestamped turn entry)")
		}
	})

	t.Run("should report not-ok for an unreadable transcript", func(t *testing.T) {
		if _, ok := AnchorTime(filepath.Join(t.TempDir(), "nope.jsonl"), DefaultTailBytes); ok {
			t.Error("ok = true, want false (unreadable)")
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

// taskUse is an assistant tool_use that spawns a subagent (name Task), with a
// distinct id so InFlightTasks can pair it against its completion.
func taskUse(ts, id string) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"role":"assistant","content":[{"type":"tool_use","id":%q,"name":"Task"}]}}`, ts, id)
}

// taskResult is the user tool_result that lands in the MAIN transcript when a
// subagent finishes, back-linked to its launching tool_use by tool_use_id.
func taskResult(ts, id string) string {
	return fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":%q}]}}`, ts, id)
}

func TestResolveKind(t *testing.T) {
	since := mustTime(t, "2026-06-01T21:39:00Z")

	tests := []struct {
		name  string
		lines []string
		want  ResolutionKind
	}{
		{
			name:  "should be resumed when an assistant message advances past the prompt (approved → green)",
			lines: []string{assistantText("2026-06-01T21:38:00Z"), assistantUse("2026-06-01T21:42:30Z")},
			want:  ResolutionResumed,
		},
		{
			name:  "should be interrupted when the newest resolution is an interrupt notice (Esc → orange)",
			lines: []string{assistantText("2026-06-01T21:38:00Z"), interruptLine("2026-06-01T21:42:30Z")},
			want:  ResolutionInterrupted,
		},
		{
			// A decline the model continued past: the rejection is followed by an
			// assistant message, which is newest, so it reads as resumed (green) —
			// consistent with "green = work happening".
			name:  "should be resumed when an assistant message follows an interrupt (decline the model continued past)",
			lines: []string{interruptLine("2026-06-01T21:42:00Z"), assistantText("2026-06-01T21:42:30Z")},
			want:  ResolutionResumed,
		},
		{
			name:  "should be none when only tool_results land after the prompt (concurrent subagent/parallel work)",
			lines: []string{assistantText("2026-06-01T21:38:00Z"), resultLine("2026-06-01T21:42:30Z")},
			want:  ResolutionNone,
		},
		{
			name:  "should be none when every resolution entry predates the prompt (unflushed pending)",
			lines: []string{assistantText("2026-06-01T21:36:45Z"), interruptLine("2026-06-01T21:38:59Z")},
			want:  ResolutionNone,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveKind(writeTranscript(t, tt.lines...), since, DefaultTailBytes)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("ResolveKind = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResolveKindFailsSoft(t *testing.T) {
	since := mustTime(t, "2026-06-01T21:39:00Z")
	got, err := ResolveKind("", since, DefaultTailBytes)
	if got != ResolutionNone || err == nil {
		t.Errorf("got (%v, %v), want (none, error)", got, err)
	}
}

func TestInFlightTasks(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		want  int
	}{
		{
			name:  "should be zero with no Task tool_use in the tail",
			lines: []string{assistantText("2026-06-01T21:39:00Z"), resultLine("2026-06-01T21:39:05Z")},
			want:  0,
		},
		{
			name:  "should count one in-flight Task whose result has not landed",
			lines: []string{taskUse("2026-06-01T21:39:00Z", "toolu_a")},
			want:  1,
		},
		{
			name:  "should count N in-flight Tasks",
			lines: []string{taskUse("2026-06-01T21:39:00Z", "toolu_a"), taskUse("2026-06-01T21:39:01Z", "toolu_b"), taskUse("2026-06-01T21:39:02Z", "toolu_c")},
			want:  3,
		},
		{
			name:  "should be zero once every launched Task has a matching result (drained)",
			lines: []string{taskUse("2026-06-01T21:39:00Z", "toolu_a"), taskUse("2026-06-01T21:39:01Z", "toolu_b"), taskResult("2026-06-01T21:39:30Z", "toolu_a"), taskResult("2026-06-01T21:39:31Z", "toolu_b")},
			want:  0,
		},
		{
			name:  "should count only the still-open Task when some have drained",
			lines: []string{taskUse("2026-06-01T21:39:00Z", "toolu_a"), taskUse("2026-06-01T21:39:01Z", "toolu_b"), taskResult("2026-06-01T21:39:30Z", "toolu_a")},
			want:  1,
		},
		{
			// A non-Task tool_use (e.g. AskUserQuestion) is not a subagent and must
			// not be counted as delegated work in flight.
			name:  "should ignore non-Task tool_use blocks",
			lines: []string{assistantUse("2026-06-01T21:39:00Z")},
			want:  0,
		},
		{
			// Tail clamp: a result whose launching tool_use scrolled out of the
			// window pairs against nothing; the count must not go negative.
			name:  "should clamp at zero when a result has no in-window tool_use",
			lines: []string{taskResult("2026-06-01T21:39:30Z", "toolu_gone")},
			want:  0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := InFlightTasks(writeTranscript(t, tt.lines...), DefaultTailBytes)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("InFlightTasks = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestInFlightTasksFailsSoft(t *testing.T) {
	got, err := InFlightTasks("", DefaultTailBytes)
	if got != 0 || err == nil {
		t.Errorf("got (%d, %v), want (0, error)", got, err)
	}
}
