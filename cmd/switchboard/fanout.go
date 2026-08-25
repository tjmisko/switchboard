package main

import (
	"log"
	"strconv"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/fanout"
	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/label"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/transcript"
)

// reconcileState is the per-session bookkeeping the reconciler carries ACROSS
// ticks. Claude usage is tracked by stable session/transcript/message identity
// in a durable cursor under the history directory, while the label cursor is
// keyed by session id (see observeLabel) and carries the pid it belongs to.
// Subagent fanout detection is delegated to the
// Observer, which owns InFlightSubagents and the subagent_spawn/stop events and
// keys its own durable state by session-id (so it survives a daemon restart or a
// `claude --resume` rather than re-emitting historical spawns).
// Memory is the odd one out: it is sampled BEFORE the lock is taken rather than
// inside the loop below, because its /proc reads are milliseconds rather than
// microseconds (see memorySampler). nil when memory sampling is off.
type reconcileState struct {
	fanout   *fanout.Observer
	memory   *memorySampler
	usage    *transcript.UsageTracker
	usageErr string                 // last reported content-free cursor error; suppresses per-tick log spam
	labels   map[string]labelCursor // labelKey -> last-emitted session label (change dedup)
	names    *label.NameCache       // pid -> Claude session name, memoized against the file's stamp
}

// labelCursor is the last name emitted for one session, plus the pid hosting it.
// The key dedups (per session, so a /clear re-announces), the pid decides
// lifetime (per process, so a tick that cannot see the session id cannot expire
// it) — see observeLabel and prune.
type labelCursor struct {
	name string
	pid  int
}

func newReconcileState(obs *fanout.Observer, mem *memorySampler) *reconcileState {
	return &reconcileState{
		fanout: obs,
		memory: mem,
		labels: map[string]labelCursor{},
		names:  &label.NameCache{},
	}
}

// observe updates c.InFlightSubagents and emits any new subagent_spawn/stop and
// usage_sample events for one claude session. It runs on a detached reconcile
// snapshot before Store.Apply; sink.Record is non-blocking and all transcript
// reads finish before the live session map is locked.
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

// observeAuxiliary preserves provider-independent Claude label and token-usage
// history after the structured adapter becomes graph-authoritative. Fanout and
// status remain exclusively adapter-owned; these two reads do not mutate the
// live session projection.
func (rs *reconcileState) observeAuxiliary(sink *history.Sink, sess *state.Session, c *state.AgentInfo, now time.Time) {
	rs.observeLabel(sink, sess, c, now)
	if c.Transcript != "" {
		rs.observeUsage(sink, sess, c, now)
	}
}

// observeLabel records the session's current display name when it changes. The
// name is derived via label.RawName (the Claude session name on disk, else the
// wezterm title, else the cwd basename), and an EventSessionLabel is emitted only
// when it differs from the last-seen value for this SESSION — so a
// renamed/relocated session leaves a multilabel-over-time trail without spamming
// an event per tick. The label is full-tier content (it can name your work) and
// is scrubbed at the minimal tier by the sink.
//
// Dedup keys on the session id, not the pid, because one process hosts a
// SEQUENCE of sessions: a /clear or a new conversation mints a fresh id in the
// same pane, and the timeline (correctly) reads that as a new lane. Under a
// pid-keyed cursor the unchanged name would be deduped away, so the new lane
// would never be told its name and would render as never-named for the rest of
// its life — while the name it is plainly wearing shows in the status bar. A
// session id is also the safer key in the other direction: it never repeats, so
// a recycled pid can never inherit a dead session's name.
//
// The name lookup goes through rs.names because it runs once per session per
// tick, and label.RawName reads and unmarshals a file on
// every call. The cache re-reads when the file's stamp moves, so a `/name` still
// lands an EventSessionLabel on the next tick.
func (rs *reconcileState) observeLabel(sink *history.Sink, sess *state.Session, c *state.AgentInfo, now time.Time) {
	name := rs.names.RawName(*sess)
	key := labelKey(sess.PID, c)
	if name == "" || name == rs.labels[key].name {
		return
	}
	rs.labels[key] = labelCursor{name: name, pid: sess.PID}
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

// observeUsage backfills and incrementally reads the root plus every child
// transcript through a durable, session-keyed UsageTracker. One event is emitted
// per logical provider-message snapshot so exact model/tier/tool dimensions and the
// provider timestamp survive until pricing. Streamed duplicates and revisions
// are resolved before emission. Cost is deliberately NOT computed here.
func (rs *reconcileState) observeUsage(sink *history.Sink, sess *state.Session, c *state.AgentInfo, now time.Time) {
	if sink == nil || !sink.Enabled() || c.SessionID == "" {
		return
	}
	if rs.usage == nil {
		tracker, err := transcript.NewUsageTracker(sink.Dir())
		if err != nil {
			rs.reportUsageError(err)
			return
		}
		rs.usage = tracker
	}
	if rs.usage == nil {
		return
	}
	_, err := rs.usage.SyncSession(c.SessionID, c.Transcript, now, func(snapshots []transcript.UsageSnapshot) error {
		events := make([]history.Event, 0, len(snapshots))
		for _, snapshot := range snapshots {
			ts := snapshot.Timestamp
			if ts.IsZero() {
				ts = now
			}
			ev := history.Event{
				Ts: ts, SessionID: c.SessionID, PID: sess.PID, Agent: sess.Agent, CWD: sess.CWD,
				Source:       agentgraph.SourceClaudeTranscript,
				UsageEventID: snapshot.UsageEventID, UsageSnapshot: true, UsageRevision: snapshot.UsageRevision,
				UsageCoverage: snapshot.Coverage,
			}
			if snapshot.Cutover {
				ev.Type = history.EventUsageCutover
				events = append(events, ev)
				continue
			}
			u := snapshot.Usage
			ev.Type = history.EventUsageSample
			ev.Model = snapshot.Model
			ev.TokIn, ev.TokOut = u.InputTokens, u.OutputTokens
			ev.TokCacheRead, ev.TokCacheCreate = u.CacheReadTokens, u.CacheCreationTokens
			ev.TokCacheCreate5m, ev.TokCacheCreate1h = u.CacheWrite5mTokens, u.CacheWrite1hTokens
			ev.ServiceTier, ev.Speed, ev.InferenceGeo = u.ServiceTier, u.Speed, u.InferenceGeo
			ev.WebSearchRequests, ev.WebFetchRequests = u.WebSearchRequests, u.WebFetchRequests
			ev.ProviderMessageID, ev.UsageSourceID = snapshot.ProviderMessageID, snapshot.SourceID
			events = append(events, ev)
		}
		return sink.AppendDurable(events)
	})
	if err != nil {
		rs.reportUsageError(err)
		return
	}
	rs.usageErr = ""
}

func (rs *reconcileState) reportUsageError(err error) {
	message := err.Error()
	if message == rs.usageErr {
		return
	}
	rs.usageErr = message
	log.Printf("claude-usage: %v", err)
}

// labelKey identifies the thing a label cursor is about: the session, once a hook
// has supplied its id, else the process hosting it (the pre-hook lead-in, where a
// pid is all there is to dedup against).
func labelKey(pid int, c *state.AgentInfo) string {
	if c != nil && c.SessionID != "" {
		return "s:" + c.SessionID
	}
	return "p:" + strconv.Itoa(pid)
}

// prune drops cursor state for pids no longer tracked, so the maps do not grow
// without bound as sessions come and go. Label cursors expire by their pid rather
// than by whether their key is still live: observe() is skipped for a tick that
// cannot see a session's claude info, but prune runs every tick regardless, and
// expiring the cursor on that blip would re-emit an identical label the moment the
// info came back. The cost is that a session id its process has rolled past (a
// /clear) is retained until the process exits — a handful of entries per pane,
// against a duplicate event per blip. The Observer's per-session state is pruned
// against the set of live session-ids (it is keyed by session-id, not pid).
func (rs *reconcileState) prune(m map[int]*state.Session) {
	for key, cur := range rs.labels {
		if _, ok := m[cur.pid]; !ok {
			delete(rs.labels, key)
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
