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

// DefaultWorkflowQuietGrace is how long a workflow run with NO agent in flight
// keeps counting as active on journal freshness alone. It bridges the
// milliseconds-to-seconds gaps a run's script spends between fan-out waves
// (agents all resulted, next wave not yet started) without holding a drained
// run open for long: once the journal has been quiet this long with nothing in
// flight, the run is over and its workflow_stop is emitted.
const DefaultWorkflowQuietGrace = 90 * time.Second

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

	// Workflow runs (subagents/workflows/wf_*/): one cursor per run, plus the
	// start/stop already-emitted sets seeded from history so a restart mid-run
	// re-emits neither the run's workflow_start nor (via spawned/stopped above)
	// its agents' events.
	workflows   map[string]*workflowCursor // run_id -> journal cursor + seen agents
	wfAnnounced map[string]bool            // run_id -> workflow_start already emitted
	wfEnded     map[string]bool            // run_id -> workflow_stop already emitted
}

func newSessionState() *sessionState {
	return &sessionState{
		spawned:     map[string]bool{},
		stopped:     map[string]bool{},
		resultDone:  map[string]bool{},
		background:  map[string]bool{},
		workflows:   map[string]*workflowCursor{},
		wfAnnounced: map[string]bool{},
		wfEnded:     map[string]bool{},
	}
}

// workflowCursor is the durable per-run bookkeeping: a byte cursor into the
// run's journal.jsonl plus the agent sets it has yielded. The journal is the
// authoritative ledger (started/result per agent — workflow agents fire no
// hooks and no parent tool_use pairs them), so on first sight of a run the
// cursor reads it from offset 0; the session-wide spawned/stopped seen-sets
// keep that re-read idempotent across daemon restarts.
type workflowCursor struct {
	offset   int64
	name     string          // workflow name from the persisted script; sticky once resolved
	started  map[string]bool // agent_id -> journal `started` seen
	resulted map[string]bool // agent_id -> journal `result` seen (authoritative completion)
	closed   map[string]bool // agent_id -> force-closed as stale (a killed run's orphan)
}

// Observer holds the per-session cursor + seen-set for every tracked session.
type Observer struct {
	mu         sync.Mutex
	dir        string // history dir, for first-sight seeding via PriorSubagentState
	staleCap   time.Duration
	quietGrace time.Duration // workflow quiet window (DefaultWorkflowQuietGrace)
	sessions   map[string]*sessionState // keyed by session-id
}

// NewObserver builds an Observer that seeds from the history log at historyDir.
func NewObserver(historyDir string) *Observer {
	return &Observer{dir: historyDir, staleCap: DefaultStaleCap, quietGrace: DefaultWorkflowQuietGrace, sessions: map[string]*sessionState{}}
}

// SetStaleCap overrides the force-close threshold (tuning/test hook).
func (o *Observer) SetStaleCap(d time.Duration) {
	o.mu.Lock()
	o.staleCap = d
	o.mu.Unlock()
}

// SetWorkflowQuietGrace overrides the workflow quiet window (tuning/test hook).
func (o *Observer) SetWorkflowQuietGrace(d time.Duration) {
	o.mu.Lock()
	o.quietGrace = d
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
		// Same guard for workflow runs: their dirs are never deleted either, so a
		// restart re-sights every historical run and must not re-announce it.
		if ws, we, err := history.PriorWorkflowState(o.dir, c.SessionID); err == nil {
			ss.wfAnnounced, ss.wfEnded = ws, we
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
	if spawns, results, newOff, err := transcript.TasksSince(c.Transcript, ss.offset); err == nil {
		ss.offset = newOff
		for _, t := range spawns {
			if t.Background && t.ID != "" {
				ss.background[t.ID] = true
			}
		}
		for _, r := range results {
			// A launch ack is the ONLY thing the parent transcript ever says about an
			// async fanout: it lands ~2s after the spawn and the real completion
			// arrives later as a <task-notification> entry, never as a tool_result.
			// So an ack marks the spawn background (which is what it proves) and must
			// never reach resultDone — recording it there force-closes a subagent
			// seconds after it starts, which is what made a session with four live
			// background agents render idle.
			if r.LaunchAck {
				ss.background[r.ToolUseID] = true
				continue
			}
			ss.resultDone[r.ToolUseID] = true
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
		// terminal entry (universal — every subagent has one), else its parent
		// tool_result landed, else a hard cap on a quiescent transcript force-closes a
		// stalled/aborted subagent so in-flight can never leak.
		done := s.Done
		// The parent tool_result is the real completion only for a FOREGROUND fanout.
		// An async fanout's parent result is a spawn ack that lands ~2s after launch
		// and is NOT completion; TasksSince now classifies those (TaskResult.LaunchAck)
		// so they never enter resultDone at all, and an async fanout completes via its
		// jsonl terminal (s.Done) or the stale cap.
		//
		// The background check below is a second line of defence, not the primary one.
		// It cannot be relied on alone: it is fed by the run_in_background input flag,
		// which no Agent spawn in the measured corpus has ever set (see
		// transcript.launchAckPrefixes). The ack classification is what actually holds.
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
	// 3) Workflow runs (subagents/workflows/wf_*/): the fan-outs the flat scan
	// above cannot see. Their agents are spawnDepth-1 children, so they count
	// toward the same in-flight total — an idle main thread with a workflow
	// running reads delegating (green) exactly like a hand-launched fanout.
	events = append(events, o.reconcileWorkflowsLocked(sess, c, ss, now, &inflight)...)
	c.InFlightSubagents = inflight
	return events
}

// reconcileWorkflowsLocked brings the per-run workflow cursors up to date,
// adds each active run's in-flight agents to *inflight, refreshes
// c.Workflows (the wire summary renderers annotate the green chip with), and
// returns the workflow_start/stop and per-agent subagent_spawn/stop events to
// record. Caller holds o.mu.
//
// Per-agent completion is the run's JOURNAL (`result` event), never the agent
// transcript's last line: a workflow agent that returns via structured output
// ends its jsonl with a user tool_result, not an assistant end_turn, so the
// flat-dir Done detection would hold it in flight forever. A killed run's
// orphans (journal `started` with no `result`, transcript gone quiet) are
// force-closed by the same staleCap as stalled flat-dir subagents.
func (o *Observer) reconcileWorkflowsLocked(sess *state.Session, c *state.AgentInfo, ss *sessionState, now time.Time, inflight *int) []history.Event {
	runs, err := transcript.WorkflowRunsForTranscript(c.Transcript)
	if err != nil {
		return nil // leave the last-known Workflows rather than guess
	}
	var events []history.Event
	var statuses []state.WorkflowStatus
	for _, run := range runs {
		wc := ss.workflows[run.RunID]
		if wc == nil {
			wc = &workflowCursor{
				name:     run.Name,
				started:  map[string]bool{},
				resulted: map[string]bool{},
				closed:   map[string]bool{},
			}
			ss.workflows[run.RunID] = wc
		}
		if run.Name != "" {
			wc.name = run.Name
		}

		// Advance the journal cursor. A missing journal (a just-launched run that
		// has not started its first agent) is "nothing yet", not an error state.
		newStarted, newResulted, newOff, jerr := transcript.WorkflowJournalSince(run.Journal, wc.offset)
		if jerr == nil {
			wc.offset = newOff
		}
		for _, id := range newStarted {
			wc.started[id] = true
		}
		for _, id := range newResulted {
			wc.resulted[id] = true
		}

		// In-flight = started − resulted − stale-closed. Staleness reads the
		// agent's own transcript mtime (its activity heartbeat), falling back to
		// the journal's when the transcript never appeared, so a killed run's
		// orphans age out rather than pinning the count (journals record no
		// terminal event, and several historical runs sit at started > resulted
		// forever).
		running := 0
		for id := range wc.started {
			if wc.resulted[id] || wc.closed[id] {
				continue
			}
			if mt, ok := workflowAgentMtime(run, id); ok && now.Sub(mt) > o.staleCap {
				wc.closed[id] = true
				continue
			}
			running++
		}

		// Active = agents in flight, or a journal still fresh (bridging the
		// between-waves instant where everything has resulted and the next wave
		// has not started). A drained run goes inactive once the journal has been
		// quiet past the grace.
		active := running > 0
		if !active {
			if mt, ok := statMtime(run.Journal); ok && now.Sub(mt) <= o.quietGrace {
				active = true
			} else if !ok {
				// No journal yet: a run dir that JUST appeared is a launching
				// workflow; one long dead (daemon-start backfill) is not.
				if dt, dok := statMtime(run.Dir); dok && now.Sub(dt) <= o.quietGrace {
					active = true
				}
			}
		}

		// The run's own bracket events. workflow_start precedes its agents'
		// spawns in the log; a run re-activated after a stop (journal grew again)
		// opens a fresh start/stop episode.
		if active && (!ss.wfAnnounced[run.RunID] || ss.wfEnded[run.RunID]) {
			ss.wfAnnounced[run.RunID] = true
			ss.wfEnded[run.RunID] = false
			events = append(events, o.workflowEvent(history.EventWorkflowStart, sess, c, run.RunID, wc.name, now))
		}

		// Per-agent spawn/stop, deduped by the session-wide seen-sets (a restart
		// re-reads the journal from 0; the seeded sets keep it idempotent).
		for _, id := range newStarted {
			if id == "" || ss.spawned[id] {
				continue
			}
			ss.spawned[id] = true
			s := transcript.Subagent{AgentID: id, AgentType: transcript.WorkflowAgentType, SpawnDepth: 1}
			if mt, ok := workflowAgentMtime(run, id); ok {
				s.ModTime = mt
			}
			ev := o.spawnEvent(sess, c, s, now, false)
			ev.WorkflowRunID = run.RunID
			events = append(events, ev)
		}
		for _, id := range newResulted {
			if id == "" || ss.stopped[id] {
				continue
			}
			ss.stopped[id] = true
			ev := o.stopEvent(sess, c, transcript.Subagent{AgentID: id, AgentType: transcript.WorkflowAgentType}, now)
			ev.WorkflowRunID = run.RunID
			events = append(events, ev)
		}
		for id := range wc.closed {
			if ss.stopped[id] {
				continue
			}
			ss.stopped[id] = true
			ev := o.stopEvent(sess, c, transcript.Subagent{AgentID: id, AgentType: transcript.WorkflowAgentType}, now)
			ev.WorkflowRunID = run.RunID
			events = append(events, ev)
		}

		if !active {
			if ss.wfAnnounced[run.RunID] && !ss.wfEnded[run.RunID] {
				ss.wfEnded[run.RunID] = true
				events = append(events, o.workflowEvent(history.EventWorkflowStop, sess, c, run.RunID, wc.name, now))
			}
			continue
		}

		*inflight += running
		statuses = append(statuses, state.WorkflowStatus{
			RunID:         run.RunID,
			Name:          wc.name,
			AgentsStarted: len(wc.started),
			AgentsDone:    len(wc.resulted),
			InFlight:      running,
		})
	}
	// statuses follows runs, which ReadDir yields in name order — already the
	// sorted-by-RunID contract state.AgentInfo.Workflows requires. nil when no
	// run is active, so the wire field omits cleanly.
	c.Workflows = statuses
	return events
}

// workflowEvent builds a workflow_start/stop bracket event. The workflow's
// name rides in Label (scrubbed at the minimal tier — it names your work); the
// run id is the minimal-safe pairing key.
func (o *Observer) workflowEvent(evType string, sess *state.Session, c *state.AgentInfo, runID, name string, now time.Time) history.Event {
	return history.Event{
		Ts: now, Type: evType,
		SessionID: c.SessionID, PID: sess.PID, Agent: sess.Agent, CWD: sess.CWD,
		WorkflowRunID: runID, Label: name,
	}
}

// workflowAgentMtime is the activity heartbeat for one workflow agent: its own
// transcript's mtime, else the run journal's (an agent whose jsonl never
// appeared can only be as fresh as the journal that announced it).
func workflowAgentMtime(run transcript.WorkflowRun, agentID string) (time.Time, bool) {
	if mt, ok := statMtime(run.AgentTranscript(agentID)); ok {
		return mt, true
	}
	return statMtime(run.Journal)
}

// statMtime is a path's mtime, or false when it cannot be stat-ed.
func statMtime(path string) (time.Time, bool) {
	if path == "" {
		return time.Time{}, false
	}
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}, false
	}
	return fi.ModTime(), true
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
