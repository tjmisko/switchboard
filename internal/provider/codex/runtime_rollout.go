package codex

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/usagelimit"
)

// RolloutState is what a rollout tail says about its newest turn boundary.
// UsageLimit is set when that boundary is a turn the usage limit ended.
type RolloutState struct {
	Runtime    agentgraph.RuntimeState
	At         time.Time
	UsageLimit *RolloutUsageLimit
}

// RolloutUsageLimit is a usage_limit_exceeded turn end. ResetsAt is nil when
// neither the rate-limit telemetry nor the message named a reset.
type RolloutUsageLimit struct {
	ResetsAt *time.Time
}

// ReadRolloutRuntime reads explicit lifecycle markers from an already-bound
// rollout. It never finds a rollout by CWD/recency and never infers idle from
// elapsed time. Oversized content rows do not prevent reading later markers.
func ReadRolloutRuntime(path string) (agentgraph.RuntimeState, time.Time, error) {
	state, err := ReadRolloutState(path)
	return state.Runtime, state.At, err
}

// ReadRolloutState is ReadRolloutRuntime plus the usage-limit verdict of the
// newest marker. Codex ends a capped turn with
//
//	task_complete {error: {codex_error_info: "usage_limit_exceeded",
//	               message: "… try again at 5:15 PM."}}
//
// and fires no Stop hook, so this read is how a capped turn is seen at all.
func ReadRolloutState(path string) (RolloutState, error) {
	const tailBytes = 256 * 1024
	unknown := RolloutState{Runtime: agentgraph.RuntimeUnknown}
	f, err := os.Open(path)
	if err != nil {
		return unknown, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return unknown, err
	}
	start := fi.Size() - tailBytes
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return unknown, err
	}
	data, err := io.ReadAll(io.LimitReader(f, tailBytes))
	if err != nil {
		return unknown, err
	}
	if start > 0 {
		if n := bytes.IndexByte(data, '\n'); n >= 0 {
			data = data[n+1:]
		} else {
			data = nil
		}
	}
	// A partially-written new row may be a start marker; defer until it flushes.
	if len(data) != 0 && data[len(data)-1] != '\n' {
		return unknown, nil
	}
	state := unknown
	var limits rolloutRateLimits
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		var event rolloutEvent
		if json.Unmarshal(line, &event) != nil || event.Type != "event_msg" {
			continue
		}
		if event.Payload.Type == "token_count" {
			limits.observe(event.Payload.RateLimits)
			continue
		}
		if !event.Timestamp.After(state.At) {
			continue
		}
		var next agentgraph.RuntimeState
		switch event.Payload.Type {
		case "task_complete", "turn_complete", "turn_completed", "turn_aborted", "shutdown_complete":
			next = agentgraph.RuntimeIdle
		case "task_started", "turn_started", "agent_reasoning", "agent_message", "exec_command_begin", "mcp_tool_call_begin":
			next = agentgraph.RuntimeActive
		default:
			continue
		}
		state = RolloutState{Runtime: next, At: event.Timestamp}
		if next == agentgraph.RuntimeIdle && event.Payload.Error.usageLimit() {
			state.UsageLimit = &RolloutUsageLimit{ResetsAt: limits.resetFor(event.Payload.Error.Message, event.Timestamp)}
		}
	}
	return state, nil
}

type rolloutEvent struct {
	Timestamp time.Time `json:"timestamp"`
	Type      string    `json:"type"`
	Payload   struct {
		Type       string            `json:"type"`
		Error      rolloutTurnError  `json:"error"`
		RateLimits *rolloutRateLimit `json:"rate_limits"`
	} `json:"payload"`
}

type rolloutTurnError struct {
	Message string `json:"message"`
	// CodexErrorInfo is a bare string for unit variants and an object for
	// variants with data (http_connection_failed, …), so it stays raw.
	CodexErrorInfo json.RawMessage `json:"codex_error_info"`
}

func (e rolloutTurnError) usageLimit() bool {
	var info string
	return json.Unmarshal(e.CodexErrorInfo, &info) == nil && info == "usage_limit_exceeded"
}

type rolloutRateLimit struct {
	Primary   *rolloutRateWindow `json:"primary"`
	Secondary *rolloutRateWindow `json:"secondary"`
}

type rolloutRateWindow struct {
	UsedPercent float64 `json:"used_percent"`
	ResetsAt    int64   `json:"resets_at"`
}

// rolloutRateLimits keeps the window nearest exhaustion from the newest
// token_count that reported any window. Codex also logs snapshots for other
// limit ids with null windows (limit_id "premium"); those say nothing.
type rolloutRateLimits struct {
	nearest *rolloutRateWindow
}

func (l *rolloutRateLimits) observe(snapshot *rolloutRateLimit) {
	if snapshot == nil || (snapshot.Primary == nil && snapshot.Secondary == nil) {
		return
	}
	l.nearest = nil
	for _, window := range []*rolloutRateWindow{snapshot.Primary, snapshot.Secondary} {
		if window != nil && window.ResetsAt > 0 && (l.nearest == nil || window.UsedPercent > l.nearest.UsedPercent) {
			l.nearest = window
		}
	}
}

// resetFor reconciles the two reset sources. The message's "try again at" is
// what Codex told the user for this failure; the telemetry epoch is exact to
// the second. When both exist and agree within two minutes the epoch wins;
// when they disagree the telemetry is from a different window, so the message
// wins.
func (l *rolloutRateLimits) resetFor(message string, at time.Time) *time.Time {
	stated := usagelimit.CodexResetFromMessage(message, at)
	if l.nearest == nil {
		return stated
	}
	epoch := time.Unix(l.nearest.ResetsAt, 0)
	if !epoch.After(at) {
		return stated
	}
	if stated != nil && (stated.Sub(epoch) > 2*time.Minute || epoch.Sub(*stated) > 2*time.Minute) {
		return stated
	}
	return &epoch
}
