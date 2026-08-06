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
// Ultracode Workflow runs (subagents/workflows/wf_*/) are a second population
// the flat dir scan cannot see: their agents fire no hooks and are ledgered in a
// per-run journal instead. They are folded into the same in-flight total and the
// same seen-sets, bracketed by workflow_start/stop.
//
// The Observer is called from BOTH the reconcile tick and the SubagentStart/Stop
// hook handler — single source of truth, two triggers — and its own mutex guards
// its maps, so it does not depend on the caller's locking.
//
// Its reads are split from its writes: Sample/Prime do the I/O with no store lock
// held, and ReconcileFrom applies the result with the lock held and no I/O under
// it. That split covers EVERY read the Observer makes — the history seed (both
// halves), the transcript cursor, the subagents dir scan, the workflow run-dir
// listing, each run's journal delta and the mtimes staleness is judged from. The
// tick uses the split; the hook trigger has no pre-lock phase, so it calls
// Reconcile, which reads inline. Both triggers reconcile the same durable
// per-session state, and a sample that state has moved past is rejected rather
// than applied (Sample.usableFor).
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
	gen        uint64          // bumped by every seed and every applied reconcile; see Sample.usableFor
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
	quietGrace time.Duration            // workflow quiet window (DefaultWorkflowQuietGrace)
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
// this read with the store lock in hand.
//
// The cost is that two callers can race the same cold session and both read. Only
// the FIRST result is installed — see the seeded check below, which is
// load-bearing rather than an optimization: the winner may already have advanced
// the cursor past what the loser measured, so installing the loser's read would
// rewind it.
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

	sd := seedFor(o.dir, sessionID, transcript)

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
	ss.applySeed(sd) // a failed read installs nothing; the backstop retries
}

// seed is one first-sight read's result.
//
// ok distinguishes the two outcomes that used to be conflated: the history read
// SUCCEEDED (possibly finding nothing, which is the normal case for a brand-new
// session and is a perfectly good seed), versus it FAILED and this says nothing
// about what was already emitted. Only the first may be installed.
type seed struct {
	spawned, stopped     map[string]bool // agent ids already spawned/stopped
	wfAnnounced, wfEnded map[string]bool // workflow run ids already started/stopped
	offset               int64
	ok                   bool
}

// seedFor is the first-sight read, factored out so the pre-lock Prime and the
// under-lock backstop in Reconcile cannot drift apart. It performs I/O and takes
// no locks.
//
// G1: seed the seen-set from already-emitted history so a restart or `--resume`
// does not re-emit historical spawns (metas are never deleted). The same guard
// covers workflow runs — their dirs are never deleted either, so a restart
// re-sights every historical run and must not re-announce it. The cursor is
// primed to EOF — the dir scan is the authoritative spawn source, so there is no
// need to re-read the whole transcript on every restart.
//
// EITHER archive read failing fails the whole seed, because both feed the same
// "what have I already emitted?" question and a half-seeded session would
// re-announce whichever population it could not read.
func seedFor(dir, sessionID, transcript string) seed {
	sp, st, err := history.PriorSubagentState(dir, sessionID)
	if err != nil {
		return seed{} // not seeded: see applySeed
	}
	ws, we, err := history.PriorWorkflowState(dir, sessionID)
	if err != nil {
		return seed{}
	}
	return seed{
		spawned: sp, stopped: st,
		wfAnnounced: ws, wfEnded: we,
		offset: fileSize(transcript), ok: true,
	}
}

// applySeed installs a seedFor result and reports whether the session is now
// seeded. The caller holds o.mu.
//
// A FAILED read installs nothing and leaves ss.seeded false, so the backstop in
// reconcile retries on the next tick. Marking it seeded anyway — which is what
// this did — hands the dir scan an EMPTY already-emitted set, and the scan then
// re-announces a spawn AND a stop for every historical subagent of the session:
// precisely the G1 double-count the seed exists to prevent. The read fails for
// ordinary reasons (EMFILE under load, a permission blip during a backup, a
// >1 MiB event line tripping the scanner's line limit), so this is not a
// theoretical path.
//
// A successful read that found nothing still counts as seeded, or a fresh session
// would retry forever.
func (ss *sessionState) applySeed(s seed) bool {
	if !s.ok {
		return false
	}
	if s.spawned != nil {
		ss.spawned = s.spawned
	}
	if s.stopped != nil {
		ss.stopped = s.stopped
	}
	if s.wfAnnounced != nil {
		ss.wfAnnounced = s.wfAnnounced
	}
	if s.wfEnded != nil {
		ss.wfEnded = s.wfEnded
	}
	ss.offset = s.offset
	ss.seeded = true
	ss.gen++
	return true
}

// Sample is the I/O half of one session's Reconcile, taken with no store lock
// held. It is a value, not a promise: it describes the transcript, the subagents
// dir and the session's workflow run dirs as they were at sample time, against
// the cursor positions recorded in base.
//
// The zero value is "no sample", which makes ReconcileFrom behave exactly like
// Reconcile.
type Sample struct {
	sessionID  string
	transcript string // the file the reads were taken from; a session id alone does not identify one
	base       int64  // the cursor the reads were taken against
	gen        uint64 // the session's state generation at sample time
	spawns     []transcript.Task
	resultIDs  []string
	newOffset  int64
	tasksOK    bool
	shrank     bool // the transcript was shorter than base at read time (G5)
	subs       []transcript.Subagent
	subsOK     bool
	runs       []workflowRunSample // one per wf_* run dir, in ReadDir (name) order
	runsOK     bool
	valid      bool
}

// workflowBase is the per-run cursor state a workflow read must be taken
// AGAINST, copied out of sessionState under o.mu so the read itself holds no
// lock. It is the workflow analogue of Sample.base.
type workflowBase struct {
	offsets map[string]int64           // run_id -> journal byte cursor
	open    map[string]map[string]bool // run_id -> agents started and not yet resulted or force-closed
}

// workflowBaseLocked copies the cursors a workflow read is taken against. The
// caller holds o.mu; the returned maps are copies, so the read that consumes
// them can run with no lock held.
//
// `open` is the set whose mtimes decide staleness. Sampling it here rather than
// re-deriving it at apply time is what lets the apply phase answer "is this
// agent stale?" from the sample: the generation guard means ss cannot have moved
// between the two, so (open ∪ the journal's new `started` ids) is exactly the
// set the apply loop will ask about.
func (ss *sessionState) workflowBaseLocked() workflowBase {
	b := workflowBase{
		offsets: make(map[string]int64, len(ss.workflows)),
		open:    make(map[string]map[string]bool, len(ss.workflows)),
	}
	for runID, wc := range ss.workflows {
		b.offsets[runID] = wc.offset
		open := map[string]bool{}
		for id := range wc.started {
			if wc.resulted[id] || wc.closed[id] {
				continue
			}
			open[id] = true
		}
		b.open[runID] = open
	}
	return b
}

// workflowRunSample is one workflow run's pre-read facts: the journal delta
// taken against the cursor in workflowBase, plus every mtime the apply phase
// needs to judge staleness (per agent) and quiescence (the journal, else the run
// dir). Stats live here rather than in the apply phase for the same reason the
// dir scan does — os.Stat is a filesystem round trip, and the apply phase runs
// under the store lock.
type workflowRunSample struct {
	run       transcript.WorkflowRun
	base      int64 // the journal cursor these reads were taken against
	started   []string
	resulted  []string
	newOffset int64
	journalOK bool // the journal read succeeded; a missing journal (just-launched run) is not OK but is not an error either

	journalMod  time.Time
	journalSeen bool // the journal could be stat-ed (it exists)
	dirMod      time.Time
	dirSeen     bool // only consulted when the journal is absent

	// agentMod is the activity heartbeat per agent that could still be in
	// flight: its own transcript's mtime, else the journal's. An id absent from
	// the map could not be stat-ed at all, which reads as "not stale" exactly as
	// it did inline.
	agentMod map[string]time.Time
}

// usableFor reports whether this sample still describes the session the caller is
// about to apply it to. A sample another producer has already overtaken is
// discarded rather than applied, because applying it would rewind the cursor to a
// stale newOffset and — worse — overwrite an in-flight count derived from a fresher
// dir scan with one derived from an older one. This is the same inversion hazard
// the resolve path hit when its enumeration moved outside the lock; here it is
// cheap to detect exactly, so it is detected rather than serialized against.
//
// The generation, not the cursor, is what makes this sound. The cursor guards only
// the TasksSince half of a sample: the dir scan has no cursor, and cannot get one
// that works — a subagent's terminal entry is appended to its OWN jsonl, and
// appending to an existing file does not move the containing dir's mtime, so a dir
// stamp is blind to the exact write that ends a fanout. What is detectable exactly,
// and for free, is that this session was reconciled between the sample and its
// application (the SubagentStart/Stop hook trigger reconciles the same session
// independently, and the window is wide — every other sampler runs in it). A
// generation bump on every seed and every applied reconcile catches that, and a
// mismatch costs one inline read, never correctness.
//
// The generation is also what covers the WORKFLOW cursors, which have no field of
// their own here: they change only inside reconcile, which bumps gen, so a
// matching gen means every per-run journal offset and open-agent set the sample
// was taken against is still the one being applied to.
//
// The transcript path is compared for a different reason: rs.samples is keyed by
// session id, but two store sessions can carry one session id with different
// transcripts (two panes resumed onto one conversation; a hook payload with a
// transcript_path but no session_id, which handleHook tolerates). Without this the
// loser of that collision has the other's dir scan applied to it.
func (s Sample) usableFor(sessionID, transcriptPath string, ss *sessionState) bool {
	return s.valid && ss != nil &&
		s.sessionID == sessionID && s.transcript == transcriptPath &&
		s.base == ss.offset && s.gen == ss.gen
}

// Sample performs every read one Reconcile needs, holding no lock while it does
// so, and seeds the session first if it has never been seen.
//
// This is the batched half of the Enumerate/ReconcileFrom split the resolve path
// already uses: the tick calls it for every session BEFORE taking the store
// lock, and ReconcileFrom then applies the result with the lock held but no I/O
// under it. The dir scans — the flat subagents/ one and the workflow run-dir
// one — ran per session per tick inside the lock.
func (o *Observer) Sample(sessionID, transcriptPath string) Sample {
	if o == nil || sessionID == "" || transcriptPath == "" {
		return Sample{}
	}
	o.Prime(sessionID, transcriptPath)

	o.mu.Lock()
	var base int64
	var gen uint64
	var wf workflowBase
	if ss := o.sessions[sessionID]; ss != nil {
		base, gen, wf = ss.offset, ss.gen, ss.workflowBaseLocked()
	}
	o.mu.Unlock()

	s := readSample(sessionID, transcriptPath, base, wf)
	s.gen = gen
	return s
}

// readSample is the actual I/O, shared by the pre-lock Sample and the under-lock
// fallback in reconcile so the two cannot drift. It takes no locks.
//
// G5: on /clear or compaction the file shrinks below the cursor — re-read from 0
// once (the agent-id seen-set keeps emission idempotent), and never let the
// cursor run past EOF.
//
// valid is set from the reads rather than ahead of them: a sample that failed to
// read carries no facts, and passing it off as usable would make ReconcileFrom skip
// the inline retry and then bail on !subsOK — giving up for the whole tick where
// plain Reconcile would have recovered within it.
func readSample(sessionID, transcriptPath string, base int64, wf workflowBase) Sample {
	s := Sample{sessionID: sessionID, transcript: transcriptPath, base: base}
	from := base
	if size := fileSize(transcriptPath); from > size {
		from, s.shrank = 0, true
	}
	if spawns, resultIDs, newOff, err := transcript.TasksSince(transcriptPath, from); err == nil {
		s.spawns, s.resultIDs, s.newOffset, s.tasksOK = spawns, resultIDs, newOff, true
	}
	if subs, err := transcript.SubagentsForTranscript(transcriptPath); err == nil {
		s.subs, s.subsOK = subs, true
	}
	// A session that never ran a workflow yields (nil, nil) — the common case, and
	// a perfectly good read. Only a genuine listing failure clears runsOK.
	if runs, err := transcript.WorkflowRunsForTranscript(transcriptPath); err == nil {
		s.runsOK = true
		for _, run := range runs {
			s.runs = append(s.runs, readWorkflowRun(run, wf))
		}
	}
	s.valid = s.tasksOK && s.subsOK && s.runsOK
	return s
}

// readWorkflowRun takes one run's journal delta and every mtime the apply phase
// will consult. It takes no locks and performs I/O.
//
// The agents it stats are the ones the apply loop can ask about: those already
// started and not yet resulted or force-closed (wf.open), plus those this very
// journal delta announced. An agent that resulted in the same delta is stat-ed
// too — one wasted stat, against the alternative of a set that has to be
// re-derived under the lock.
func readWorkflowRun(run transcript.WorkflowRun, wf workflowBase) workflowRunSample {
	w := workflowRunSample{run: run, base: wf.offsets[run.RunID]}

	started, resulted, newOff, err := transcript.WorkflowJournalSince(run.Journal, w.base)
	w.started, w.resulted, w.newOffset, w.journalOK = started, resulted, newOff, err == nil

	w.journalMod, w.journalSeen = statMtime(run.Journal)
	if !w.journalSeen {
		// No journal yet: the run dir's own stamp is the only freshness signal,
		// and it is only consulted in that case.
		w.dirMod, w.dirSeen = statMtime(run.Dir)
	}

	w.agentMod = make(map[string]time.Time, len(wf.open[run.RunID])+len(started))
	for id := range wf.open[run.RunID] {
		if mt, ok := workflowAgentMtime(run, id); ok {
			w.agentMod[id] = mt
		}
	}
	for _, id := range started {
		if _, done := w.agentMod[id]; done || id == "" {
			continue
		}
		if mt, ok := workflowAgentMtime(run, id); ok {
			w.agentMod[id] = mt
		}
	}
	return w
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
// sample this session has moved past — a different cursor, a different transcript,
// a reconcile applied in between, or reads that failed — is rejected and re-read
// inline, so a rejected sample costs latency, never correctness.
//
// An ACCEPTED sample is still a sample: it describes the transcript, subagents
// dir and workflow run dirs as of sample time, so a change on disk in the window
// between is folded in one tick later than plain Reconcile would have. That lag is
// inherent to sampling and bounded by the tick; what is NOT tolerable, and what
// usableFor exists to prevent, is a sample overwriting state a fresher read
// already established.
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
	if !ss.seeded && !ss.applySeed(seedFor(o.dir, c.SessionID, c.Transcript)) {
		// The lazy backstop. Prime (called before the store lock is taken) normally
		// gets here first, so this fires only for a session that appeared between
		// the caller's snapshot and the lock, or on the hook trigger, which has no
		// pre-lock phase to hang a Prime on. Correctness does not depend on which
		// path seeds — only cost does, and this one pays it under the lock.
		//
		// Reaching here means the read FAILED, and this tick must emit nothing: the
		// dir scan below is authoritative about what EXISTS but knows nothing about
		// what was already recorded, so running it against an empty seen-set would
		// re-announce every historical subagent of this session. Retrying next tick
		// costs one tick of delegation lag; guessing costs a corrupted timeline.
		return nil
	}

	if !s.usableFor(c.SessionID, c.Transcript, ss) {
		// No sample, or one this session has moved past. Read inline — under the lock,
		// as this always used to be. Correctness never depends on the sample; only
		// the lock hold does.
		s = readSample(c.SessionID, c.Transcript, ss.offset, ss.workflowBaseLocked())
	}
	// Everything below mutates ss, which retires every sample taken against it —
	// including the one another producer may be holding right now, mid-tick. o.mu is
	// held for the whole body, so where the bump sits inside it does not matter.
	ss.gen++

	// G5, and it must be durable: on /clear or compaction the transcript shrinks
	// below the cursor, and the reset is written to ss BEFORE the read outcome is
	// considered — exactly as it was when this ran inline. Folding the reset into
	// the read (using it only when the read succeeded) loses it precisely when the
	// file is being replaced, which is when a truncation and a failed read are most
	// likely to coincide: the cursor would keep a value past EOF, and if the file
	// then regrew past it, every entry in between would be skipped forever.
	if s.shrank {
		ss.offset = 0
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
	// 3) Workflow runs (subagents/workflows/wf_*/): the fan-outs the flat scan
	// above cannot see. Their agents are spawnDepth-1 children, so they count
	// toward the same in-flight total — an idle main thread with a workflow
	// running reads delegating (green) exactly like a hand-launched fanout.
	events = append(events, o.applyWorkflowsLocked(sess, c, ss, now, &inflight, s)...)
	c.InFlightSubagents = inflight
	return events
}

// applyWorkflowsLocked brings the per-run workflow cursors up to date FROM THE
// SAMPLE, adds each active run's in-flight agents to *inflight, refreshes
// c.Workflows (the wire summary renderers annotate the green chip with), and
// returns the workflow_start/stop and per-agent subagent_spawn/stop events to
// record. Caller holds o.mu. It performs no I/O: every listing, journal delta
// and mtime it consults was read by readWorkflowRun before the lock.
//
// Per-agent completion is the run's JOURNAL (`result` event), never the agent
// transcript's last line: a workflow agent that returns via structured output
// ends its jsonl with a user tool_result, not an assistant end_turn, so the
// flat-dir Done detection would hold it in flight forever. A killed run's
// orphans (journal `started` with no `result`, transcript gone quiet) are
// force-closed by the same staleCap as stalled flat-dir subagents.
func (o *Observer) applyWorkflowsLocked(sess *state.Session, c *state.AgentInfo, ss *sessionState, now time.Time, inflight *int, s Sample) []history.Event {
	if !s.runsOK {
		return nil // leave the last-known Workflows rather than guess
	}
	var events []history.Event
	var statuses []state.WorkflowStatus
	for _, rs := range s.runs {
		run := rs.run
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
		if rs.journalOK {
			wc.offset = rs.newOffset
		}
		for _, id := range rs.started {
			wc.started[id] = true
		}
		for _, id := range rs.resulted {
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
			if mt, ok := rs.agentMod[id]; ok && now.Sub(mt) > o.staleCap {
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
			if rs.journalSeen && now.Sub(rs.journalMod) <= o.quietGrace {
				active = true
			} else if !rs.journalSeen {
				// No journal yet: a run dir that JUST appeared is a launching
				// workflow; one long dead (daemon-start backfill) is not.
				if rs.dirSeen && now.Sub(rs.dirMod) <= o.quietGrace {
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
		for _, id := range rs.started {
			if id == "" || ss.spawned[id] {
				continue
			}
			ss.spawned[id] = true
			sub := transcript.Subagent{AgentID: id, AgentType: transcript.WorkflowAgentType, SpawnDepth: 1}
			if mt, ok := rs.agentMod[id]; ok {
				sub.ModTime = mt
			}
			ev := o.spawnEvent(sess, c, sub, now, false)
			ev.WorkflowRunID = run.RunID
			events = append(events, ev)
		}
		for _, id := range rs.resulted {
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
	// statuses follows s.runs, which ReadDir yielded in name order — already the
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
// appeared can only be as fresh as the journal that announced it). It stats, so
// it belongs to the sample phase.
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
