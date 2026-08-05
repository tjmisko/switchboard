package main

import (
	"os"
	"strconv"
	"time"

	"github.com/tjmisko/switchboard/internal/fanout"
	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/label"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/transcript"
)

// reconcileState is the per-session bookkeeping the reconciler carries ACROSS
// ticks. The usage cursor is daemon-internal and keyed by pid; the label cursor
// is keyed by session id (see observeLabel) and carries the pid it belongs to, so
// both are pruned when the process they track dies. Subagent fanout detection is
// delegated to the
// Observer, which owns InFlightSubagents and the subagent_spawn/stop events and
// keys its own durable state by session-id (so it survives a daemon restart or a
// `claude --resume` rather than re-emitting historical spawns).
// Memory is the odd one out: it is sampled BEFORE the lock is taken rather than
// inside the loop below, because its /proc reads are milliseconds rather than
// microseconds (see memorySampler). nil when memory sampling is off.
type reconcileState struct {
	fanout      *fanout.Observer
	memory      *memorySampler
	usageOffset map[int]int64            // pid -> transcript bytes already summed for usage
	labels      map[string]labelCursor   // labelKey -> last-emitted session label (change dedup)
	names       *label.NameCache         // pid -> Claude session name, memoized against the file's stamp
	samples     map[string]fanout.Sample // session id -> this tick's pre-lock fanout reads (see sampleFanout)
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
		fanout:      obs,
		memory:      mem,
		usageOffset: map[int]int64{},
		labels:      map[string]labelCursor{},
		names:       &label.NameCache{},
	}
}

// sampleFanout does ALL of the fanout Observer's reading for every session in the
// last published snapshot, BEFORE the tick takes the store lock: the first-sight
// history seed, the forward transcript cursor, and the per-session subagents/ dir
// scan.
//
// It follows the same rule as sampleMemory directly below it, and for the same
// reason: Store.Apply blocks every RPC reader and every hook for as long as it
// holds, so nothing that touches a disk belongs inside it. Two distinct costs
// lived in there, and they showed up differently on the live daemon:
//
//   - the seed reads the history archive, and produced 481-639ms holds the first
//     time a tick saw a newly started session (or an existing pid taking a new
//     session id);
//   - the dir scan runs EVERY tick for EVERY session, and is the steady-state
//     body of the hold rather than its tail.
//
// Read through Snapshot — the shared read lock — never through Apply. A session
// that appears between this snapshot and the lock, or that something else
// reconciles in between, is simply read inline inside Reconcile exactly as before:
// the sample is checked against the state it was taken against and discarded if
// that state has moved (fanout.Sample.usableFor).
//
// It does shift WHEN a change is seen, which the guard cannot and should not
// prevent: a fanout that starts or ends inside the window between this sampling
// and the tick's Apply is folded in on the NEXT tick, so its stop event is dated a
// tick later than an inline read would have dated it. That lag is inherent to
// sampling and bounded by the tick interval. The hazard the guard does remove is
// the one that is not merely late — a sample overwriting a count that a fresher
// read (the SubagentStart/Stop hook's own Reconcile) already established.
func (rs *reconcileState) sampleFanout(store *state.Store) {
	if rs.fanout == nil {
		return
	}
	// A fresh map per tick: a sample is only valid against the state it was taken
	// against, and keeping last tick's around would just be dead weight for the
	// usableFor check to reject.
	rs.samples = map[string]fanout.Sample{}
	for _, sess := range store.Snapshot().Sessions {
		c := sess.Claude
		if c == nil || c.SessionID == "" || c.Transcript == "" {
			continue
		}
		rs.samples[sampleKey(c)] = rs.fanout.Sample(c.SessionID, c.Transcript)
	}
}

// sampleKey identifies the reads one session needs. The session id alone does not:
// two store sessions can carry the same id with different transcripts (two panes
// resumed onto one conversation, or a hook payload with a transcript_path but no
// session_id, which handleHook tolerates), and under an id-only key one of them
// silently overwrites the other's sample. The Observer rejects a sample taken
// against another transcript anyway, so this is the difference between both
// sessions using their own reads and one of them falling back to an inline read
// under the lock — which is the cost this whole path exists to avoid.
func sampleKey(c *state.AgentInfo) string {
	return c.SessionID + "\x00" + c.Transcript
}

// observe updates c.InFlightSubagents and emits any new subagent_spawn/stop
// events for one claude session. It runs inside the reconcile Apply (under the
// store lock) because it writes to c, which the store owns.
//
// It performs no reads of its own any more. The fanout reads come from the
// pre-lock sample (sampleFanout), the name comes from a stamp-checked cache, and
// the usage delta left entirely — see sampleUsage.
func (rs *reconcileState) observe(sink *history.Sink, sess *state.Session, c *state.AgentInfo, now time.Time) {
	// The session label is derived from disk/window title, not the transcript, so
	// it is tracked even before the transcript exists.
	rs.observeLabel(sink, sess, c, now)
	if c.Transcript == "" {
		return
	}
	rs.observeFanout(sink, sess, c, now)
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
// The name lookup goes through rs.names because this runs under the store lock,
// once per session per tick, and label.RawName reads and unmarshals a file on
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
//
// This runs under the store lock, so it applies the reads sampleFanout already
// took outside it. A session with no sample — one that appeared after the
// snapshot — falls back to reading inline, which is what this always did.
func (rs *reconcileState) observeFanout(sink *history.Sink, sess *state.Session, c *state.AgentInfo, now time.Time) {
	if rs.fanout == nil {
		return
	}
	for _, ev := range rs.fanout.ReconcileFrom(rs.samples[sampleKey(c)], sess, c, now) {
		sink.Record(ev)
	}
}

// sampleUsage runs observeUsage for every session in the last published snapshot,
// BEFORE the tick takes the store lock.
//
// It can move out wholesale, unlike the fanout observation next to it, because it
// mutates no session state at all: it reads a transcript delta, advances a
// daemon-internal cursor, and emits history events. Nothing a consumer can see
// through the store depends on it, and sink.Record is non-blocking, so there was
// never a reason for the read to be inside the lock beyond the loop it happened
// to sit in.
//
// This was the last transcript read left under the lock. It is easy to overlook
// because first sight is genuinely cheap — the cursor primes with a bare os.Stat
// and returns without reading — so an audit that only looks at a newly-appeared
// session concludes, wrongly, that this path is already cold-safe. Every tick
// AFTER that reads the delta, and during an active turn the delta is real bytes.
func (rs *reconcileState) sampleUsage(store *state.Store, sink *history.Sink, now time.Time) {
	for _, snap := range store.Snapshot().Sessions {
		sess := snap
		c := sess.Claude
		if c == nil || c.Transcript == "" {
			continue
		}
		rs.observeUsage(sink, &sess, c, now)
	}
}

// observeUsage samples the token delta since the last offset and emits one
// usage_sample per model the delta touched, each tagged with Event.Model so the
// deriver can price it at that model's rate. On first sight of a session it
// primes the cursor to the current file size WITHOUT emitting, so a pre-existing
// transcript's backlog is not dumped as one spike dated at daemon start — only
// usage accrued while we are watching is recorded. Cost is deliberately NOT
// computed here; the sample only carries the model name and raw token counts.
//
// Called from sampleUsage, outside the store lock. It is keyed by pid and only
// the reconcile tick ever calls it, so the cursor has a single writer.
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
	for pid := range rs.usageOffset {
		if _, ok := m[pid]; !ok {
			delete(rs.usageOffset, pid)
		}
	}
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
