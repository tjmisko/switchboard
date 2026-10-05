package codex

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/usagelimit"
)

// testdata/rollout-usage-limit.jsonl is the capped turn of a real session
// (2026-10-05 12:28 PDT) reduced to its markers and rate-limit telemetry:
// 99% of the 5h window used, then the "premium" snapshot with null windows,
// then task_complete with usage_limit_exceeded and "try again at 5:15 PM".
func TestReadRolloutStateShouldReportAUsageLimitWhenTheRealCappedTurnEnds(t *testing.T) {
	state, err := ReadRolloutState(filepath.Join("testdata", "rollout-usage-limit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if state.Runtime != agentgraph.RuntimeIdle || !state.At.Equal(time.Date(2026, 10, 5, 19, 28, 5, 435_000_000, time.UTC)) {
		t.Fatalf("runtime %s at %v, want idle at the task_complete", state.Runtime, state.At)
	}
	if state.UsageLimit == nil || state.UsageLimit.ResetsAt == nil {
		t.Fatalf("usage limit = %+v, want one with a reset", state.UsageLimit)
	}
	// 1791245739 is 17:15:39 PDT, the window the message calls "5:15 PM".
	if want := time.Unix(1791245739, 0); !state.UsageLimit.ResetsAt.Equal(want) {
		t.Fatalf("resets_at = %v, want the telemetry epoch %v", state.UsageLimit.ResetsAt, want)
	}
}

func TestReadRolloutStateShouldDropTheLimitWhenALaterTurnStarts(t *testing.T) {
	path := rolloutFixtureWith(t, rolloutMarker("task_started", "2026-10-05T19:40:00Z"))
	state, err := ReadRolloutState(path)
	if err != nil {
		t.Fatal(err)
	}
	if state.Runtime != agentgraph.RuntimeActive || state.UsageLimit != nil {
		t.Fatalf("runtime %s limit %+v, want active with no limit", state.Runtime, state.UsageLimit)
	}
}

func TestReadRolloutStateShouldNotReportALimitWhenTheTurnEndsOnAnotherError(t *testing.T) {
	for name, line := range map[string]string{
		"clean completion":       rolloutMarker("task_complete", "2026-10-05T19:30:00Z"),
		"interrupt":              rolloutMarker("turn_aborted", "2026-10-05T19:30:00Z"),
		"rate limit (transient)": `{"timestamp":"2026-10-05T19:30:00Z","type":"event_msg","payload":{"type":"task_complete","error":{"message":"Rate limit reached","codex_error_info":"rate_limit_exceeded"}}}`,
		"object-valued info":     `{"timestamp":"2026-10-05T19:30:00Z","type":"event_msg","payload":{"type":"task_complete","error":{"message":"stream failed","codex_error_info":{"http_connection_failed":{"http_status_code":502}}}}}`,
		"no error info":          `{"timestamp":"2026-10-05T19:30:00Z","type":"event_msg","payload":{"type":"task_complete","error":{"message":"You've hit your usage limit."}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			state, err := ReadRolloutState(writeRollout(t, line+"\n"))
			if err != nil {
				t.Fatal(err)
			}
			if state.Runtime != agentgraph.RuntimeIdle || state.UsageLimit != nil {
				t.Fatalf("runtime %s limit %+v, want idle with no limit", state.Runtime, state.UsageLimit)
			}
		})
	}
}

func TestReadRolloutStateShouldTrustTheMessageWhenTelemetryNamesAnotherWindow(t *testing.T) {
	at := time.Date(2026, 10, 5, 19, 28, 5, 0, time.UTC)
	message := "You’ve hit your usage limit. Upgrade to Pro, or try again at 5:15 PM."
	stale := `{"timestamp":"2026-10-05T19:28:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":100,"window_minutes":300,"resets_at":` +
		itoa(at.Add(30*time.Hour).Unix()) + `}}}}`
	end := `{"timestamp":"2026-10-05T19:28:05Z","type":"event_msg","payload":{"type":"task_complete","error":{"message":"` + message + `","codex_error_info":"usage_limit_exceeded"}}}`
	state, err := ReadRolloutState(writeRollout(t, stale+"\n"+end+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := usagelimit.CodexResetFromMessage(message, at)
	if state.UsageLimit == nil || state.UsageLimit.ResetsAt == nil || !state.UsageLimit.ResetsAt.Equal(*want) {
		t.Fatalf("resets_at = %+v, want the stated %v", state.UsageLimit, want)
	}
}

func TestReadRolloutStateShouldUseTheTelemetryWhenTheMessageNamesNoTime(t *testing.T) {
	resetsAt := time.Date(2026, 10, 6, 0, 15, 39, 0, time.UTC)
	telemetry := `{"timestamp":"2026-10-05T19:28:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":40,"resets_at":1},"secondary":{"used_percent":100,"resets_at":` +
		itoa(resetsAt.Unix()) + `}}}}`
	end := `{"timestamp":"2026-10-05T19:28:05Z","type":"event_msg","payload":{"type":"task_complete","error":{"message":"You've hit your usage limit.","codex_error_info":"usage_limit_exceeded"}}}`
	state, err := ReadRolloutState(writeRollout(t, telemetry+"\n"+end+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if state.UsageLimit == nil || state.UsageLimit.ResetsAt == nil || !state.UsageLimit.ResetsAt.Equal(resetsAt) {
		t.Fatalf("resets_at = %+v, want the exhausted weekly window's %v", state.UsageLimit, resetsAt)
	}
}

func TestReadRolloutStateShouldReportALimitWithNoResetWhenNothingNamesOne(t *testing.T) {
	end := `{"timestamp":"2026-10-05T19:28:05Z","type":"event_msg","payload":{"type":"task_complete","error":{"message":"You've hit your usage limit.","codex_error_info":"usage_limit_exceeded"}}}`
	state, err := ReadRolloutState(writeRollout(t, end+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if state.UsageLimit == nil || state.UsageLimit.ResetsAt != nil {
		t.Fatalf("limit = %+v, want a limit with no reset", state.UsageLimit)
	}
}

func rolloutFixtureWith(t *testing.T, extra string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "rollout-usage-limit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return writeRollout(t, string(body)+extra+"\n")
}

func writeRollout(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func rolloutMarker(kind, timestamp string) string {
	return `{"timestamp":"` + timestamp + `","type":"event_msg","payload":{"type":"` + kind + `"}}`
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
