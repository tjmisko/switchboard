package transcript

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tjmisko/switchboard/internal/tailcache"
)

// A finished child's transcript never changes again, so a failed read cached
// against its identity would report it live until something invalidated it.
// chmod moves neither size nor mtime, so the second poll sees the same identity
// the failed one did and only an uncached failure lets it re-read.
func TestSubagentsForTranscriptShouldRetryActivityReadWhenAnOpenFailedOnAnUnchangedFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions, so the open cannot be made to fail")
	}
	t.Cleanup(tailcache.SetDefault(tailcache.New(tailcache.OS{}, 0)))

	dir := t.TempDir()
	transcriptPath := filepath.Join(dir, "sess.jsonl")
	subagentsDir := filepath.Join(dir, "sess", "subagents")
	if err := os.MkdirAll(subagentsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	child := filepath.Join(subagentsDir, "agent-done.jsonl")
	writeFile(t, child, []string{subagentWorkingLine("2026-06-01T21:39:00Z"), subagentTerminalLine("2026-06-01T21:39:30Z")})
	before, err := tailcache.OS{}.Stat(child)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if err := os.Chmod(child, 0); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(child, 0o644) })
	subs, err := SubagentsForTranscript(transcriptPath)
	if err != nil {
		t.Fatalf("SubagentsForTranscript with unreadable child: %v", err)
	}
	if len(subs) != 1 || subs[0].Done {
		t.Fatalf("unreadable child = %+v, want one not-done subagent", subs)
	}

	if err := os.Chmod(child, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	after, err := tailcache.OS{}.Stat(child)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if after != before {
		t.Fatalf("identity moved across chmod (%+v -> %+v); the test no longer exercises an unchanged file", before, after)
	}
	subs, err = SubagentsForTranscript(transcriptPath)
	if err != nil {
		t.Fatalf("SubagentsForTranscript: %v", err)
	}
	if len(subs) != 1 || !subs[0].Done || subs[0].LatestEntryAt.IsZero() {
		t.Fatalf("child after the open recovered = %+v, want Done with its latest entry time", subs)
	}
}

// An incomplete tail is a write in progress; the write that completes it moves
// the size, so caching the not-yet-done answer cannot outlive the change.
func TestSubagentsForTranscriptShouldCacheIncompleteTailWhenTheFileIsUnchanged(t *testing.T) {
	t.Cleanup(tailcache.SetDefault(tailcache.New(tailcache.OS{}, 0)))

	dir := t.TempDir()
	transcriptPath := filepath.Join(dir, "sess.jsonl")
	subagentsDir := filepath.Join(dir, "sess", "subagents")
	if err := os.MkdirAll(subagentsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	child := filepath.Join(subagentsDir, "agent-half.jsonl")
	if err := os.WriteFile(child, []byte(subagentTerminalLine("2026-06-01T21:39:30Z")), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	for i := 0; i < 2; i++ {
		subs, err := SubagentsForTranscript(transcriptPath)
		if err != nil {
			t.Fatalf("SubagentsForTranscript: %v", err)
		}
		if len(subs) != 1 || subs[0].Done || subs[0].ModTime.IsZero() {
			t.Fatalf("poll %d: half-written child = %+v, want not done with its mtime", i, subs)
		}
	}
	if stats := tailcache.Default().Stats(); stats.Reads != 1 {
		t.Fatalf("stats = %+v, want the unchanged incomplete tail read once", stats)
	}
}
