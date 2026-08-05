// Package fanout is switchboard's subagent-fanout detector. It is the single
// source of truth for two things about a Claude session: how many subagents it
// has in flight (state.AgentInfo.InFlightSubagents, the S dimension behind the
// delegating/green status) and the subagent_spawn/stop history events the
// timeline turns into per-subagent spans.
//
// Detection deliberately does NOT rely on the tail-bounded transcript scan that
// the original observeFanout used — a fan-out of several agents (each tool_use
// carries the full subagent prompt) or a long-running subagent whose spawn and
// tool_result straddle the 128 KiB window would scroll out and be lost. Instead
// the authoritative source is the per-session subagents/ metadata directory
// (~/.claude/projects/<proj>/<session-id>/subagents/agent-<id>.{meta.json,jsonl}),
// which never scrolls and records every subagent — keyed by the universal
// agent-<id> filename stem, the only id present on teammates and grandchildren
// (which carry no tool_use_id). A forward byte cursor over the parent transcript
// is layered on as a secondary signal (the run_in_background flag and a
// tool_result completion cross-check), never as the spawn source, so a cursor
// reset on /clear or compaction can never lose a spawn.
//
// The Observer is called from BOTH the reconcile tick and the SubagentStart/Stop
// hook handler — single source of truth, two triggers. Every call must hold the
// store lock, but a mutex guards the maps in case a future caller does not.
package fanout

import (
	"os"
	"sync"
	"time"

	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/transcript"
)

// DefaultStaleCap bounds how long a spawned-but-unfinished subagent may go with a
// quiescent transcript before the Observer force-closes it (completion unknown),
// so a crashed/aborted subagent that writes neither a terminal entry nor a
// tool_result cannot leak as perpetual in-flight. An ACTIVE subagent keeps its
// jsonl mtime fresh as it works, so this only fires on genuinely stalled ones.
const DefaultStaleCap = 30 * time.Minute

// sessionState is the durable per-session bookkeeping carried across reconcile
// ticks. Keyed by session-id (NOT pid) so a daemon restart or `claude --resume`
// — new pid, same session-id, same subagents/ dir — reuses it after re-seeding.
type sessionState struct {
	seeded     bool
	offset     int64           // forward cursor into the parent transcript
	spawned    map[string]bool // agent_id -> spawn event already emitted
	stopped    map[string]bool // agent_id -> stop event already emitted
	resultDone map[string]bool // tool_use_id -> its tool_result landed (cursor cross-check)
	background map[string]bool // tool_use_id -> run_in_background (cursor; for timeline tagging)
}

func newSessionState() *sessionState {
	return &sessionState{
		spawned:    map[string]bool{},
		stopped:    map[string]bool{},
		resultDone: map[string]bool{},
		background: map[string]bool{},
	}
}

// Observer holds the per-session cursor + seen-set for every tracked session.
type Observer struct {
	mu       sync.Mutex
	dir      string // history dir, for first-sight seeding via PriorSubagentState
	staleCap time.Duration
	sessions map[string]*sessionState // keyed by session-id
}

// NewObserver builds an Observer that seeds from the history log at historyDir.
func NewObserver(historyDir string) *Observer {
	return &Observer{dir: historyDir, staleCap: DefaultStaleCap, sessions: map[string]*sessionState{}}
}

// SetStaleCap overrides the force-close threshold (tuning/test hook).
func (o *Observer) SetStaleCap(d time.Duration) {
	o.mu.Lock()
	o.staleCap = d
	o.mu.Unlock()
}

// Prime performs the first-sight seed for one session WITHOUT holding the store
// lock, so the reconcile tick can pay that cost before it takes the lock rather
// than inside it.
//
// It exists because the seed is the most expensive thing the Observer does and
// it used to run in the worst possible place. Reconcile is called from inside
// store.Apply, so a newly discovered session made the tick read the history
// archive with the exclusive lock held — measured on the live daemon at 481-639ms
// per new session, which every waybar subscriber, every hook, and every chip
// click queued behind. Sessions appear whenever the user starts one, so this was
// not a rare path; it was a stall you could feel every time.
//
// Calling it is optional and always safe: it is idempotent, a no-op for a session
// already seeded, and Reconcile still seeds lazily for anything that reaches it
// unprimed. Skipping it costs latency, never correctness.
//
// The file read deliberately happens with NO lock held, not even o.mu. Holding
// o.mu across it would hand the stall straight back: Reconcile takes o.mu while
// the caller holds the store lock, so a hook-triggered Reconcile would block on
// this read with the store lock in hand. Two callers racing the same cold session
// both read and both install the same answer, which is harmless.
func (o *Observer) Prime(sessionID, transcript string) {
	if o == nil || sessionID == "" {
		return
	}
	o.mu.Lock()
	ss := o.sessions[sessionID]
	seeded := ss != nil && ss.seeded
	o.mu.Unlock()
	if seeded {
		return
	}

	sp, st, off := seedFor(o.dir, sessionID, transcript)

	o.mu.Lock()
	defer o.mu.Unlock()
	ss = o.sessions[sessionID]
	if ss == nil {
		ss = newSessionState()
		o.sessions[sessionID] = ss
	}
	if ss.seeded {
		// Lost the race to a Reconcile (or another Prime) that seeded while this one
		// was reading. Its state is at least as fresh, and it may already have
		// advanced the cursor past what was measured here, so discard this read
		// rather than overwrite.
		return
	}
	ss.applySeed(sp, st, off)
}

// seedFor is the first-sight read, factored out so the pre-lock Prime and the
// under-lock backstop in Reconcile cannot drift apart. It performs I/O and takes
// no locks.
//
// G1: seed the seen-set from already-emitted history so a restart or `--resume`
// does not re-emit historical spawns (metas are never deleted). The cursor is
// primed to EOF — the dir scan is the authoritative spawn source, so there is no
// need to re-read the whole transcript on every restart.
func seedFor(dir, sessionID, transcript string) (spawned, stopped map[string]bool, offset int64) {
	if sp, st, err := history.PriorSubagentState(dir, sessionID); err == nil {
		spawned, stopped = sp, st
	}
	return spawned, stopped, fileSize(transcript)
}

// applySeed installs a seedFor result. The caller holds o.mu. A failed history
// read (nil maps) leaves the empty sets newSessionState built rather than
// nilling them out, so a later spawn can still be recorded.
func (ss *sessionState) applySeed(spawned, stopped map[string]bool, offset int64) {
	if spawned != nil {
		ss.spawned = spawned
	}
	if stopped != nil {
		ss.stopped = stopped
	}
	ss.offset = offset
	ss.seeded = true
}

// Sample is the I/O half of one session's Reconcile, taken with no store lock
// held. It is a value, not a promise: it describes the transcript and subagents
// dir as they were at sample time, against the cursor position recorded in base.
//
// The zero value is "no sample", which makes ReconcileFrom behave exactly like
// Reconcile.
type Sample struct {
	sessionID string
	base      int64 // the cursor the reads were taken against
	spawns    []transcript.Task
	resultIDs []string
	newOffset int64
	tasksOK   bool
	subs      []transcript.Subagent
	subsOK    bool
	valid     bool
}

// usableFor reports whether this sample still describes the session's current
// cursor. A sample taken against a cursor that has since moved — another
// producer reconciled in between — is discarded rather than applied, because
// applying it would rewind the cursor to a stale newOffset and re-scan bytes
// whose signals were already folded in. This is the same inversion hazard the
// resolve path hit when its enumeration moved outside the lock; here it is
// cheap to detect exactly, so it is detected rather than serialized against.
func (s Sample) usableFor(sessionID string, offset int64) bool {
	return s.valid && s.sessionID == sessionID && s.base == offset
}

// Sample performs every read one Reconcile needs, holding no lock while it does
// so, and seeds the session first if it has never been seen.
//
// This is the batched half of the Enumerate/ReconcileFrom split the resolve path
// already uses: the tick calls it for every session BEFORE taking the store
// lock, and ReconcileFrom then applies the result with the lock held but no I/O
// under it. The dir scan in particular ran per session per tick inside the lock.
func (o *Observer) Sample(sessionID, transcriptPath string) Sample {
	if o == nil || sessionID == "" || transcriptPath == "" {
		return Sample{}
	}
	o.Prime(sessionID, transcriptPath)

	o.mu.Lock()
	var base int64
	if ss := o.sessions[sessionID]; ss != nil {
		base = ss.offset
	}
	o.mu.Unlock()

	return readSample(sessionID, transcriptPath, base)
}

// readSample is the actual I/O, shared by the pre-lock Sample and the under-lock
// fallback in reconcile so the two cannot drift. It takes no locks.
//
// G5: on /clear or compaction the file shrinks below the cursor — re-read from 0
// once (the agent-id seen-set keeps emission idempotent), and never let the
// cursor run past EOF.
func readSample(sessionID, transcriptPath string, base int64) Sample {
	s := Sample{sessionID: sessionID, base: base, valid: true}
	from := base
	if size := fileSize(transcriptPath); from > size {
		from = 0
	}
	if spawns, resultIDs, newOff, err := transcript.TasksSince(transcriptPath, from); err == nil {
		s.spawns, s.resultIDs, s.newOffset, s.tasksOK = spawns, resultIDs, newOff, true
	}
	if subs, err := transcript.SubagentsForTranscript(transcriptPath); err == nil {
		s.subs, s.subsOK = subs, true
	}
	return s
}

// Reconcile brings the Observer's view of one Claude session up to date and
// returns the subagent_spawn/stop events to record (each exactly once). It also
// sets c.InFlightSubagents to the durable spawned-minus-completed count over the
// session's direct children (spawnDepth<2). A nil/empty/idless session, or a
// transcript that cannot be scanned, is a no-op that leaves the last-known count.
//
// It does its own reads. Callers with a pre-lock phase should use Sample +
// ReconcileFrom instead; the hook trigger has none, so it lands here.
func (o *Observer) Reconcile(sess *state.Session, c *state.AgentInfo, now time.Time) []history.Event {
	return o.reconcile(sess, c, now, Sample{})
}

// ReconcileFrom is Reconcile applied to reads already taken outside the lock. A
// sample that no longer matches the session's cursor is silently re-read inline,
// so this is never less correct than Reconcile — only faster when it hits.
func (o *Observer) ReconcileFrom(s Sample, sess *state.Session, c *state.AgentInfo, now time.Time) []history.Event {
	return o.reconcile(sess, c, now, s)
}

func (o *Observer) reconcile(sess *state.Session, c *state.AgentInfo, now time.Time, s Sample) []history.Event {
	if sess == nil || c == nil || c.Transcript == "" || c.SessionID == "" {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()

	ss := o.sessions[c.SessionID]
	if ss == nil {
		ss = newSessionState()
		o.sessions[c.SessionID] = ss
	}
	if !ss.seeded {
		// The lazy backstop. Prime (called before the store lock is taken) normally
		// gets here first, so this fires only for a session that appeared between
		// the caller's snapshot and the lock, or on the hook trigger, which has no
		// pre-lock phase to hang a Prime on. Correctness does not depend on which
		// path seeds — only cost does, and this one pays it under the lock.
		sp, st, off := seedFor(o.dir, c.SessionID, c.Transcript)
		ss.applySeed(sp, st, off)
	}

	if !s.usableFor(c.SessionID, ss.offset) {
		// No sample, or one the cursor has moved past. Read inline — under the lock,
		// as this always used to be. Correctness never depends on the sample; only
		// the lock hold does.
		s = readSample(c.SessionID, c.Transcript, ss.offset)
	}

	// 1) Advance the forward cursor. It supplies the run_in_background flag (only
	// the parent tool_use carries it) and a secondary tool_result completion
	// cross-check.
	if s.tasksOK {
		ss.offset = s.newOffset
		for _, t := range s.spawns {
			if t.Background && t.ID != "" {
				ss.background[t.ID] = true
			}
		}
		for _, id := range s.resultIDs {
			ss.resultDone[id] = true
		}
	}

	// 2) Authoritative dir scan: every subagent of this session, keyed by the
	// universal agent-id, immune to transcript scroll-out.
	if !s.subsOK {
		return nil // leave the last-known count rather than guess
	}

	var events []history.Event
	inflight := 0
	for _, s := range s.subs {
		if s.AgentID == "" {
			continue
		}
		if !ss.spawned[s.AgentID] {
			ss.spawned[s.AgentID] = true
			bg := s.ToolUseID != "" && ss.background[s.ToolUseID]
			events = append(events, o.spawnEvent(sess, c, s, now, bg))
		}
		// Completion, most-authoritative first: the subagent's own jsonl reached a
		// terminal entry (universal — every subagent has one), else its parent
		// tool_result landed, else a hard cap on a quiescent transcript force-closes a
		// stalled/aborted subagent so in-flight can never leak.
		done := s.Done
		// The parent tool_result is the real completion only for a FOREGROUND fanout.
		// A run_in_background fanout gets an immediate "Spawned successfully"
		// tool_result ~1s after launch that is NOT completion, so its resultDone must
		// be ignored — a background fanout completes via its jsonl terminal (s.Done)
		// or the stale cap, never the spawn-ack. (Without this guard every background
		// fanout stops ~1s after it starts.)
		if !done && s.ToolUseID != "" && !ss.background[s.ToolUseID] && ss.resultDone[s.ToolUseID] {
			done = true
		}
		if !done && !s.ModTime.IsZero() && now.Sub(s.ModTime) > o.staleCap {
			done = true
		}
		if done {
			if !ss.stopped[s.AgentID] {
				ss.stopped[s.AgentID] = true
				events = append(events, o.stopEvent(sess, c, s, now))
			}
			continue
		}
		// Still running. Count toward the main thread's in-flight only for a direct
		// child (depth 0/1 — anonymous Agent/Task fanouts are depth 1, named
		// teammates depth 0); grandchildren (depth>=2) are nested work the main
		// thread did not launch and are rendered as decoration, not counted here.
		if s.SpawnDepth < 2 {
			inflight++
		}
	}
	c.InFlightSubagents = inflight
	return events
}

// cursorFor exposes one session's forward cursor for tests that need to assert it
// did not move backwards.
func (o *Observer) cursorFor(sessionID string) int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	if ss := o.sessions[sessionID]; ss != nil {
		return ss.offset
	}
	return 0
}

// Prune drops per-session state for session-ids no longer live, bounding the map
// as sessions come and go. A pruned session that later resumes re-seeds from the
// history log on next sight, so dropping it never causes a re-emit.
func (o *Observer) Prune(live map[string]bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for id := range o.sessions {
		if !live[id] {
			delete(o.sessions, id)
		}
	}
}

func (o *Observer) spawnEvent(sess *state.Session, c *state.AgentInfo, s transcript.Subagent, now time.Time, background bool) history.Event {
	// Date the spawn at the real spawn time (meta/jsonl mtime), not reconcile-now,
	// so a backfilled span is not mis-ordered after its own stop (G9). Clamp into
	// the past-but-not-future window.
	ts := s.ModTime
	if ts.IsZero() || ts.After(now) {
		ts = now
	}
	return history.Event{
		Ts: ts, Type: history.EventSubagentSpawn,
		SessionID: c.SessionID, PID: sess.PID, Agent: sess.Agent, CWD: sess.CWD,
		AgentID: s.AgentID, ToolUseID: s.ToolUseID, AgentType: s.AgentType, Description: s.Description,
		Background: background,
	}
}

func (o *Observer) stopEvent(sess *state.Session, c *state.AgentInfo, s transcript.Subagent, now time.Time) history.Event {
	return history.Event{
		Ts: now, Type: history.EventSubagentStop,
		SessionID: c.SessionID, PID: sess.PID, Agent: sess.Agent, CWD: sess.CWD,
		AgentID: s.AgentID, ToolUseID: s.ToolUseID, AgentType: s.AgentType,
	}
}

// fileSize is the transcript's current size, or 0 when it cannot be stat-ed.
func fileSize(path string) int64 {
	if fi, err := os.Stat(path); err == nil {
		return fi.Size()
	}
	return 0
}
