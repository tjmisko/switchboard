package main

import (
	"os"
	"time"

	"github.com/tjmisko/switchboard/internal/fanout"
	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/label"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/transcript"
)

// reconcileState is the per-session bookkeeping the reconciler carries ACROSS
// ticks. The usage (token) and label cursors are daemon-internal, keyed by pid,
// and pruned when a session dies. Subagent fanout detection is delegated to the
// Observer, which owns InFlightSubagents and the subagent_spawn/stop events and
// keys its own durable state by session-id (so it survives a daemon restart or a
// `claude --resume` rather than re-emitting historical spawns).
type reconcileState struct {
	fanout      *fanout.Observer
	usageOffset map[int]int64  // pid -> transcript bytes already summed for usage
	labels      map[int]string // pid -> last-emitted session label (change dedup)
}

func newReconcileState(obs *fanout.Observer) *reconcileState {
	return &reconcileState{
		fanout:      obs,
		usageOffset: map[int]int64{},
		labels:      map[int]string{},
	}
}

// observe updates c.InFlightSubagents and emits any new subagent_spawn/stop and
// usage_sample events for one claude session. It runs inside the reconcile Apply
// (under the store lock); sink.Record is non-blocking, and the transcript reads
// are bounded — the same I/O profile as the status self-heals in the same loop.
func (rs *reconcileState) observe(sink *history.Sink, sess *state.Session, c *state.AgentInfo, now time.Time) {
	// The session label is derived from disk/window title, not the transcript, so
	// it is tracked even before the transcript exists.
	rs.observeLabel(sink, sess, c, now)
	if c.Transcript == "" {
		return
	}
	rs.observeFanout(sink, sess, c, now)
	rs.observeUsage(sink, sess, c, now)
}

// observeLabel records the session's current display name when it changes. The
// name is derived via label.RawName (the Claude session name on disk, else the
// wezterm title, else the cwd basename), and an EventSessionLabel is emitted only
// when it differs from the last-seen value for this pid — so a renamed/relocated
// session leaves a multilabel-over-time trail without spamming an event per tick.
// The label is full-tier content (it can name your work) and is scrubbed at the
// minimal tier by the sink.
func (rs *reconcileState) observeLabel(sink *history.Sink, sess *state.Session, c *state.AgentInfo, now time.Time) {
	name := label.RawName(*sess)
	if name == "" || name == rs.labels[sess.PID] {
		return
	}
	rs.labels[sess.PID] = name
	sink.Record(history.Event{
		Ts: now, Type: history.EventSessionLabel,
		SessionID: c.SessionID, PID: sess.PID, Agent: sess.Agent, CWD: sess.CWD,
		Label: name,
	})
}

// observeFanout delegates subagent fanout detection to the Observer — the single
// source of truth for InFlightSubagents and the subagent_spawn/stop events — and
// records whatever events it returns. The Observer reads the authoritative
// per-session subagents/ metadata dir (immune to the transcript tail's scroll-out)
// plus a forward cursor, so a multi-agent fan-out or a long-running subagent whose
// spawn and result straddle the 128 KiB window is no longer lost. The same
// Reconcile is invoked from the SubagentStart/Stop hook for hook-speed updates;
// the Observer's durable per-session state dedups across both triggers.
func (rs *reconcileState) observeFanout(sink *history.Sink, sess *state.Session, c *state.AgentInfo, now time.Time) {
	if rs.fanout == nil {
		return
	}
	for _, ev := range rs.fanout.Reconcile(sess, c, now) {
		sink.Record(ev)
	}
}

// observeUsage samples the token delta since the last offset and emits one
// usage_sample per model the delta touched, each tagged with Event.Model so the
// deriver can price it at that model's rate. On first sight of a session it
// primes the cursor to the current file size WITHOUT emitting, so a pre-existing
// transcript's backlog is not dumped as one spike dated at daemon start — only
// usage accrued while we are watching is recorded. Cost is deliberately NOT
// computed here; the sample only carries the model name and raw token counts.
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
	byModel, newOff, err := transcript.UsageSinceByModel(c.Transcript, off)
	if err != nil {
		return
	}
	rs.usageOffset[sess.PID] = newOff
	for model, u := range byModel {
		if u.IsZero() {
			continue
		}
		sink.Record(history.Event{
			Ts: now, Type: history.EventUsageSample,
			SessionID: c.SessionID, PID: sess.PID, Agent: sess.Agent, CWD: sess.CWD,
			Model: model,
			TokIn: u.InputTokens, TokOut: u.OutputTokens,
			TokCacheRead: u.CacheReadTokens, TokCacheCreate: u.CacheCreationTokens,
		})
	}
}

// prune drops cursor state for pids no longer tracked, so the maps do not grow
// without bound as sessions come and go. The Observer's per-session state is
// pruned against the set of live session-ids (it is keyed by session-id, not pid).
func (rs *reconcileState) prune(m map[int]*state.Session) {
	for pid := range rs.usageOffset {
		if _, ok := m[pid]; !ok {
			delete(rs.usageOffset, pid)
		}
	}
	for pid := range rs.labels {
		if _, ok := m[pid]; !ok {
			delete(rs.labels, pid)
		}
	}
	if rs.fanout != nil {
		live := map[string]bool{}
		for _, sess := range m {
			if sess.Claude != nil && sess.Claude.SessionID != "" {
				live[sess.Claude.SessionID] = true
			}
		}
		rs.fanout.Prune(live)
	}
}
