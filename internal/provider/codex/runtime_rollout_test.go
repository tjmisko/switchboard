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

func TestObserveTurnUsageLimitShouldReadTheAppServerTurnError(t *testing.T) {
	var s graphState
	s.observeTurnUsageLimit([]rpcTurn{{
		CompletedAt: 1792620487,
		Error: &rpcTurnError{
			Message:        "You've hit your usage limit. Upgrade to Pro ... try again at 6:23 PM.",
			CodexErrorInfo: []byte(`"usageLimitExceeded"`),
		},
	}})
	if s.usageLimitAt.IsZero() || s.usageLimitResetsAt == nil {
		t.Fatalf("usage limit not recorded: at=%v resets=%v", s.usageLimitAt, s.usageLimitResetsAt)
	}
	if got := s.usageLimitAt.UTC(); got.Unix() != 1792620487 {
		t.Fatalf("usage limit at = %v, want the turn's completedAt", got)
	}
	// A later non-limit failure must not replace the cap.
	s.observeTurnUsageLimit([]rpcTurn{{
		CompletedAt: 1792620500,
		Error:       &rpcTurnError{Message: "something else", CodexErrorInfo: []byte(`"internalServerError"`)},
	}})
	if got := s.usageLimitAt.UTC(); got.Unix() != 1792620487 {
		t.Fatalf("a non-limit turn replaced the cap: %v", got)
	}
}

func TestReadRolloutStateShouldPreferTheRateLimitEpochForTheReset(t *testing.T) {
	at := time.Date(2026, 9, 30, 22, 0, 0, 0, time.UTC)
	epoch := at.Add(3 * time.Hour)
	rows := fmt.Sprintf(
		"{\"timestamp\":%q,\"type\":\"event_msg\",\"payload\":{\"type\":\"token_count\",\"rate_limits\":{\"limit_id\":\"codex\",\"primary\":{\"used_percent\":100,\"resets_at\":%d}}}}\n"+
			"{\"timestamp\":%q,\"type\":\"event_msg\",\"payload\":{\"type\":\"task_complete\",\"error\":{\"message\":\"You’ve hit your usage limit.\",\"codex_error_info\":\"usage_limit_exceeded\"}}}\n",
		at.Add(-time.Second).Format(time.RFC3339Nano), epoch.Unix(),
		at.Format(time.RFC3339Nano))
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(path, []byte(rows), 0600); err != nil {
		t.Fatal(err)
	}
	state, err := ReadRolloutState(path)
	if err != nil || state.UsageLimit == nil {
		t.Fatalf("usage limit = %+v err=%v, want the cap", state.UsageLimit, err)
	}
	if !state.UsageLimit.ResetFromEpoch {
		t.Fatalf("reset_from_epoch = false, want the telemetry epoch")
	}
	if state.UsageLimit.ResetsAt == nil || !state.UsageLimit.ResetsAt.Equal(epoch) {
		t.Fatalf("resets_at = %v, want %v", state.UsageLimit.ResetsAt, epoch)
	}
}

func TestReadRolloutStateShouldMarkAMessageResetAsNotFromEpoch(t *testing.T) {
	at := time.Date(2026, 9, 30, 22, 0, 0, 0, time.UTC)
	rows := fmt.Sprintf(
		"{\"timestamp\":%q,\"type\":\"event_msg\",\"payload\":{\"type\":\"task_complete\",\"error\":{\"message\":\"You’ve hit your usage limit. try again at 11:15 PM.\",\"codex_error_info\":\"usage_limit_exceeded\"}}}\n",
		at.Format(time.RFC3339Nano))
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(path, []byte(rows), 0600); err != nil {
		t.Fatal(err)
	}
	state, err := ReadRolloutState(path)
	if err != nil || state.UsageLimit == nil {
		t.Fatalf("usage limit = %+v err=%v, want the cap", state.UsageLimit, err)
	}
	if state.UsageLimit.ResetFromEpoch {
		t.Fatalf("reset_from_epoch = true, want a message reset")
	}
}

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
