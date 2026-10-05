package main

import (
	"strconv"
	"testing"
	"time"
)

// A hook fired inside a Codex subagent carries the root's session_id and the
// child's own transcript_path (captured 2026-10-05), so the path is filed under
// the writer that fired it.
func TestCodexRememberTranscriptShouldEvictTheOldestChildWhenPastTheLimit(t *testing.T) {
	root := newCodexHookRootState("root", time.Time{})
	root.rememberTranscript("", "/root.jsonl")
	for i := 0; i <= codexChildTranscriptLimit; i++ {
		root.rememberTranscript("child-"+strconv.Itoa(i), "/child-"+strconv.Itoa(i)+".jsonl")
	}
	if root.transcript != "/root.jsonl" {
		t.Fatalf("root transcript = %q", root.transcript)
	}
	if len(root.childTranscripts) != codexChildTranscriptLimit {
		t.Fatalf("children = %d, want %d", len(root.childTranscripts), codexChildTranscriptLimit)
	}
	if _, kept := root.childTranscripts["child-0"]; kept {
		t.Fatal("oldest child survived eviction")
	}
	last := "child-" + strconv.Itoa(codexChildTranscriptLimit)
	if root.childTranscripts[last] == "" {
		t.Fatalf("newest child %s was evicted", last)
	}
}

func TestCodexRememberTranscriptShouldForgetChildrenWhenTheSessionRotates(t *testing.T) {
	root := newCodexHookRootState("old", time.Time{})
	root.rememberTranscript("child", "/child.jsonl")
	commitCodexHookSession(root, "new", time.Now())
	if len(root.childTranscripts) != 0 || len(root.childTranscriptOrder) != 0 {
		t.Fatalf("children after rotation = %v / %v", root.childTranscripts, root.childTranscriptOrder)
	}
}
