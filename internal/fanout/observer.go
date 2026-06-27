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

// Reconcile brings the Observer's view of one Claude session up to date and
// returns the subagent_spawn/stop events to record (each exactly once). It also
// sets c.InFlightSubagents to the durable spawned-minus-completed count over the
// session's direct children (spawnDepth<2). A nil/empty/idless session, or a
// transcript that cannot be scanned, is a no-op that leaves the last-known count.
func (o *Observer) Reconcile(sess *state.Session, c *state.AgentInfo, now time.Time) []history.Event {
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
		// G1: seed the seen-set from already-emitted history so a restart or
		// `--resume` does not re-emit historical spawns (metas are never deleted).
		// Prime the cursor to EOF — the dir scan is the authoritative spawn source,
		// so there is no need to re-read the whole transcript on every restart.
		if sp, st, err := history.PriorSubagentState(o.dir, c.SessionID); err == nil {
			ss.spawned, ss.stopped = sp, st
		}
		ss.offset = fileSize(c.Transcript)
		ss.seeded = true
	}

	// 1) Advance the forward cursor. It supplies the run_in_background flag (only
	// the parent tool_use carries it) and a secondary tool_result completion
	// cross-check. G5: on /clear or compaction the file shrinks below the offset —
	// re-read from 0 once (the agent-id seen-set keeps emission idempotent), and
	// never let the offset run past EOF.
	if size := fileSize(c.Transcript); ss.offset > size {
		ss.offset = 0
	}
	if spawns, resultIDs, newOff, err := transcript.TasksSince(c.Transcript, ss.offset); err == nil {
		ss.offset = newOff
		for _, t := range spawns {
			if t.Background && t.ID != "" {
				ss.background[t.ID] = true
			}
		}
		for _, id := range resultIDs {
			ss.resultDone[id] = true
		}
	}

	// 2) Authoritative dir scan: every subagent of this session, keyed by the
	// universal agent-id, immune to transcript scroll-out.
	subs, err := transcript.SubagentsForTranscript(c.Transcript)
	if err != nil {
		return nil // leave the last-known count rather than guess
	}

	var events []history.Event
	inflight := 0
	for _, s := range subs {
		if s.AgentID == "" {
			continue
		}
		if !ss.spawned[s.AgentID] {
			ss.spawned[s.AgentID] = true
			bg := s.ToolUseID != "" && ss.background[s.ToolUseID]
			events = append(events, o.spawnEvent(sess, c, s, now, bg))
		}
		// Completion, most-authoritative first: the subagent's own jsonl reached a
		// terminal entry (universal — every subagent has one), else its tool_result
		// landed (classic fanouts only), else a hard cap on a quiescent transcript
		// force-closes a stalled/aborted subagent so in-flight can never leak.
		done := s.Done
		if !done && s.ToolUseID != "" && ss.resultDone[s.ToolUseID] {
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
