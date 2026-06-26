package main

import (
	"os"
	"time"

	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/statustune"
	"github.com/tjmisko/switchboard/internal/transcript"
)

// Bits recording which fanout events we have already emitted for a Task id.
const (
	spawnEmitted uint8 = 1 << iota
	stopEmitted
)

// reconcileState is the per-session bookkeeping the reconciler carries ACROSS
// ticks to derive fanout (subagent spawn/stop) and usage (token) events from the
// transcript. It cannot live on the store snapshot — it is daemon-internal
// cursor state (which Task ids we have already reported, how far into the
// transcript we have summed usage). Keyed by pid; pruned when a session dies.
type reconcileState struct {
	tasks       map[int]map[string]uint8 // pid -> toolUseID -> spawn/stop bits
	usageOffset map[int]int64            // pid -> transcript bytes already summed for usage
}

func newReconcileState() *reconcileState {
	return &reconcileState{
		tasks:       map[int]map[string]uint8{},
		usageOffset: map[int]int64{},
	}
}

// observe updates c.InFlightSubagents and emits any new subagent_spawn/stop and
// usage_sample events for one claude session. It runs inside the reconcile Apply
// (under the store lock); sink.Record is non-blocking, and the transcript reads
// are bounded — the same I/O profile as the status self-heals in the same loop.
func (rs *reconcileState) observe(sink *history.Sink, sess *state.Session, c *state.AgentInfo, now time.Time, tun statustune.Tuning) {
	if c.Transcript == "" {
		return
	}
	rs.observeFanout(sink, sess, c, now, tun)
	rs.observeUsage(sink, sess, c, now)
}

// observeFanout diffs the Task set against what we have already reported, emits
// spawn/stop for the new transitions, and refreshes the in-flight count (the S
// dimension behind the delegating status). A spawn carries the agent type and
// description; a stop links back by tool_use_id.
func (rs *reconcileState) observeFanout(sink *history.Sink, sess *state.Session, c *state.AgentInfo, now time.Time, tun statustune.Tuning) {
	tasks, err := transcript.Tasks(c.Transcript, tun.TailBytes)
	if err != nil {
		return // leave the last-known count rather than guess
	}
	seen := rs.tasks[sess.PID]
	if seen == nil {
		seen = map[string]uint8{}
		rs.tasks[sess.PID] = seen
	}
	inflight := 0
	for _, tk := range tasks {
		if !tk.Done {
			inflight++
		}
		bits := seen[tk.ID]
		if bits&spawnEmitted == 0 {
			sink.Record(history.Event{
				Ts: now, Type: history.EventSubagentSpawn,
				SessionID: c.SessionID, PID: sess.PID, Agent: sess.Agent, CWD: sess.CWD,
				ToolUseID: tk.ID, AgentType: tk.AgentType, Description: tk.Description,
			})
			bits |= spawnEmitted
		}
		if tk.Done && bits&stopEmitted == 0 {
			sink.Record(history.Event{
				Ts: now, Type: history.EventSubagentStop,
				SessionID: c.SessionID, PID: sess.PID, Agent: sess.Agent, CWD: sess.CWD,
				ToolUseID: tk.ID, AgentType: tk.AgentType,
			})
			bits |= stopEmitted
		}
		seen[tk.ID] = bits
	}
	c.InFlightSubagents = inflight
}

// observeUsage samples the token delta since the last offset and emits a
// usage_sample. On first sight of a session it primes the cursor to the current
// file size WITHOUT emitting, so a pre-existing transcript's backlog is not
// dumped as one spike dated at daemon start — only usage accrued while we are
// watching is recorded.
func (rs *reconcileState) observeUsage(sink *history.Sink, sess *state.Session, c *state.AgentInfo, now time.Time) {
	off, primed := rs.usageOffset[sess.PID]
	if !primed {
		if fi, err := os.Stat(c.Transcript); err == nil {
			rs.usageOffset[sess.PID] = fi.Size()
		} else {
			rs.usageOffset[sess.PID] = 0
		}
		return
	}
	u, newOff, err := transcript.UsageSince(c.Transcript, off)
	if err != nil {
		return
	}
	rs.usageOffset[sess.PID] = newOff
	if u.IsZero() {
		return
	}
	sink.Record(history.Event{
		Ts: now, Type: history.EventUsageSample,
		SessionID: c.SessionID, PID: sess.PID, Agent: sess.Agent, CWD: sess.CWD,
		TokIn: u.InputTokens, TokOut: u.OutputTokens,
		TokCacheRead: u.CacheReadTokens, TokCacheCreate: u.CacheCreationTokens,
	})
}

// prune drops cursor state for pids no longer tracked, so the maps do not grow
// without bound as sessions come and go.
func (rs *reconcileState) prune(m map[int]*state.Session) {
	for pid := range rs.tasks {
		if _, ok := m[pid]; !ok {
			delete(rs.tasks, pid)
		}
	}
	for pid := range rs.usageOffset {
		if _, ok := m[pid]; !ok {
			delete(rs.usageOffset, pid)
		}
	}
}
