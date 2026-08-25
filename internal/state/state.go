// Package state owns the in-memory session map and the on-disk state.json
// mirror. All mutations go through Store.Apply, which calls subscribers and
// schedules an atomic write.
package state

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type Session struct {
	PID int `json:"pid"`
	// Hostname is populated only on detached copies in the federated client
	// view. The host-local Store deliberately leaves it empty: local discovery,
	// liveness, persistence, and navigation remain keyed exactly as before.
	// Together with PID it namespaces a live aggregate row; StartedAt is the
	// daemon's discovery-lifetime fence for actions and bindings. It rejects
	// stale observations while daemon continuity is intact, but is not a kernel
	// process-birth token. Omitted from ordinary local snapshots, so the frozen
	// host-local state.json shape is unchanged.
	Hostname  string    `json:"hostname,omitempty"`
	CWD       string    `json:"cwd"`
	TTY       string    `json:"tty"`
	StartedAt time.Time `json:"started_at"`
	Focused   bool      `json:"focused"`
	// Remote is populated only on detached aggregate copies. Renderers use it
	// to avoid treating remote CWD, transcript, and PID values as paths or
	// process identities on this machine. Like Hostname and Navigable, it is
	// omitted from ordinary host-local snapshots and durable state.
	Remote bool `json:"remote,omitempty"`
	// Suspended is true when the agent process is job-control-stopped (Ctrl-Z /
	// SIGSTOP). Renderers grey such chips out. Omitted when false so the common
	// case stays off the wire.
	Suspended bool `json:"suspended,omitempty"`
	// Headless marks a non-interactive `claude -p`/SDK run (see
	// discovery.IsHeadless). It appears in bars for visibility but has no TUI
	// to navigate to, so renderers style it inert and focus/cycle/pick skip
	// it. Omitted when false.
	Headless bool `json:"headless,omitempty"`
	// Navigable is populated only on detached aggregate copies. It says an
	// exact local route candidate exists now; every action still revalidates the
	// pane, window, liveness, and StartedAt before acting. Host-local snapshots
	// leave it false/omitted, preserving the durable schema.
	Navigable bool `json:"navigable,omitempty"`

	// Agent names the coding-agent CLI that owns this session: "claude" or
	// "codex" (the AgentKind* constants). Set at discovery from the process. It
	// selects which enrichment block (claude/codex) hooks write and how a
	// renderer reads status. Omitted only when the kind is not yet known.
	Agent string `json:"agent,omitempty"`
	// DisplayName is Switchboard-owned Codex display metadata. It is valid only
	// for its exact conversation_id and never changes Codex's native thread name.
	DisplayName *DisplayName `json:"display_name,omitempty"`

	// MemAgentBytes is the live resident cost of the agent process alone:
	// Pss + SwapPss from /proc/<pid>/smaps_rollup, refreshed each reconcile tick.
	// PSS charges shared pages to their sharers in fractions, so summing across
	// sessions never double-counts. Omitted rather than zero when a reading
	// failed or the backend cannot take one — 0 would mean "measured, and empty".
	MemAgentBytes int64 `json:"mem_agent_bytes,omitempty"`
	// MemTreeBytes is the same measure summed over the agent process and every
	// descendant — the session's whole process tree, the only unit that captures
	// subagent work (subagents have no PIDs of their own). Subtract
	// MemAgentBytes for what the children cost. Same absence semantics.
	MemTreeBytes int64 `json:"mem_tree_bytes,omitempty"`

	Wezterm  *WeztermInfo  `json:"wezterm,omitempty"`
	Hyprland *HyprlandInfo `json:"hyprland,omitempty"`
	// Claude and Codex are the per-agent enrichment blocks; they share one shape
	// (AgentInfo). Exactly one is populated, matching Agent — the other is
	// omitted. The split keeps the frozen "claude" wire key intact for existing
	// bar consumers while adding "codex" purely additively.
	Claude *AgentInfo `json:"claude,omitempty"`
	Codex  *AgentInfo `json:"codex,omitempty"`

	// AgentGraph is the additive provider-neutral view of the root thread and
	// its descendants. Child nodes are display/history detail only; they are not
	// independently switchable Sessions.
	AgentGraph *AgentGraph `json:"agent_graph,omitempty"`
}

// Agent kind identifiers, stored in Session.Agent. They match the string values
// of discovery.Agent, which is where a session's kind originates.
const (
	AgentKindClaude = "claude"
	AgentKindCodex  = "codex"
)

// Status values stored in AgentInfo.Status. The first three are hook-driven and
// frozen wire values; StatusDelegating is daemon-derived: an idle main thread
// with subagents still in flight (it renders GREEN — work is happening, no
// action needed — see docs/status-color-state-model.md cases 5/14). Renderers
// that do not special-case it must treat it as working (green), never as
// attention-worthy. "unknown" is never stored; renderers synthesize it from an
// empty status.
const (
	StatusWorking    = "working"
	StatusIdle       = "idle"
	StatusPermission = "permission"
	StatusDelegating = "delegating"
)

// Enrichment returns the populated per-agent block for this session (selected by
// Agent), or nil when no hook has fired yet. Renderers call it to read status
// without knowing which agent produced it.
func (s Session) Enrichment() *AgentInfo {
	switch s.Agent {
	case AgentKindCodex:
		return s.Codex
	case AgentKindClaude:
		return s.Claude
	default:
		if s.Claude != nil {
			return s.Claude
		}
		return s.Codex
	}
}

// AgentBlock returns the enrichment block for the given agent kind, allocating
// it (and recording the kind on the session) when absent. Hook handling routes
// through it so one code path serves every agent.
func (s *Session) AgentBlock(kind string) *AgentInfo {
	if s.Agent == "" {
		s.Agent = kind
	}
	if kind == AgentKindCodex {
		if s.Codex == nil {
			s.Codex = &AgentInfo{}
		}
		return s.Codex
	}
	if s.Claude == nil {
		s.Claude = &AgentInfo{}
	}
	return s.Claude
}

type WeztermInfo struct {
	MuxPID      int    `json:"mux_pid"`
	MuxSocket   string `json:"mux_socket"`
	PaneID      int    `json:"pane_id"`
	TabID       int    `json:"tab_id"`
	WindowID    int    `json:"window_id"`
	WindowTitle string `json:"window_title"`
	// Title is the pane's OWN title — the string the agent CLI paints there
	// (Claude Code animates a spinner glyph while a turn runs and parks the
	// static idle glyph while waiting at the prompt). Distinct from WindowTitle,
	// which follows the window's active pane and could cross-contaminate between
	// split panes. Kept off the wire (json:"-"): it is a live in-process signal
	// for the reconciler's idle-title recovery (docs/timing-hazards.md H9), not
	// part of the frozen state.json contract — and it deliberately does not
	// survive a daemon restart, because the recovery may only trust a title
	// sampled after the chip's transition (TitleAt), which a rehydrated zero
	// value guarantees.
	Title string `json:"-"`
	// TitleAt is when Title was last sampled from the terminal (the resolver
	// re-locates every session each reconcile tick). The freshness gate for H9.
	TitleAt time.Time `json:"-"`
}

type HyprlandInfo struct {
	Address     string `json:"address"`
	Workspace   string `json:"workspace"`
	WorkspaceID int    `json:"workspace_id"`
	Monitor     string `json:"monitor"`
}

// AgentInfo is the per-session enrichment a coding agent's hooks feed in. The
// shape is identical for every agent (Claude Code, Codex); Session.Agent and the
// wire key it sits under ("claude"/"codex") say which agent produced it.
type AgentInfo struct {
	SessionID string `json:"session_id,omitempty"`
	// Transcript is provider-specific legacy enrichment retained for existing
	// Claude/Codex consumers. The neutral graph never exposes transcript paths.
	Transcript string `json:"transcript,omitempty"`
	Status     string `json:"status"` // working|idle|permission|delegating (never "unknown")

	// StatusSinceWire is the wire projection of StatusSince: the instant the
	// current status began, so a renderer can show "idle 3m" / "waiting 45s" in
	// the tooltip without the daemon pre-formatting a duration. It is DERIVED —
	// stamped from StatusSince onto a per-snapshot copy of the block in
	// snapshotLocked — never written by hook/reconciler logic, which keep using
	// the in-memory StatusSince below. A pointer so it omits cleanly before the
	// first status edge and so encoding/json formats it exactly like started_at.
	StatusSinceWire *time.Time `json:"status_since,omitempty"`

	// InFlightSubagents is a Claude-specific legacy compatibility projection: how
	// many subagent Tasks the main thread has launched
	// but not yet collected (transcript.InFlightTasks), recomputed each reconcile
	// tick. It is the S dimension: >0 with an idle main thread is the delegating
	// (green) case. Exposed on the wire (omitempty, so absent when 0 — the golden
	// contract is unchanged) so renderers can show "N agents" in the tooltip and
	// `switchboard-ctl list` reveals the true state behind a green chip.
	// Subagents spawned by an ultracode Workflow run count here too (they are
	// spawnDepth-1 children, listed per-run in Workflows below).
	InFlightSubagents int `json:"in_flight_subagents,omitempty"`

	// Workflows is a Claude-specific legacy compatibility projection listing the
	// ultracode Workflow runs currently ACTIVE in this
	// session — fan-outs the Workflow tool orchestrates, whose subagents live
	// under <session-dir>/subagents/workflows/wf_*/ and fire no hooks. Derived
	// each reconcile tick by the fanout Observer from those on-disk records
	// (journal + agent transcript mtimes) and cleared when the last run drains,
	// so a renderer can spell out WHY a chip is green ("workflow
	// simplification-audit · 7/17 agents") rather than showing a bare
	// delegating. Sorted by RunID — snapshotChangeKey JSON-encodes every tagged
	// field to decide whether to publish, so an unstable order would republish
	// identical state every tick.
	Workflows []WorkflowStatus `json:"workflows,omitempty"`

	// StatusSince marks when Status last transitioned to its current value. The
	// reconciler uses it to age out a "permission" chip that Claude Code left
	// latched (a declined question / interrupt fires no clearing hook). Kept
	// in-memory (json:"-") as the source of truth for the duration math; it is
	// projected to the wire as StatusSinceWire (status_since) at snapshot time, so
	// the in-memory value's zero-reads-as-"long ago" reconcile semantics are
	// unchanged (a re-hydrated session is re-evaluated against its transcript on
	// the first reconcile; dropStaleSessions re-stamps it to startup time).
	StatusSince time.Time `json:"-"`

	// Pending is Claude's legacy per-writer compatibility state. It maps each
	// WRITER currently blocked on a permission prompt to that
	// prompt's correlators. A session is 1 + N concurrent writers — the main thread
	// plus every in-flight subagent — that share a pid, a chip and a transcript_path
	// but write to different files and can each block independently
	// (docs/subagent-permission-plan.md §1). The scalar this replaced could hold
	// exactly one prompt, which is why a teammate's tool could clear a prompt it had
	// nothing to do with.
	//
	// The key is the NORMALIZED bare agent_id — empty means the MAIN THREAD. Keys
	// arrive already normalized: rpc.handleHook runs every incoming agent_id through
	// normalizeAgentID exactly once, at its entry. Nothing here (or downstream) may
	// strip an "agent-" prefix a second time; see normalizeAgentID for why.
	//
	// The fold is `len(Pending) > 0 → RED`, ahead of every other rule: the chip may
	// leave "permission" only when no writer still owns a prompt.
	//
	// In-memory only. Its KEY SET — and only its key set — is projected onto the wire
	// as PendingWriters so prompt ownership survives a daemon restart (§9); the
	// correlators below are re-earned from the next hook.
	Pending map[string]PendingPrompt `json:"-"`

	// PendingWriters is the wire projection of Pending's KEY SET: sorted ascending,
	// with the literal "main" standing in for the empty (main-thread) key. It is
	// DERIVED — stamped onto a per-snapshot copy of the block in enrichForWire,
	// exactly as StatusSinceWire is — and must never be written by hook or
	// reconciler logic, which keep using the in-memory Pending map above. The one
	// exception is Load, the inverse codec, which rebuilds Pending from it at
	// hydrate before any snapshot is taken.
	//
	// Why the keys and not the whole prompt: losing OWNERSHIP is unrecoverable.
	// PermissionRequest is edge-triggered, no hook re-raises a live prompt, and a
	// blocked writer runs no tools — so a dropped entry is a permanent missed RED
	// for the rest of that prompt's life. Losing Tool/InputHash costs one reconcile
	// tick of latency. Persist what guards the worse error; re-earn the rest (§9.5).
	//
	// The sort is load-bearing, not cosmetic: snapshotChangeKey JSON-encodes every
	// tagged field to decide whether to publish, so an unsorted slice built by
	// ranging a map would differ between snapshots of identical state and republish
	// to every waybar slot on every reconcile tick.
	PendingWriters []string `json:"pending_writers,omitempty"`

	// PendingTool is the tool_name of the prompt the chip's red is reported under:
	// the MAIN thread's if it has one, else the lowest-keyed writer's. It is DERIVED
	// from Pending (see derivePendingTool) and re-stamped by every mutation of it —
	// never assign it directly.
	//
	// It survives the map because two consumers still want a scalar: the hold gate's
	// tool-name fast path (docs/status-color-state-model.md A2/case 12 — whose
	// matching rule plan T7 owns) and the `pending=` field of the decision log, the
	// forensic backbone this whole investigation was reconstructed from. See
	// PendingSummary for the log's fuller rendering.
	//
	// In-memory only: transient onset state, not part of the wire contract.
	PendingTool string `json:"-"`
}

// WorkflowStatus summarizes one active ultracode Workflow run for the wire —
// the numbers behind a "workflow <name> · done/total agents" annotation. The
// counts come from the run's journal (the authoritative per-agent ledger):
// AgentsStarted/AgentsDone are agents launched/resulted SO FAR — the journal
// records no plan, so "total" here grows as the script fans out, exactly like
// the CLI's own "7/17 agents done" line. InFlight is started minus resulted
// minus any agent the Observer force-closed as stale, so a killed run's
// orphaned agents age out of the count rather than pinning it forever.
type WorkflowStatus struct {
	RunID         string `json:"run_id"`         // the run dir's basename, e.g. "wf_5e3cb808-2ac"
	Name          string `json:"name,omitempty"` // workflow name from the persisted script; "" when unresolvable
	AgentsStarted int    `json:"agents_started"` // journal `started` events seen so far
	AgentsDone    int    `json:"agents_done"`    // journal `result` events seen so far
	InFlight      int    `json:"in_flight"`      // started − resulted − force-closed
}

// PendingWriterMain is the wire spelling of the empty (main-thread) Pending key.
// The empty string is a load-bearing discriminator in memory but a poor value on a
// public contract, so the projection substitutes this literal. It cannot collide
// with a real writer: subagent ids are the <id> stem of an agent-<id>.jsonl file,
// and no such stem is "main" (a subagent named "main" is stored as agent-main<hex>).
const PendingWriterMain = "main"

// PendingPrompt is one writer's outstanding permission prompt: which tool it was
// raised for, which call (a hash of tool_input, computed at the ctl edge — the raw
// input is never forwarded or stored), and when it appeared.
//
// Tool and InputHash are the correlators the hold gate's fast path matches a later
// PostToolUse against; Since dates the prompt for the per-prompt liveness backstop
// (plan T10). All three are in-memory only — a hydrated prompt carries none of them
// and must resolve by transcript instead (§9.6, trap 2).
type PendingPrompt struct {
	Tool      string
	InputHash string
	Since     time.Time
}

// SetPending records that writer agentID (bare; "" is the main thread) is blocked
// on prompt p, allocating the map on first use and re-deriving PendingTool.
func (a *AgentInfo) SetPending(agentID string, p PendingPrompt) {
	if a.Pending == nil {
		a.Pending = make(map[string]PendingPrompt, 1)
	}
	a.Pending[agentID] = p
	a.derivePendingTool()
}

// DropPending removes one writer's prompt — the resolution primitive: `P[a]` is
// removed only by evidence from writer `a` (plan §3.3). Re-derives PendingTool.
func (a *AgentInfo) DropPending(agentID string) {
	delete(a.Pending, agentID)
	a.derivePendingTool()
}

// ClearPending forgets every pending prompt. Used where the whole red is being
// abandoned rather than resolved per writer: a session rotation (a /clear or fork
// retires the prompts with the session that raised them) and the chip's exit from
// "permission".
func (a *AgentInfo) ClearPending() {
	a.Pending = nil
	a.PendingTool = ""
}

// PendingWriterKeys returns Pending's keys in a stable ascending order, in their
// in-memory (bare, "" = main) spelling. Callers that iterate Pending must use this
// rather than ranging the map: Go randomizes map iteration, and the daemon's
// outputs — the wire projection, the decision log, the hydrate verdicts — must be
// reproducible across ticks.
func (a *AgentInfo) PendingWriterKeys() []string {
	if len(a.Pending) == 0 {
		return nil
	}
	keys := make([]string, 0, len(a.Pending))
	for k := range a.Pending {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// PendingSummary renders Pending for the decision log's `pending=` field: the
// reported tool (PendingTool — the main thread's prompt if it has one, else the
// lowest-keyed writer's) suffixed with "+N" when N further writers are also
// blocked. One prompt therefore logs exactly what it always logged, so
// statustune.ParseDecision and `switchboard-ctl diagnose` keep reading it, while a
// multi-writer red no longer silently reports as a single one.
//
// An empty map falls through to PendingTool, which is "" for a live block and may
// be a hand-seeded or hydrated value otherwise.
func (a *AgentInfo) PendingSummary() string {
	if n := len(a.Pending); n > 1 {
		return fmt.Sprintf("%s+%d", a.PendingTool, n-1)
	}
	return a.PendingTool
}

// derivePendingTool re-stamps the scalar PendingTool from the map: the main
// thread's prompt wins when it has one (it is the writer the user is most likely
// looking at), else the lowest-keyed writer's — a deterministic choice, because
// "any one" out of a Go map is a different one every tick.
func (a *AgentInfo) derivePendingTool() {
	if len(a.Pending) == 0 {
		a.PendingTool = ""
		return
	}
	if p, ok := a.Pending[""]; ok {
		a.PendingTool = p.Tool
		return
	}
	a.PendingTool = a.Pending[a.PendingWriterKeys()[0]].Tool
}

// pendingWritersForWire projects a Pending map onto its sorted wire key set,
// substituting PendingWriterMain for the empty key. nil in / empty in yields nil,
// so the field omits rather than emitting an empty array.
//
// The sort runs on the TRANSLATED names, so the wire document is sorted as a
// reader sees it.
func pendingWritersForWire(pending map[string]PendingPrompt) []string {
	if len(pending) == 0 {
		return nil
	}
	names := make([]string, 0, len(pending))
	for k := range pending {
		if k == "" {
			k = PendingWriterMain
		}
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// pendingFromWire is the inverse: it rebuilds a Pending map holding ownership and
// nothing else — every PendingPrompt is zero, because Tool/InputHash/Since are
// deliberately not persisted (§9.5). PendingWriterMain maps back to the empty key.
//
// It manufactures nothing: an absent/empty field yields a nil map, and the caller
// (dropStaleSessions) decides what an empty set beside a persisted red means.
func pendingFromWire(names []string) map[string]PendingPrompt {
	if len(names) == 0 {
		return nil
	}
	pending := make(map[string]PendingPrompt, len(names))
	for _, n := range names {
		if n == PendingWriterMain {
			n = ""
		}
		pending[n] = PendingPrompt{}
	}
	return pending
}

// ClaudeInfo is the original name for AgentInfo, kept as an alias so existing
// callers and tests compile unchanged.
type ClaudeInfo = AgentInfo

type Snapshot struct {
	SchemaVersion int           `json:"schema_version,omitempty"`
	Sessions      []Session     `json:"sessions"`
	UpdatedAt     time.Time     `json:"updated_at"`
	Capabilities  *Capabilities `json:"capabilities,omitempty"`
}

// Capabilities reports the detected backend stack and which tier is active, so
// a renderer can decide whether to show "jump to" affordances. Observe is the
// always-available floor; Navigate is true only when both a terminal locator
// and a WM focus backend are present. Omitted entirely (never null) when the
// daemon has not set it; consumers tolerate its absence.
type Capabilities struct {
	Observe  bool   `json:"observe"`
	Navigate bool   `json:"navigate"`
	WM       string `json:"wm"`
	Terminal string `json:"terminal"`
}

// Broadcast is one fan-out unit: a snapshot plus a shared JSON body for consumers
// that can forward that exact generation. Federation-facing subscriptions use
// these values only as wakeups and re-read Store.Snapshot, because a value queued
// before their independent initial read may be older than it.
type Broadcast struct {
	Snapshot Snapshot
	// JSON is the COMPACT encoding of Snapshot: exactly what json.Encoder writes,
	// minus the trailing newline. It is paired with this particular broadcast;
	// consumers which also take an independent initial Snapshot must treat the
	// channel as a notification and re-read current state, because a queued value
	// can predate that initial read. The RPC subscription does exactly that.
	//
	// It is nil when the encode failed. A subscriber must then encode Snapshot
	// itself rather than send a truncated frame.
	//
	// Treat it as immutable: every subscriber holds this same backing array.
	JSON []byte
}

type Store struct {
	path        string
	mu          sync.RWMutex
	sessions    map[int]*Session
	subscribers map[chan Broadcast]struct{}
	caps        *Capabilities
	// publishedKey is snapshotChangeKey of the last snapshot Apply decided to
	// publish — the reference the change check compares against. nil before the
	// first publish (and after a failed encode or a failed persist), which compares
	// unequal to everything, so the next Apply publishes.
	publishedKey []byte
	// publishedGen counts adoptions of publishedKey. Apply captures it at adopt
	// time so that a persist failing AFTER the unlock can retract its own adoption
	// without clobbering one a later Apply made in the meantime. See
	// invalidatePublished.
	publishedGen uint64
	// broadcastGen serializes post-unlock fanout by the Apply generation. Two
	// concurrent Apply calls may reach broadcast out of order; an older one must
	// never overwrite the newer full replacement in subscriber queues.
	broadcastMu  sync.Mutex
	broadcastGen uint64
}

func New(statePath string) *Store {
	return &Store{
		path:        statePath,
		sessions:    make(map[int]*Session),
		subscribers: make(map[chan Broadcast]struct{}),
	}
}

// SetCapabilities records the detected backend stack. It is included in every
// subsequent snapshot. Set once at daemon startup, before serving.
func (s *Store) SetCapabilities(c Capabilities) {
	s.mu.Lock()
	s.caps = &c
	s.mu.Unlock()
}

// lockHoldWarn is the Apply hold duration above which the daemon logs a line.
// Zero — the default — disables the check entirely, costing one comparison per
// Apply. Enable with SWITCHBOARD_DEBUG_LOCK set to a Go duration ("5ms").
//
// It exists because "is the store lock still what is stalling the bar?" is the
// only question that matters when a chip click feels slow, and it cannot be
// answered from outside the process: an RPC probe measures lock wait plus
// round-trip plus scheduling, and cannot say which. This measures the hold
// itself. On a healthy daemon the answer is silence.
//
// Read once at init rather than per call so the hot path never touches the
// environment.
var lockHoldWarn = func() time.Duration {
	d, err := time.ParseDuration(os.Getenv("SWITCHBOARD_DEBUG_LOCK"))
	if err != nil || d <= 0 {
		return 0
	}
	return d
}()

// Apply mutates the store under lock, then — only when the mutation actually
// changed something a consumer can see — notifies subscribers and persists.
//
// The change check exists because Apply's callers are mostly unconditional: the
// reconciler runs every 5 s and calls Apply whether or not the world moved, so a
// machine sitting idle overnight was waking ten waybar processes and rewriting
// state.json on every tick, forever, to republish byte-identical state.
func (s *Store) Apply(fn func(map[int]*Session)) {
	s.mu.Lock()
	var heldFrom time.Time
	if lockHoldWarn > 0 {
		heldFrom = time.Now()
	}
	fn(s.sessions)
	snap := s.snapshotLocked()
	gen, changed := s.adoptPublishedLocked(snap)
	if lockHoldWarn > 0 {
		if held := time.Since(heldFrom); held > lockHoldWarn {
			// Deliberately still under the lock: this reports the hold, and a hold
			// long enough to trip the threshold has already done its damage. The
			// write is one Fprintf on a path that by definition fires rarely.
			fmt.Fprintf(os.Stderr, "state: Apply held the write lock %v (> %v) sessions=%d\n",
				held.Round(time.Microsecond), lockHoldWarn, len(s.sessions))
		}
	}
	s.mu.Unlock()

	if !changed {
		return
	}
	s.broadcast(snap, gen)
	if err := s.persist(snap); err != nil {
		fmt.Fprintf(os.Stderr, "state: persist failed: %v\n", err)
		// The reference was adopted before the write was attempted, so leaving it
		// adopted would suppress every later Apply that produces this same state —
		// freezing state.json at its last good content indefinitely, until something
		// unrelated happens to change. Before the publish gate existed every Apply
		// rewrote the file, so a transient failure (ENOSPC, a momentarily unwritable
		// cache dir) healed on the very next tick. Retracting the adoption restores
		// exactly that: the next Apply republishes even if nothing moved.
		s.invalidatePublished(gen)
	}
}

// adoptPublishedLocked compares snap against the last snapshot Apply decided to
// publish and, when they differ, adopts snap as the new reference. It reports the
// generation of the adoption (for invalidatePublished) and whether anything
// changed.
//
// It deliberately runs under the SAME write lock as the mutation that produced
// snap. That is what makes suppression safe: the reference advances in mutation
// order, so a change can never be compared against a reference stamped by a
// LATER mutation and dropped as a no-op. Broadcast generation ordering is
// serialized separately after the unlock; persistence may still overlap, but it
// is not a live subscription source. The cost is one encode inside the write
// lock, which is microseconds against the milliseconds of terminal/WM I/O the
// reconciler used to hold it for.
func (s *Store) adoptPublishedLocked(snap Snapshot) (gen uint64, changed bool) {
	key := snapshotChangeKey(snap)
	if key != nil && bytes.Equal(key, s.publishedKey) {
		return s.publishedGen, false
	}
	s.publishedKey = key
	s.publishedGen++
	return s.publishedGen, true
}

// invalidatePublished retracts the adoption made at generation gen, so the next
// Apply republishes even when the state has not moved. Apply calls it when the
// persist that followed the adoption failed.
//
// The generation check is the whole point: Apply is past its unlock by the time a
// persist can fail, so another Apply may already have adopted a NEWER reference.
// Clearing unconditionally would discard that newer reference. The cost of doing
// so would only be one redundant publish, not a correctness bug — but the newer
// Apply is also the one that knows whether ITS persist succeeded, so it is the
// only one entitled to decide. Retracting only our own adoption leaves that
// decision where it belongs: if the newer persist also failed, it retracts its
// own generation; if it succeeded, state.json already holds strictly fresher
// state than ours and there is nothing to heal.
func (s *Store) invalidatePublished(gen uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.publishedGen != gen {
		return // a later Apply owns the reference now; it will heal its own failure
	}
	s.publishedKey = nil
}

// snapshotChangeKey encodes everything about a snapshot that a consumer can
// observe, and nothing else. Two snapshots with equal keys are indistinguishable
// both on the subscribe stream and in state.json, so publishing the second is
// pure noise.
//
// It is a JSON encode rather than a hand-written field-by-field comparison on
// purpose. A comparator's failure mode is silent: add a field to Session, forget
// to compare it, and bars simply stop updating for that field with no test
// failing anywhere. Encoding inherits the wire contract instead — every field
// carrying a JSON tag is compared by construction, and every in-memory-only
// field (json:"-") is excluded by construction, now and for anything added later.
//
// UpdatedAt is the single field dropped by hand, and it is the whole reason a
// naive equality check would never fire: snapshotLocked stamps it time.Now() on
// EVERY snapshot, so two identical states differ there by definition and the
// suppression would silently never trigger. The other fields re-stamped from the
// wall clock rather than earned by a real change fall out for free, because they
// are already json:"-" and so never reach an encode:
//
//   - WeztermInfo.TitleAt — the resolver re-samples the pane title every reconcile
//     tick and stamps it (mapping.weztermInfo), so it advances on a quiet machine.
//   - WeztermInfo.Title with it: the agent CLI repaints that string continuously
//     while a turn runs (animated spinner glyph).
//   - AgentInfo.PendingTool — transient red-onset state, not a clock but equally
//     invisible to consumers. AgentInfo.Pending's correlator VALUES
//     (Tool/InputHash/Since) fall out for the same reason.
//
// AgentInfo.StatusSince is json:"-" too, but it is NOT a hidden field for this
// purpose: snapshotLocked projects it onto StatusSinceWire (status_since), which
// IS encoded and IS compared. AgentInfo.Pending's KEY SET is the same case: it is
// projected onto PendingWriters (pending_writers) and so IS compared — which is
// why that projection must be SORTED. A slice built by ranging the map would
// differ between snapshots of identical state, and the gate would republish to
// every waybar slot and rewrite state.json on every reconcile tick, reintroducing
// exactly the wake-storm this check exists to suppress. That is correct rather than a leak — audited against
// docs/state-schema.md ("when status last transitioned to its current value") and
// against every writer: rpc.handleHook stamps it only inside its
// `status != info.Status` guard, and each of the reconciler's self-heals stamps it
// on the same line it assigns a new Status. It moves on a status edge and nowhere
// else, so a moved status_since is a real change that must reach the bar.
//
// ⚠ Not excluded, and worth knowing: mem_agent_bytes / mem_tree_bytes are
// re-sampled every tick and genuinely jitter, so a session whose PSS drifts
// publishes even when nothing else moved. They are real wire data driving the
// tooltip, so suppressing them would be a lie — the suppression simply fires less
// often on a machine whose agents are still breathing.
func snapshotChangeKey(snap Snapshot) []byte {
	key, err := json.Marshal(struct {
		SchemaVersion int           `json:"schema_version"`
		Sessions      []Session     `json:"sessions"`
		Capabilities  *Capabilities `json:"capabilities,omitempty"`
	}{SchemaVersion: snap.SchemaVersion, Sessions: snap.Sessions, Capabilities: snap.Capabilities})
	if err != nil {
		// Not reachable today (Snapshot holds no unencodable field), but fail OPEN:
		// a nil key compares unequal to everything, so a broken encode republishes
		// rather than silently freezing every bar on the last good state.
		fmt.Fprintf(os.Stderr, "state: change key encode failed: %v\n", err)
		return nil
	}
	return key
}

// Snapshot returns a deep-ish copy of current state. Values are copied; the
// pointer fields (Wezterm/Hyprland/Claude) are shared — fine for read-only
// consumers.
func (s *Store) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshotLocked()
}

func (s *Store) snapshotLocked() Snapshot {
	sessions := make([]Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		cp := *sess
		// Deep-copy the enrichment blocks so the snapshot never shares the live
		// *AgentInfo with a later Apply (a read-after-unlock race), and project the
		// in-memory StatusSince onto the wire-only StatusSinceWire on that copy.
		cp.Claude = enrichForWire(sess.Claude)
		cp.Codex = enrichForWire(sess.Codex)
		cp.AgentGraph = sess.AgentGraph.Clone()
		cp.DisplayName = cloneDisplayName(sess.DisplayName)
		sessions = append(sessions, cp)
	}
	// Sort into chip order (lessChipOrder), which carries a PID tie-break for
	// determinism: equal sort keys would otherwise leave order to map iteration,
	// making positional selectors (rpc.pickSession index, sessions[0])
	// nondeterministic across snapshots.
	sort.Slice(sessions, func(i, j int) bool {
		return lessChipOrder(sessions[i], sessions[j])
	})
	return Snapshot{SchemaVersion: CurrentSchemaVersion, Sessions: sessions, UpdatedAt: time.Now(), Capabilities: s.caps}
}

// enrichForWire returns a wire-ready copy of an enrichment block: a value copy
// (so the snapshot never shares the live pointer with a concurrent Apply) with the
// two derived wire fields stamped onto that copy —
//
//   - StatusSinceWire from the in-memory StatusSince, non-nil only once a status
//     edge has stamped it, so the field omits cleanly before then;
//   - PendingWriters from the in-memory Pending map's key set, sorted and with
//     "main" substituted for the empty key, nil while no writer is blocked.
//
// Both are projections and nothing else: they are recomputed here on every
// snapshot, so a stale value on the live block (Load leaves one behind until the
// hydrate consumes it) can never reach the wire. nil in, nil out.
func enrichForWire(info *AgentInfo) *AgentInfo {
	if info == nil {
		return nil
	}
	cp := *info
	cp.StatusSinceWire = nil
	if !cp.StatusSince.IsZero() {
		since := cp.StatusSince
		cp.StatusSinceWire = &since
	}
	cp.PendingWriters = pendingWritersForWire(cp.Pending)
	return &cp
}

// lessChipOrder defines the left-to-right chip order on the bottom bar:
// sessions with a resolved workspace come first, ordered by numeric workspace
// ID (so chips follow workspace order); within a workspace, and among
// sessions whose workspace is not yet resolved, oldest-started wins.
// Unresolved-workspace sessions are pushed to the end.
func lessChipOrder(a, b Session) bool {
	aID, aResolved := workspaceID(a)
	bID, bResolved := workspaceID(b)
	if aResolved != bResolved {
		return aResolved // resolved sessions sort before unresolved ones
	}
	if aResolved && aID != bID {
		return aID < bID
	}
	if !a.StartedAt.Equal(b.StartedAt) {
		return a.StartedAt.Before(b.StartedAt)
	}
	return a.PID < b.PID // deterministic tie-break (Phase 0.9)
}

// workspaceID returns the session's Hyprland workspace ID and whether it is
// resolved. ID 0 is treated as unresolved (Hyprland workspaces are positive,
// or negative for special workspaces).
func workspaceID(s Session) (int, bool) {
	if s.Hyprland == nil || s.Hyprland.WorkspaceID == 0 {
		return 0, false
	}
	return s.Hyprland.WorkspaceID, true
}

// Subscribe returns a channel that receives snapshots after mutations which
// changed them, paired with their shared encoding. The channel is buffered and
// coalesces toward the newest complete replacement if the receiver lags. A
// subscriber that also performs an independent initial Snapshot read must treat
// channel values as notifications and re-read current state. Close the returned
// cancel func to unsubscribe.
func (s *Store) Subscribe() (<-chan Broadcast, func()) {
	ch := make(chan Broadcast, 4)
	s.mu.Lock()
	s.subscribers[ch] = struct{}{}
	s.mu.Unlock()

	cancel := func() {
		s.mu.Lock()
		delete(s.subscribers, ch)
		close(ch)
		s.mu.Unlock()
	}
	return ch, cancel
}

func (s *Store) broadcast(snap Snapshot, gen uint64) {
	s.broadcastMu.Lock()
	defer s.broadcastMu.Unlock()
	if gen <= s.broadcastGen {
		return
	}
	s.broadcastGen = gen
	// Peek the subscriber count before paying for the encode. A daemon with no bar
	// attached — headless box, bar mid-restart — should not serialize into the void.
	s.mu.RLock()
	subscribers := len(s.subscribers)
	s.mu.RUnlock()
	if subscribers == 0 {
		return
	}

	// Encode OUTSIDE the lock, once, for everyone. Holding RLock across the encode
	// would block every Apply (the write lock) for its duration, which is the exact
	// contention this package is being pulled apart to remove. A subscriber that
	// arrives in the gap misses nothing: rpc.subscribe hands a brand-new connection
	// its own full snapshot on connect, independently of this path.
	b := Broadcast{Snapshot: snap}
	if js, err := marshalSnapshot(snap); err == nil {
		b.JSON = js
	} else {
		// Leave JSON nil and let the subscriber encode the snapshot itself; a bar
		// falling back to a slower path beats a bar receiving a broken frame.
		fmt.Fprintf(os.Stderr, "state: broadcast encode failed: %v\n", err)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	for ch := range s.subscribers {
		select {
		case ch <- b:
		default:
			// Coalesce to the latest full snapshot without blocking the writer.
			// Keeping the old frame would be incorrect when this is the final
			// mutation: there may be no later broadcast to repair the subscriber.
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- b:
			default:
			}
		}
	}
}

// marshalSnapshot produces the compact wire body a broadcast shares with every
// subscriber: plain encoding/json defaults, so it is byte-for-byte what
// json.Encoder would write for the same value (minus the trailing newline) and
// what Store.persist writes with indentation added. It is a named function rather
// than an inline call so the golden test pins THESE bytes against the frozen
// state.json document instead of a copy of this line that could drift from it.
func marshalSnapshot(snap Snapshot) ([]byte, error) { return json.Marshal(snap) }

func (s *Store) persist(snap Snapshot) error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".state-*.json")
	if err != nil {
		return err
	}
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(snap); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

// Load hydrates the store from the on-disk mirror. Errors are returned but
// callers should treat them as non-fatal — the live reconciliation pass will
// rebuild state from /proc anyway.
//
// It is the exact inverse of enrichForWire for the derived fields it can invert:
// pending_writers is decoded back into the in-memory Pending map so the daemon
// speaks one language about prompt ownership from the first instruction after
// Load. That decode is pure translation — it restores WHICH writers were blocked
// and asserts nothing about whether they still are. The policy (re-stamping Since
// to startup, seeding a pre-T12 mirror, dropping writers a transcript proves
// resolved) belongs to dropStaleSessions, which runs next.
//
// StatusSince is deliberately NOT recovered here; it has no wire form, and
// dropStaleSessions stamps it to startup time on purpose.
func (s *Store) Load() error {
	if s.path == "" {
		return nil
	}
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	var snap Snapshot
	if err := json.NewDecoder(f).Decode(&snap); err != nil {
		return err
	}
	if snap.SchemaVersion != 0 && snap.SchemaVersion != CurrentSchemaVersion {
		// Clean break: incompatible mirrors are not migrated. Live discovery and
		// hooks rebuild the store, including intentionally ignoring schema v2.
		return nil
	}
	if snap.SchemaVersion == 0 {
		for _, sess := range snap.Sessions {
			if sess.Agent == AgentKindCodex {
				return nil
			}
		}
	}
	hydratedAt := time.Now()
	s.mu.Lock()
	for i := range snap.Sessions {
		sess := snap.Sessions[i]
		hydratePendingWriters(sess.Claude)
		hydratePendingWriters(sess.Codex)
		hydrateAgentGraph(&sess, hydratedAt)
		s.sessions[sess.PID] = &sess
	}
	s.mu.Unlock()
	return nil
}

// hydratePendingWriters decodes a block's persisted pending_writers back into the
// in-memory Pending map and drops the wire slice, so the map is the single source
// of truth the instant Load returns (enrichForWire re-derives the slice for every
// later snapshot). A block with no persisted writers is left with a nil map, which
// is what tells dropStaleSessions it is reading a pre-T12 mirror.
func hydratePendingWriters(info *AgentInfo) {
	if info == nil {
		return
	}
	info.Pending = pendingFromWire(info.PendingWriters)
	info.PendingWriters = nil
	info.derivePendingTool()
}
