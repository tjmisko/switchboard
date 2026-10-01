package codex

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
)

func TestRolloutRuntimeRequiresExplicitLatestLifecycle(t *testing.T) {
	base := time.Date(2026, 9, 30, 22, 0, 0, 0, time.UTC)
	marker := func(kind string, seconds int) string {
		return fmt.Sprintf("{\"timestamp\":%q,\"type\":\"event_msg\",\"payload\":{\"type\":%q}}\n", base.Add(time.Duration(seconds)*time.Second).Format(time.RFC3339Nano), kind)
	}
	for _, tc := range []struct {
		name, text string
		want       agentgraph.RuntimeState
	}{
		{"complete", marker("task_started", 0) + marker("task_complete", 1), agentgraph.RuntimeIdle},
		{"resumed", marker("task_complete", 1) + marker("task_started", 2), agentgraph.RuntimeActive},
		{"reasoning", marker("agent_reasoning", 1), agentgraph.RuntimeActive},
		{"no evidence", marker("token_count", 1), agentgraph.RuntimeUnknown},
		{"partial start", marker("task_complete", 1) + `{"type":"event_msg"`, agentgraph.RuntimeUnknown},
		{"oversized screenshot", strings.Repeat("x", 2*1024*1024) + "\n" + marker("task_complete", 1), agentgraph.RuntimeIdle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rollout.jsonl")
			if err := os.WriteFile(path, []byte(tc.text), 0600); err != nil {
				t.Fatal(err)
			}
			got, _, err := ReadRolloutRuntime(path)
			if err != nil || got != tc.want {
				t.Fatalf("runtime=%s err=%v want=%s", got, err, tc.want)
			}
		})
	}
}
