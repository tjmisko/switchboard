package codex

import (
	"bytes"
	"encoding/json"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/tailcache"
	"github.com/tjmisko/switchboard/internal/tailread"
	"github.com/tjmisko/switchboard/internal/turnfailure"
	"github.com/tjmisko/switchboard/internal/usagelimit"
)

// RolloutState is what a rollout tail says about its newest turn boundary.
// UsageLimit is set when that boundary is a turn the usage limit ended.
type RolloutState struct {
	Runtime    agentgraph.RuntimeState
	At         time.Time
	UsageLimit *RolloutUsageLimit
	// TurnFailure is the turnfailure.Kind when that boundary is a turn a network
	// or auth error ended; empty otherwise. Never set together with UsageLimit.
	// A string, so the cached value copies without aliasing.
	TurnFailure string
}

// RolloutUsageLimit is a usage_limit_exceeded turn end. ResetsAt is nil when
// neither the rate-limit telemetry nor the message named a reset.
type RolloutUsageLimit struct {
	ResetsAt *time.Time
	// ResetFromEpoch reports that ResetsAt came from the rate-limit telemetry
	// epoch rather than the error message.
	ResetFromEpoch bool
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
//
// The verdict is cached by the rollout's file identity (#98): an unchanged
// rollout is stat'ed, not re-read.
func ReadRolloutState(path string) (RolloutState, error) {
	if path == "" {
		return readRolloutState(path)
	}
	state, err := tailcache.Load(tailcache.Default(), path, "codex/rollout-state", readRolloutState)
	return state.clone(), err
}

// clone detaches the usage-limit verdict from the cached one.
func (s RolloutState) clone() RolloutState {
	if s.UsageLimit == nil {
		return s
	}
	limit := *s.UsageLimit
	if limit.ResetsAt != nil {
		resetsAt := *limit.ResetsAt
		limit.ResetsAt = &resetsAt
	}
	s.UsageLimit = &limit
	return s
}

// observeTurnUsageLimit records the newest usage-limit turn error in turns.
// It reads the app-server's persisted turn error, the same fact the rollout
// writes, so a cap is seen even when the rollout is not bound or is still
// being written.
func (s *graphState) observeTurnUsageLimit(turns []rpcTurn) {
	for _, turn := range turns {
		if !turn.Error.usageLimit() {
			continue
		}
		var at time.Time
		switch {
		case turn.CompletedAt != 0:
			at = time.Unix(turn.CompletedAt, 0)
		case turn.StartedAt != 0:
			at = time.Unix(turn.StartedAt, 0)
		default:
			continue
		}
		if !at.After(s.usageLimitAt) {
			continue
		}
		s.usageLimitAt = at
		s.usageLimitResetsAt = usagelimit.CodexResetFromMessage(turn.Error.Message, at)
	}
}

// observeTurnFailure records the newest network or auth turn error in turns,
// the same fact the rollout's terminal row carries. A failure is current only
// while it is the newest turn: a later turn, successful or not yet finished,
// means the user moved on, so the failure is dropped.
func (s *graphState) observeTurnFailure(turns []rpcTurn) {
	var newest, failedAt time.Time
	var failedKind string
	for _, turn := range turns {
		at := turn.endedOrStartedAt()
		if at.IsZero() {
			continue
		}
		if at.After(newest) {
			newest = at
		}
		kind, ok := turn.Error.turnFailure()
		if !ok || turn.CompletedAt == 0 || !at.After(failedAt) {
			continue
		}
		failedAt, failedKind = at, string(kind)
	}
	if failedKind == "" || failedAt.Before(newest) {
		return
	}
	if !failedAt.After(s.turnFailureAt) {
		return
	}
	s.turnFailureAt, s.turnFailureKind = failedAt, failedKind
}

func readRolloutState(path string) (RolloutState, error) {
	const tailBytes = 256 * 1024
	unknown := RolloutState{Runtime: agentgraph.RuntimeUnknown}
	data, complete, err := tailread.Lines(path, tailBytes)
	if err != nil {
		return unknown, err
	}
	// A partially-written new row may be a start marker; defer until it flushes.
	if !complete {
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
		if next != agentgraph.RuntimeIdle || len(event.Payload.Error) == 0 {
			continue
		}
		var turnError rolloutTurnError
		if json.Unmarshal(event.Payload.Error, &turnError) != nil {
			continue
		}
		if turnError.usageLimit() {
			resetsAt, fromEpoch := limits.resetFor(turnError.Message, event.Timestamp)
			state.UsageLimit = &RolloutUsageLimit{ResetsAt: resetsAt, ResetFromEpoch: fromEpoch}
			continue
		}
		if kind, ok := turnfailure.FromCodex(turnError.CodexErrorInfo, turnError.Message); ok {
			state.TurnFailure = string(kind)
		}
	}
	return state, nil
}

type rolloutEvent struct {
	Timestamp time.Time `json:"timestamp"`
	Type      string    `json:"type"`
	Payload   struct {
		Type string `json:"type"`
		// Error stays raw until the row is known to be a turn end, so an
		// unexpected shape can never cost a lifecycle marker its decode.
		Error      json.RawMessage   `json:"error"`
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
func (l *rolloutRateLimits) resetFor(message string, at time.Time) (*time.Time, bool) {
	stated := usagelimit.CodexResetFromMessage(message, at)
	if l.nearest == nil {
		return stated, false
	}
	epoch := time.Unix(l.nearest.ResetsAt, 0)
	if !epoch.After(at) {
		return stated, false
	}
	if stated != nil && (stated.Sub(epoch) > 2*time.Minute || epoch.Sub(*stated) > 2*time.Minute) {
		return stated, false
	}
	return &epoch, true
}
