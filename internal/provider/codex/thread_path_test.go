package codex

import (
	"encoding/json"
	"testing"
)

func TestGraphStateShouldKeepEachThreadsRolloutPathWhenTheServerNamesIt(t *testing.T) {
	var root, child, relative rpcThread
	for raw, into := range map[string]*rpcThread{
		`{"id":"root","path":"/home/u/.codex/sessions/2026/10/05/rollout-root.jsonl"}`:                             &root,
		`{"id":"child","parentThreadId":"root","path":"/home/u/.codex/sessions/2026/10/05/./rollout-child.jsonl"}`: &child,
		`{"id":"loose","parentThreadId":"root","path":"rollout-loose.jsonl"}`:                                      &relative,
	} {
		if err := json.Unmarshal([]byte(raw), into); err != nil {
			t.Fatal(err)
		}
	}

	state := newGraphState(root, []rpcThread{child, relative}, 8)

	if got := state.nodes["root"].rolloutPath; got != "/home/u/.codex/sessions/2026/10/05/rollout-root.jsonl" {
		t.Fatalf("root path = %q", got)
	}
	if got := state.nodes["child"].rolloutPath; got != "/home/u/.codex/sessions/2026/10/05/rollout-child.jsonl" {
		t.Fatalf("child path = %q, want it cleaned", got)
	}
	if got := state.nodes["loose"].rolloutPath; got != "" {
		t.Fatalf("relative path = %q, want it refused", got)
	}
}
