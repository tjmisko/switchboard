package codex

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/tailcache"
)

func TestReadRolloutStateShouldReuseTheVerdictWhenUnchangedAndReReadWhenTruncated(t *testing.T) {
	t.Cleanup(tailcache.SetDefault(tailcache.New(tailcache.OS{}, 0)))
	base := time.Date(2026, 9, 30, 22, 0, 0, 0, time.UTC)
	marker := func(kind string, seconds int) string {
		return fmt.Sprintf("{\"timestamp\":%q,\"type\":\"event_msg\",\"payload\":{\"type\":%q}}\n", base.Add(time.Duration(seconds)*time.Second).Format(time.RFC3339Nano), kind)
	}
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(path, []byte(marker("task_started", 0)+marker("task_complete", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if got, err := ReadRolloutState(path); err != nil || got.Runtime != agentgraph.RuntimeIdle {
			t.Fatalf("ReadRolloutState = %+v, %v; want idle", got, err)
		}
	}
	if stats := tailcache.Default().Stats(); stats.Reads != 1 || stats.Stats != 3 {
		t.Fatalf("cache work = %+v, want one read and a stat per poll", stats)
	}
	// A rollout rewritten shorter (truncation) is re-read.
	if err := os.WriteFile(path, []byte(marker("task_started", 2)), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadRolloutState(path); err != nil || got.Runtime != agentgraph.RuntimeActive {
		t.Fatalf("after truncation = %+v, %v; want the new active marker", got, err)
	}
}

func TestReadRolloutStateShouldDetachTheUsageLimitWhenServedFromTheCache(t *testing.T) {
	t.Cleanup(tailcache.SetDefault(tailcache.New(tailcache.OS{}, 0)))
	resetsAt := time.Date(2026, 9, 30, 23, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	line := fmt.Sprintf("{\"timestamp\":%q,\"type\":\"event_msg\",\"payload\":{\"type\":\"task_complete\",\"error\":{\"codex_error_info\":\"usage_limit_exceeded\",\"message\":\"try again later\"}}}\n", resetsAt.Add(-time.Hour).Format(time.RFC3339Nano))
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := ReadRolloutState(path)
	if err != nil || first.UsageLimit == nil {
		t.Fatalf("first read = %+v, %v; want the cap", first, err)
	}
	first.UsageLimit.ResetsAt = &resetsAt
	second, _ := ReadRolloutState(path)
	if second.UsageLimit == nil || second.UsageLimit == first.UsageLimit || second.UsageLimit.ResetsAt != nil {
		t.Fatalf("second read = %+v, want a detached copy unaffected by the caller's edit", second.UsageLimit)
	}
}
