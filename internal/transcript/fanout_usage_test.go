package transcript

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// taskUseRich is a Task tool_use carrying the subagent_type/description metadata
// Tasks surfaces for the rich spawn events.
func taskUseRich(ts, id, agentType, desc string) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"role":"assistant","content":[{"type":"tool_use","id":%q,"name":"Task","input":{"subagent_type":%q,"description":%q}}]}}`,
		ts, id, agentType, desc)
}

func TestTasksReportsMetadataAndDone(t *testing.T) {
	path := writeTranscript(t,
		taskUseRich("2026-06-01T21:39:00Z", "toolu_a", "Explore", "map the auth code"),
		taskUseRich("2026-06-01T21:39:01Z", "toolu_b", "general-purpose", "write the migration"),
		taskResult("2026-06-01T21:39:30Z", "toolu_a"),
	)
	tasks, err := Tasks(path, DefaultTailBytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 {
		t.Fatalf("got %d tasks, want 2", len(tasks))
	}
	// Launch order preserved.
	if tasks[0].ID != "toolu_a" || tasks[0].AgentType != "Explore" || tasks[0].Description != "map the auth code" {
		t.Errorf("task 0 = %+v", tasks[0])
	}
	if !tasks[0].Done {
		t.Errorf("task toolu_a has a result → Done, got %+v", tasks[0])
	}
	if tasks[1].Done {
		t.Errorf("task toolu_b has no result → not Done, got %+v", tasks[1])
	}
}

func assistantUsage(ts string, in, out, cacheRead, cacheCreate int64) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":%d,"output_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d}}}`,
		ts, in, out, cacheRead, cacheCreate)
}

func TestUsageSinceSumsNewAssistantMessages(t *testing.T) {
	path := writeTranscript(t,
		assistantUsage("2026-06-01T21:39:00Z", 100, 50, 9000, 200),
		assistantUsage("2026-06-01T21:39:10Z", 30, 20, 1000, 0),
	)
	u, off, err := UsageSince(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if u.InputTokens != 130 || u.OutputTokens != 70 || u.CacheReadTokens != 10000 || u.CacheCreationTokens != 200 {
		t.Errorf("summed usage = %+v", u)
	}
	if off == 0 {
		t.Errorf("offset should advance past the consumed lines")
	}

	// A second call from the advanced offset sees nothing new.
	u2, off2, err := UsageSince(path, off)
	if err != nil {
		t.Fatal(err)
	}
	if !u2.IsZero() || off2 != off {
		t.Errorf("re-read from offset should be empty; got %+v off %d (was %d)", u2, off2, off)
	}
}

func TestUsageSinceIncremental(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	first := assistantUsage("2026-06-01T21:39:00Z", 100, 50, 0, 0) + "\n"
	if err := os.WriteFile(path, []byte(first), 0o644); err != nil {
		t.Fatal(err)
	}
	_, off, err := UsageSince(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Append a second message and read only the delta from the saved offset.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(assistantUsage("2026-06-01T21:40:00Z", 5, 7, 0, 0) + "\n")
	f.Close()

	u, _, err := UsageSince(path, off)
	if err != nil {
		t.Fatal(err)
	}
	if u.InputTokens != 5 || u.OutputTokens != 7 {
		t.Errorf("incremental delta = %+v, want only the appended message", u)
	}
}

func TestUsageSinceExcludesPartialFinalLineUntilComplete(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")

	line1 := assistantUsage("2026-06-01T21:39:00Z", 100, 50, 0, 0)
	line2 := assistantUsage("2026-06-01T21:40:00Z", 7, 3, 0, 0)
	// line1 is complete (newline-terminated); line2 is mid-write — its bytes are on
	// disk but its trailing newline has not landed yet.
	if err := os.WriteFile(path, []byte(line1+"\n"+line2), 0o644); err != nil {
		t.Fatal(err)
	}

	// The partial final line is excluded and the offset stops just before it.
	u1, off1, err := UsageSince(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if u1.InputTokens != 100 || u1.OutputTokens != 50 {
		t.Errorf("first read counted the partial line: %+v, want only line1 (100/50)", u1)
	}
	if want := int64(len(line1) + 1); off1 != want {
		t.Errorf("offset = %d, want %d (just past line1's newline, before the partial line)", off1, want)
	}

	// The writer finishes line2 (its newline lands).
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("\n")
	f.Close()

	// The previously-partial line is now read exactly once — line1 is behind the
	// offset and cannot be re-summed, so there is no double count.
	u2, off2, err := UsageSince(path, off1)
	if err != nil {
		t.Fatal(err)
	}
	if u2.InputTokens != 7 || u2.OutputTokens != 3 {
		t.Errorf("second read = %+v, want only line2 (7/3) with no double count of line1", u2)
	}

	// A third read sees nothing new, proving line2 was consumed exactly once.
	u3, off3, err := UsageSince(path, off2)
	if err != nil {
		t.Fatal(err)
	}
	if !u3.IsZero() || off3 != off2 {
		t.Errorf("third read should be empty; got %+v off %d (was %d)", u3, off3, off2)
	}
}

func TestUsageSinceResetsOnTruncation(t *testing.T) {
	path := writeTranscript(t, assistantUsage("2026-06-01T21:39:00Z", 100, 50, 0, 0))
	// Pretend a prior read advanced well past the current (smaller) file size.
	u, off, err := UsageSince(path, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	// Truncation detected (size < offset) → restart from 0, re-summing the file.
	if u.InputTokens != 100 || off == 1<<30 {
		t.Errorf("truncation should reset and re-read; got %+v off %d", u, off)
	}
}
