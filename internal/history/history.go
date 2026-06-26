// Package history is Switchboard's durable, opt-in activity log: an append-only
// stream of status transitions and session lifecycle events that a future
// dashboard (timeline view, attention stats, plan-usage graphs) reads back.
//
// It is the same information the daemon already computes for the live state and
// the journald decision log (statustune.Decision), written to a place
// Switchboard owns so retention is bounded and the data is query-shaped rather
// than grep-only. The store is deliberately simple — one JSON event per line,
// one file per UTC day — so it stays zero-dependency and portable (no cgo), and
// a torn final line costs one event, not the store.
//
//   - Event is the atom (a transition, a session_start/end, a suspend/resume,
//     and later usage_sample / subagent_spawn|stop). Everything a dashboard
//     shows — per-session colored intervals, idle/red-wait totals, hours of
//     agent attention — is an aggregation over these.
//   - Sink is the writer: best-effort and asynchronous, so Record never blocks
//     the daemon's hot path (it is called while the state lock is held).
//   - Config gates it OFF by default and bounds what is recorded (privacy tier)
//     and how long it is kept (retention).
package history

import (
	"os"
	"path/filepath"
	"time"
)

// Event types. A transition closes one colored interval and opens the next; the
// lifecycle events bound a session's first and last intervals; suspend/resume
// mark the greyed-out spans. (Usage and subagent events arrive in Phase 4.)
const (
	EventTransition   = "transition"
	EventSessionStart = "session_start"
	EventSessionEnd   = "session_end"
	EventSuspend      = "suspend"
	EventResume       = "resume"
	// Phase 4 — fanout & plan usage.
	EventSubagentSpawn = "subagent_spawn" // a Task/Agent subagent was launched
	EventSubagentStop  = "subagent_stop"  // its result landed (it finished)
	EventUsageSample   = "usage_sample"   // token usage accrued since the last sample
	// v2 — session names, model/cost, focus & attention.
	EventSessionLabel = "session_label" // the session's name/label changed (Label set)
	EventFocus        = "focus"          // window focus moved (SessionID = focused agent, empty = focus left all agents)
	EventActivity     = "activity"       // user went idle / active (global; To = idle|active)
)

// Detail tiers. Minimal records only what a timeline needs (ids, status, timing,
// the project abbreviation) and omits anything that reveals *what* you are doing
// (the raw cwd, the tool a prompt was for). Full adds those back.
const (
	DetailMinimal = "minimal"
	DetailFull    = "full"
)

// Event is one record in the activity log. Most fields are omitempty so a line
// stays small and a reader tolerates older/newer shapes; `ts` and `type` are
// always present. `pid` is always written and `session_id` whenever known, so a
// reader can group a session's events by session_id (stable across PID reuse)
// and fall back to pid for the pre-hook lifecycle events that have no id yet.
type Event struct {
	Ts        time.Time `json:"ts"`                   // the event instant (marshals RFC3339)
	Type      string    `json:"type"`                 // one of the Event* constants
	SessionID string    `json:"session_id,omitempty"` // agent session UUID, when known
	PID       int       `json:"pid,omitempty"`        // OS pid (always set in practice)
	Agent     string    `json:"agent,omitempty"`      // claude | codex
	Project   string    `json:"project,omitempty"`    // project abbreviation (resolved from cwd)

	// Transition payload.
	From      string `json:"from,omitempty"`        // status before the edge
	To        string `json:"to,omitempty"`          // status after the edge
	Rule      string `json:"rule,omitempty"`        // statustune rule id, when a rule decided it
	Subagents int    `json:"subagents,omitempty"`   // S: subagents in flight at the edge
	DurPrevMs int64  `json:"dur_prev_ms,omitempty"` // how long `from` was held (the closed interval)

	// Subagent payload (subagent_spawn / subagent_stop). AgentType is kept at the
	// minimal tier (it names the agent kind, not your work); Description is
	// scrubbed (it is the task content).
	ToolUseID string `json:"tool_use_id,omitempty"` // links a spawn to its stop
	AgentType string `json:"agent_type,omitempty"`  // e.g. "Explore", "general-purpose"

	// Usage payload (usage_sample): tokens accrued since the previous sample.
	// Model names the model the tokens were spent on (e.g. "claude-opus-4-8"),
	// used to price the sample; it is minimal-safe (names the model tier, not
	// your work) so it is kept alongside the token counts at the minimal tier.
	TokIn          int64  `json:"tok_in,omitempty"`
	TokOut         int64  `json:"tok_out,omitempty"`
	TokCacheRead   int64  `json:"tok_cache_read,omitempty"`
	TokCacheCreate int64  `json:"tok_cache_create,omitempty"`
	Model          string `json:"model,omitempty"`

	// Full-tier only (scrubbed at the minimal tier).
	CWD         string `json:"cwd,omitempty"`         // working directory
	Pending     string `json:"pending,omitempty"`     // the tool a permission prompt was for
	Reason      string `json:"reason,omitempty"`      // human detail behind a rule
	Description string `json:"description,omitempty"` // a subagent's task description
	Label       string `json:"label,omitempty"`       // a session's current name (session_label) — can name your work, so scrubbed
}

// DefaultDir is where the activity log lives: $XDG_STATE_HOME/switchboard/history,
// falling back to ~/.local/state/switchboard/history. State (durable, not
// regenerable), deliberately not cache — losing a day of history is permanent,
// unlike state.json which the daemon rebuilds from /proc.
func DefaultDir() string {
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "switchboard", "history")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "switchboard", "history")
}

// dayKey is the UTC day a timestamp partitions into (the day-file basename).
func dayKey(ts time.Time) string {
	return ts.UTC().Format("2006-01-02")
}

// HeldMs is how long a status was held — the DurPrevMs of the interval a
// transition closes — given when it began and `now`. A zero `since` (the first
// edge of a session, or a fresh rehydrate) yields 0 rather than a nonsensical
// since-epoch duration.
func HeldMs(since, now time.Time) int64 {
	if since.IsZero() {
		return 0
	}
	return now.Sub(since).Milliseconds()
}
