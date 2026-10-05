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
	// UsageLimit is set while the session's last turn ended on its provider's
	// usage limit. Publication reads its status as limited while the record is
	// active (ProjectPublished); omitted otherwise.
	UsageLimit *UsageLimit `json:"usage_limit,omitempty"`
	// usageLimitActivityAt is the newest activity ClearUsageLimit has seen, so
	// RecordUsageLimit can refuse evidence that the session has already outrun.
	// In-memory only; a restart forgets it, which costs at most one stale read.
	usageLimitActivityAt time.Time
	// Navigable is populated only on detached aggregate copies. It says an
	// exact local route candidate exists now; every action still revalidates the
	// pane, window, liveness, and StartedAt before acting. Host-local snapshots
	// leave it false/omitted, preserving the durable schema.
	Navigable bool `json:"navigable,omitempty"`
	// LocalWorkspace is the Hyprland workspace ID, ON THIS MACHINE, of the
	// window displaying this session. It is populated only on detached
	// aggregate copies, and only for remote rows: a local session already
	// carries its own workspace in Hyprland, while a remote one has its
	// Hyprland block stripped precisely because those coordinates locate the
	// REMOTE desktop and mean nothing here.
	//
	// The distinction matters to a reader deciding where to switch. "Which
	// workspace is this session on" has two different answers for a federated
	// row, and the only actionable one is this: the workspace whose window is
	// showing it. Zero means unresolved (Hyprland workspace IDs are non-zero),
	// which is the same convention workspaceID uses.
	LocalWorkspace int `json:"local_workspace,omitempty"`

	// Agent names the coding-agent CLI that owns this session: "claude" or
	// "codex" (the AgentKind* constants). Set at discovery from the process. It
	// selects which enrichment block (claude/codex) hooks write and how a
	// renderer reads status. Omitted only when the kind is not yet known.
	Agent string `json:"agent,omitempty"`
	// DisplayName is Switchboard-owned Codex display metadata. It is valid only
	// for its exact conversation_id and never changes Codex's native thread name.
	DisplayName *DisplayName `json:"display_name,omitempty"`
	// ResolvedName is the provider host's current display name before project
	// prefixing. The host that owns the process is the only machine that can
	// safely resolve Claude's PID-keyed session file; federated consumers carry
	// this projection instead of looking up the same PID in their local process
	// namespace. Codex does not need this projection: its generated display name
	// and authoritative native /rename already travel in provider state. It is
	// refreshed by the reconciler and omitted until the first successful naming
	// pass.
	ResolvedName string `json:"resolved_name,omitempty"`

	Wezterm  *WeztermInfo  `json:"wezterm,omitempty"`
	Hyprland *HyprlandInfo `json:"hyprland,omitempty"`
	// Herdr is set when the session runs in a herdr pane; herdr is then the
	// authority for its status (see HerdrInfo).
	Herdr *HerdrInfo `json:"herdr,omitempty"`
	// Claude, Codex and Pi are the per-agent enrichment blocks; they share one
	// shape (AgentInfo). At most one is populated, matching Agent — the others
	// are omitted. The split keeps the frozen "claude" wire key intact for
	// existing bar consumers while adding "codex" and "pi" purely additively.
	Claude *AgentInfo `json:"claude,omitempty"`
	Codex  *AgentInfo `json:"codex,omitempty"`
	// Pi is allocated by the first Pi extension hook attributed to the
	// session. Until then a Pi session has no block and publishes through its
	// herdr graph alone.
	Pi *AgentInfo `json:"pi,omitempty"`

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
	// AgentKindPi is discovered through herdr. Its extension hooks
	// (switchboard-ctl pi-hook) carry Pi's lifecycle into rpc, where today only
	// their usage-limit evidence is applied; once a hook binds the session, its
	// enrichment block is Pi and its graph root is Pi's session id (PiRootID).
	AgentKindPi = "pi"
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
	// StatusLimited (usage_limit.go) is publication-only and never stored.
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
	case AgentKindPi:
		return s.Pi
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
	if kind == AgentKindPi {
		if s.Pi == nil {
			s.Pi = &AgentInfo{}
		}
		return s.Pi
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
	// ActivePaneID is the WezTerm pane in the visible tab, read from the
	// compositor title marker. Nil means the integration has not supplied it.
	// This live observation stays off the wire; Focused is its public projection.
	ActivePaneID *int `json:"-"`
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
	// delegating. Sorted by RunID — SnapshotChangeKey JSON-encodes every tagged
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
	// as PendingWriters so prompt ownership survives a daemon restart (§9); these
	// correlators are re-earned from the next hook. The per-call records that DO
	// survive a restart live in PendingPrompts below, beside this map rather than
	// inside it: this map is the ownership authority every existing reader folds on,
	// and widening it to a set would change what `len(Pending)` means to all of them.
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
	// The sort is load-bearing, not cosmetic: SnapshotChangeKey JSON-encodes every
	// tagged field to decide whether to publish, so an unsorted slice built by
	// ranging a map would differ between snapshots of identical state and republish
	// to every waybar slot on every reconcile tick.
	PendingWriters []string `json:"pending_writers,omitempty"`

	// PendingPrompts is the per-CALL companion to that key set: one record for
	// every prompt each blocked writer holds open, oldest-first within a writer and
	// grouped by writer in the same ascending order PendingWriters uses. It is
	// ADDITIVE beside PendingWriters, never a replacement — an older reader that
	// knows only the key set keeps reading exactly what it always read, and a
	// state.json written before this field existed still restores a writer's red
	// through the key set alone.
	//
	// Why the whole set and not one prompt per writer: one assistant turn dispatches
	// two or more gated calls in 7.6 % of tool-using turns
	// (askuserquestion-model-plan.md §2.2), and the daemon has tracked them as a set
	// since the prompt container landed. Persisting only the key set collapsed that
	// set to one record on every restart, so answering the survivor took the chip
	// green while the other calls were still blocking the agent — the same missed RED
	// the container fixed, reintroduced by the restart alone.
	//
	// It carries Attention for the same class of reason. The restore path used to
	// stamp `approval` on every rebuilt prompt because the block could not say
	// otherwise, so every question-red came back as an approval-red; that is a
	// silent downgrade now that the two are distinguished.
	//
	// Unlike PendingWriters this is NOT derived from Pending — it is state in its
	// own right, held in memory in the bare ("" = main) writer spelling and
	// projected to the wire ("main") by enrichForWire. What enrichForWire does
	// enforce is the invariant that makes the two fields safe to read together:
	// records are pruned to Pending's key set, so a writer the reconciler released
	// can never leave a record behind for a later restore to raise a red from.
	PendingPrompts []PendingPromptRecord `json:"pending_prompts,omitempty"`

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

// PendingPromptRecord is ONE open prompt as it survives a daemon restart: the
// writer that owns it, what it is waiting for, which kind of wait it is, and when
// it opened. It is the persisted element of AgentInfo.PendingPrompts.
//
// Writer is the same spelling PendingWriters uses — a bare subagent agent_id, or
// "main" for the main thread on the wire ("" in memory).
//
// Tool and InputHash are the correlators the hook-speed match uses. They are
// persisted here even though the Pending map's copies are not, and the difference
// is deliberate: a record stands for one CALL, and a record with no tool cannot be
// told apart from its writer's other calls — neither by the hook's (tool, hash)
// rule nor by the transcript latch that binds a tool_use id — so a set of them
// would restore as several indistinguishable reds that any one completion could
// clear. The Pending map's values stay ownership-only, exactly as before.
//
// Since is the instant the PermissionRequest fired, NOT the restart instant. It
// dates the record against the writer's own transcript so a restored prompt can
// still bind the call it gates. The restart instant remains the clock the
// whole-file resolution rules run from; those two clocks are separate on purpose
// (see the claude adapter's restoredPending and Restore).
//
// Attention is the neutral graph's spelling — "approval" or "user_input". An
// absent or unrecognized value degrades to the tool's own kind rather than to a
// colourless prompt, so a hand-edited or future value can never restore as green.
//
// CallID is the optional opaque identity already earned by the transcript latch.
// Persisting it lets hydrate subtract one answered call from a mixed parallel set
// without dropping the writer's still-blocked siblings. Empty keeps the legacy
// fail-closed behavior for prompts that had not bound before shutdown.
type PendingPromptRecord struct {
	Writer    string    `json:"writer"`
	Tool      string    `json:"tool,omitempty"`
	InputHash string    `json:"input_hash,omitempty"`
	CallID    string    `json:"call_id,omitempty"`
	Attention string    `json:"attention,omitempty"`
	Since     time.Time `json:"since"`
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
	a.PendingPrompts = ownedPendingPrompts(a.Pending, a.PendingPrompts)
	a.derivePendingTool()
}

// ClearPending forgets every pending prompt. Used where the whole red is being
// abandoned rather than resolved per writer: a session rotation (a /clear or fork
// retires the prompts with the session that raised them) and the chip's exit from
// "permission".
func (a *AgentInfo) ClearPending() {
	a.Pending = nil
	a.PendingPrompts = nil
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
// lowest-keyed writer's) suffixed with "+N" when N further CALLS are also
// blocked. One prompt therefore logs exactly what it always logged, so
// statustune.ParseDecision and `switchboard-ctl diagnose` keep reading it, while
// parallel prompts from one writer no longer silently report as a single one.
//
// An empty map falls through to PendingTool, which is "" for a live block and may
// be a hand-seeded or hydrated value otherwise.
func (a *AgentInfo) PendingSummary() string {
	if n := a.PendingCallCount(); n > 1 {
		return fmt.Sprintf("%s+%d", a.PendingTool, n-1)
	}
	return a.PendingTool
}

// PendingCallCount reports how many individual calls currently hold the block
// red. PendingPrompts is the per-call source; a writer represented only by the
// legacy key set contributes one conservative residual call.
func (a *AgentInfo) PendingCallCount() int {
	counts := make(map[string]int, len(a.Pending)+len(a.PendingWriters))
	for writer := range a.Pending {
		counts[writer] = 0
	}
	for _, writer := range a.PendingWriters {
		counts[pendingWriterMemoryName(writer)] = 0
	}
	for _, record := range a.PendingPrompts {
		counts[pendingWriterMemoryName(record.Writer)]++
	}
	total := 0
	for _, count := range counts {
		if count == 0 {
			count = 1
		}
		total += count
	}
	return total
}

// PendingCallCountForWriter is PendingCallCount narrowed to one wire or memory
// writer spelling. It lets renderers say "main (3 calls)" instead of repeating
// a writer name once per prompt.
func (a *AgentInfo) PendingCallCountForWriter(writer string) int {
	want := pendingWriterMemoryName(writer)
	count := 0
	for _, record := range a.PendingPrompts {
		if pendingWriterMemoryName(record.Writer) == want {
			count++
		}
	}
	if count > 0 {
		return count
	}
	if _, ok := a.Pending[want]; ok {
		return 1
	}
	for _, candidate := range a.PendingWriters {
		if pendingWriterMemoryName(candidate) == want {
			return 1
		}
	}
	return 0
}

func pendingWriterMemoryName(writer string) string {
	if writer == PendingWriterMain {
		return ""
	}
	return writer
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

// ownedPendingPrompts returns the records whose writer still owns a prompt in
// pending. It is the invariant that keeps the two pending fields readable
// together: the Pending map is the ownership authority, so a record for a writer
// that map no longer holds is a red nobody owns, and restoring one would
// resurrect a prompt this daemon already released.
//
// It allocates only when something is actually dropped, because it runs on every
// snapshot.
func ownedPendingPrompts(pending map[string]PendingPrompt, records []PendingPromptRecord) []PendingPromptRecord {
	if len(records) == 0 {
		return nil
	}
	owned := true
	for _, record := range records {
		if _, ok := pending[bareWriter(record.Writer)]; !ok {
			owned = false
			break
		}
	}
	if owned {
		return records
	}
	kept := make([]PendingPromptRecord, 0, len(records))
	for _, record := range records {
		if _, ok := pending[bareWriter(record.Writer)]; ok {
			kept = append(kept, record)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

// pendingPromptsForWire translates a record set into its wire spelling: the
// writer keys become PendingWriterMain for the main thread, and the records are
// grouped by writer in the same ascending order pendingWritersForWire emits,
// preserving each writer's own oldest-first order within its group.
//
// The ordering is load-bearing for the same reason that sort is: snapshotChangeKey
// JSON-encodes every tagged field to decide whether to publish, so a set whose
// order depended on map iteration would differ between snapshots of identical
// state and republish to every bar on every tick.
func pendingPromptsForWire(records []PendingPromptRecord) []PendingPromptRecord {
	if len(records) == 0 {
		return nil
	}
	byWriter := make(map[string][]PendingPromptRecord, len(records))
	for _, record := range records {
		writer := bareWriter(record.Writer)
		record.Writer = wireWriter(writer)
		byWriter[writer] = append(byWriter[writer], record)
	}
	writers := make([]string, 0, len(byWriter))
	for writer := range byWriter {
		writers = append(writers, wireWriter(writer))
	}
	sort.Strings(writers)
	wire := make([]PendingPromptRecord, 0, len(records))
	for _, writer := range writers {
		wire = append(wire, byWriter[bareWriter(writer)]...)
	}
	return wire
}

// pendingPromptsFromWire is the inverse: the records in their in-memory ("" =
// main) spelling, order preserved. Like pendingFromWire it manufactures nothing.
func pendingPromptsFromWire(records []PendingPromptRecord) []PendingPromptRecord {
	if len(records) == 0 {
		return nil
	}
	decoded := make([]PendingPromptRecord, len(records))
	for i, record := range records {
		record.Writer = bareWriter(record.Writer)
		decoded[i] = record
	}
	return decoded
}

// bareWriter and wireWriter translate the one writer key whose two spellings
// differ. Everything in memory is bare; everything on the wire says "main".
func bareWriter(writer string) string {
	if writer == PendingWriterMain {
		return ""
	}
	return writer
}

func wireWriter(writer string) string {
	if writer == "" {
		return PendingWriterMain
	}
	return writer
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
// that can forward that exact generation. Store subscriptions use channel values
// only as wakeups and re-read CurrentBroadcast, because a value queued before an
// independent initial read may be older than it.
type Broadcast struct {
	Snapshot Snapshot
	// JSON is the COMPACT encoding of Snapshot: exactly what json.Encoder writes,
	// minus the trailing newline. It is paired with this particular broadcast;
	// consumers which also take an independent initial read must treat the channel
	// as a notification and re-read CurrentBroadcast, because a queued value can
	// predate that initial read. The RPC subscription does exactly that.
	//
	// It is nil when the encode failed. A subscriber must then encode Snapshot
	// itself rather than send a truncated frame.
	//
	// Treat it as immutable: every subscriber holds this same backing array.
	JSON []byte
}

// NewBroadcast builds one fan-out unit: the snapshot plus the single encoding
// every subscriber for that publish shares. JSON is nil when the encode failed;
// consumers must then encode Snapshot themselves rather than send a truncated
// frame.
func NewBroadcast(snap Snapshot) Broadcast {
	b := Broadcast{Snapshot: snap}
	js, err := marshalSnapshot(snap)
	if err != nil {
		fmt.Fprintf(os.Stderr, "state: broadcast encode failed: %v\n", err)
		return b
	}
	b.JSON = js
	return b
}

type Store struct {
	path        string
	mu          sync.RWMutex
	sessions    map[int]*Session
	subscribers map[chan Broadcast]struct{}
	caps        *Capabilities
	// publishedKey is SnapshotChangeKey of the last snapshot Apply decided to
	// publish — the reference the change check compares against. nil before the
	// first publish (and after a failed encode or a failed persist), which compares
	// unequal to everything, so the next Apply publishes.
	publishedKey []byte
	// publishedGen counts adoptions of publishedKey. Apply captures it at adopt
	// time so that a persist failing AFTER the unlock can retract its own adoption
	// without clobbering one a later Apply made in the meantime. See
	// invalidatePublished.
	publishedGen uint64
	// broadcastGen serializes live frame adoption/fanout and ordered persistence
	// enqueueing by Apply generation. Two concurrent Apply calls may arrive out
	// of order; an older one must never replace the latest subscriber frame or be
	// queued behind a newer state.json replacement.
	broadcastMu  sync.Mutex
	broadcastGen uint64
	// frameMu guards lastBroadcast independently of the store and publish-stats
	// locks. The frame is immutable after publication and swapped before fanout.
	frameMu       sync.RWMutex
	lastBroadcast *Broadcast
	// statsMu guards the publish-stats accumulator and NOTHING else. See
	// publishstats.go for why it is its own lock rather than a few fields under
	// s.mu.
	statsMu sync.Mutex
	stats   publishCounters
	// persistSnapshot is the ordered state.json writer. It is a seam only so the
	// generation-order test can pause one write deterministically.
	persistSnapshot func(Snapshot) error
	// persistMu protects a latest-wins persistence batch. Publication queues its
	// full replacement while holding broadcastMu, then releases the publication
	// sequencer before waiting for disk. One short-lived worker writes batches in
	// order; concurrent generations which accumulate behind an active write are
	// collapsed to the newest full snapshot, whose completion satisfies every
	// older waiter in that batch.
	persistMu      sync.Mutex
	persistRunning bool
	persistPending *persistBatch
	// afterBroadcastUnlock is a deterministic test seam. Production leaves it
	// nil; ordering tests use it to observe that a generation has been queued and
	// the live publication lock has actually been released.
	afterBroadcastUnlock func(uint64)
}

type persistBatch struct {
	snapshot Snapshot
	waiters  []chan error
}

func New(statePath string) *Store {
	s := &Store{
		path:        statePath,
		sessions:    make(map[int]*Session),
		subscribers: make(map[chan Broadcast]struct{}),
		// The first publish-stats window opens here, not when the daemon starts
		// the ticker, so the startup burst is counted somewhere rather than
		// discarded. It makes the first line's window= longer than the interval,
		// which is why that field is printed.
		stats: publishCounters{since: time.Now()},
	}
	s.persistSnapshot = s.persist
	return s
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
	snap := s.publishedLocked()
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
	if err := s.broadcast(snap, gen); err != nil {
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
// LATER mutation and dropped as a no-op. Live broadcast generation ordering and
// persistence enqueueing are serialized separately after the unlock; disk I/O
// is not. The cost is one encode inside the write lock, which is microseconds
// against the milliseconds of terminal/WM I/O the reconciler used to hold it for.
func (s *Store) adoptPublishedLocked(snap Snapshot) (gen uint64, changed bool) {
	key := SnapshotChangeKey(snap)
	if key != nil && bytes.Equal(key, s.publishedKey) {
		s.countDecision(false)
		return s.publishedGen, false
	}
	// A nil key (encode failure) lands here and is counted as a publish, because
	// that is what it causes: failing open republishes.
	s.countDecision(true)
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

// SnapshotChangeKey encodes everything about a snapshot that a consumer can act
// on. Equal keys may still carry later advisory clock values on the raw wire;
// suppressing those alone is intentional because they do not represent a state
// edge or change freshness truth.
//
// It is a JSON encode rather than a hand-written field-by-field comparison on
// purpose. A comparator's failure mode is silent: add a field to Session, forget
// to compare it, and bars simply stop updating for that field with no test
// failing anywhere. Encoding inherits the wire contract instead — every field
// carrying a JSON tag is compared by construction, and every in-memory-only
// field (json:"-") is excluded by construction, now and for anything added later.
//
// Three advertised clock fields are advisory and deliberately excluded:
//
//   - Snapshot.UpdatedAt is stamped by snapshotLocked on every snapshot.
//   - AgentGraph.ObservedAt advances with provider polling. A lagging value only
//     moves Fresh's lower bound earlier and cannot make a consumer falsely stale.
//   - AgentNode.UpdatedAt is a provider poll/display-age anchor. Any actionable
//     runtime, attention, lifecycle, usage, or completion change still moves its
//     own encoded field and therefore the key.
//
// The key clears those clocks only on detached copies; the subscription frame
// and state.json retain the real timestamps. Other values re-stamped from the
// wall clock fall out for free because they are already json:"-":
//
//   - WeztermInfo.TitleAt — the resolver re-samples the pane title every reconcile
//     tick and stamps it (mapping.weztermInfo), so it advances on a quiet machine.
//   - WeztermInfo.Title with it: the agent CLI repaints that string continuously
//     while a turn runs (animated spinner glyph).
//   - AgentInfo.PendingTool — transient red-onset state, not a clock but equally
//     invisible to consumers. AgentInfo.Pending's correlator VALUES
//     (Tool/InputHash/Since) fall out for the same reason.
//
// WindowTitle and AgentGraph.FreshUntil need no special key rule. Terminal
// constructors normalize titles with panetitle.Normalize, and AgentGraph's JSON
// boundary emits CeilFreshUntil, so the key automatically sees the same derived
// values consumers receive. In particular FreshUntil must remain encoded: if
// its ceiling moves, suppressing the frame could make a consumer falsely stale.
//
// AgentInfo.PendingPrompts carries those same correlators and IS encoded, which is
// not a contradiction: a record is stamped once when its PermissionRequest fires
// and never re-derived from a clock, so an unchanged prompt set encodes
// byte-identically tick after tick. It is ordered rather than map-ranged for
// exactly that reason (pendingPromptsForWire).
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
func SnapshotChangeKey(snap Snapshot) []byte {
	var sessions []Session
	if snap.Sessions != nil {
		sessions = make([]Session, len(snap.Sessions))
		copy(sessions, snap.Sessions)
	}
	for i := range sessions {
		if sessions[i].AgentGraph == nil {
			continue
		}
		graph := *sessions[i].AgentGraph
		graph.ObservedAt = time.Time{}
		if graph.Nodes != nil {
			graph.Nodes = make([]AgentNode, len(graph.Nodes))
			copy(graph.Nodes, sessions[i].AgentGraph.Nodes)
		}
		for j := range graph.Nodes {
			graph.Nodes[j].UpdatedAt = time.Time{}
		}
		sessions[i].AgentGraph = &graph
	}
	key, err := json.Marshal(struct {
		SchemaVersion int           `json:"schema_version"`
		Sessions      []Session     `json:"sessions"`
		Capabilities  *Capabilities `json:"capabilities,omitempty"`
	}{SchemaVersion: snap.SchemaVersion, Sessions: sessions, Capabilities: snap.Capabilities})
	if err != nil {
		// Not reachable today (Snapshot holds no unencodable field), but fail OPEN:
		// a nil key compares unequal to everything, so a broken encode republishes
		// rather than silently freezing every bar on the last good state.
		fmt.Fprintf(os.Stderr, "state: change key encode failed: %v\n", err)
		return nil
	}
	return key
}

// ObservablyEqual reports whether two snapshots are indistinguishable to every
// wire consumer. It uses the same key as Store.Apply so a caller cannot drift
// from the publish gate when Session gains another JSON field. UpdatedAt and
// other explicitly in-memory-only fields are intentionally ignored.
//
// An encoding failure compares unequal: one redundant publication is safer
// than suppressing a real state change.
func ObservablyEqual(a, b Snapshot) bool {
	keyA, keyB := SnapshotChangeKey(a), SnapshotChangeKey(b)
	return keyA != nil && keyB != nil && bytes.Equal(keyA, keyB)
}

// Snapshot returns a detached copy of current state. Once the store lock is
// released, publication encodes this value concurrently with later Apply calls;
// sharing even a small pointer or slice would make one frame internally
// inconsistent (and data-racy under -race).
func (s *Store) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshotLocked()
}

// PublishedSnapshot is Snapshot as consumers see it (ProjectPublished). Daemon
// logic reads Snapshot, whose statuses are the provider FSM's own; every path
// that hands a snapshot to a renderer, a client, or state.json reads this.
func (s *Store) PublishedSnapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.publishedLocked()
}

func (s *Store) publishedLocked() Snapshot {
	snap := s.snapshotLocked()
	return ProjectPublished(snap, snap.UpdatedAt)
}

func (s *Store) snapshotLocked() Snapshot {
	sessions := make([]Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		cp := *sess
		if sess.Wezterm != nil {
			value := *sess.Wezterm
			cp.Wezterm = &value
		}
		if sess.Hyprland != nil {
			value := *sess.Hyprland
			if value.ActivePaneID != nil {
				paneID := *value.ActivePaneID
				value.ActivePaneID = &paneID
			}
			cp.Hyprland = &value
		}
		// Deep-copy the enrichment blocks so the snapshot never shares the live
		// *AgentInfo with a later Apply (a read-after-unlock race), and project the
		// in-memory StatusSince onto the wire-only StatusSinceWire on that copy.
		cp.Claude = enrichForWire(sess.Claude)
		cp.Codex = enrichForWire(sess.Codex)
		cp.Pi = enrichForWire(sess.Pi)
		cp.AgentGraph = sess.AgentGraph.Clone()
		cp.DisplayName = cloneDisplayName(sess.DisplayName)
		cp.Herdr = cloneHerdr(sess.Herdr)
		cp.UsageLimit = cloneUsageLimit(sess.UsageLimit)
		sessions = append(sessions, cp)
	}
	// Sort into chip order, which carries a PID tie-break for determinism: equal
	// sort keys would otherwise leave order to map iteration, making positional
	// selectors (rpc.pickSession index, sessions[0]) nondeterministic across
	// snapshots. A host-local snapshot has only its own workspaces to order by,
	// so it passes no override.
	SortSessionOrder(sessions, nil)
	var caps *Capabilities
	if s.caps != nil {
		value := *s.caps
		caps = &value
	}
	return Snapshot{SchemaVersion: CurrentSchemaVersion, Sessions: sessions, UpdatedAt: time.Now(), Capabilities: caps}
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
//
// PendingPrompts is NOT derived — it is real state, carried by value like
// Workflows — but it is pruned and re-spelled on the same copy, so the two pending
// fields on any published snapshot describe one consistent set of owners. Pruning
// here rather than at every mutation site is what keeps a legacy reconciler path
// (which releases prompts through DropPending/ClearPending and knows nothing about
// records) from leaving an orphan behind.
func enrichForWire(info *AgentInfo) *AgentInfo {
	if info == nil {
		return nil
	}
	cp := *info
	cp.Workflows = append([]WorkflowStatus(nil), info.Workflows...)
	if info.Pending != nil {
		cp.Pending = make(map[string]PendingPrompt, len(info.Pending))
		for writer, prompt := range info.Pending {
			cp.Pending[writer] = prompt
		}
	}
	cp.StatusSinceWire = nil
	if !cp.StatusSince.IsZero() {
		since := cp.StatusSince
		cp.StatusSinceWire = &since
	}
	cp.PendingWriters = pendingWritersForWire(cp.Pending)
	cp.PendingPrompts = pendingPromptsForWire(ownedPendingPrompts(cp.Pending, cp.PendingPrompts))
	return &cp
}

// SortSessionOrder sorts the canonical session list for navigation and displays.
//
// workspace supplies a row's workspace key, overriding the session's own
// Hyprland block whenever it reports one; returning false falls back to that
// block, and a nil func is the host-local rule (every row keyed by its own
// window). The federated client view injects a key because a REMOTE row's
// Hyprland block is nil by construction — a remote desktop's workspace numbers
// mean nothing on the machine drawing the bar — while the local WezTerm window
// displaying that session's SSH pane does sit on a local workspace, and that is
// where the user expects its chip.
//
// The sort is stable, so rows whose keys tie keep the caller's input order.
// That is what makes the aggregate deterministic without teaching this
// comparator about hostnames: PID and even StartedAt can repeat across hosts,
// and the aggregate builder appends hosts in a fixed order.
func SortSessionOrder(sessions []Session, workspace func(Session) (int, bool)) {
	sort.SliceStable(sessions, func(i, j int) bool {
		return lessSessionOrder(sessions[i], sessions[j], workspace)
	})
}

// lessSessionOrder defines the canonical session order:
// sessions with a resolved workspace come first, ordered by numeric workspace
// ID (so chips follow workspace order); within a workspace, and among
// sessions whose workspace is not yet resolved, oldest-started wins.
// Unresolved-workspace sessions are pushed to the end.
func lessSessionOrder(a, b Session, workspace func(Session) (int, bool)) bool {
	aID, aResolved := chipWorkspace(a, workspace)
	bID, bResolved := chipWorkspace(b, workspace)
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

// chipWorkspace returns the workspace a row sorts by: the injected key when it
// resolves one, else the session's own window.
func chipWorkspace(s Session, workspace func(Session) (int, bool)) (int, bool) {
	if workspace != nil {
		if id, ok := workspace(s); ok {
			return id, true
		}
	}
	return workspaceID(s)
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
	ch := make(chan Broadcast, 1)
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

func (s *Store) broadcast(snap Snapshot, gen uint64) error {
	s.broadcastMu.Lock()
	if gen <= s.broadcastGen {
		s.broadcastMu.Unlock()
		return nil
	}
	s.broadcastGen = gen
	// Peek the subscriber count before paying for the encode. A daemon with no bar
	// attached — headless box, bar mid-restart — should not serialize into the void.
	s.mu.RLock()
	subscribers := len(s.subscribers)
	s.mu.RUnlock()
	if subscribers == 0 {
		// The cache backs a later subscriber's initial frame. Do not leave a
		// pre-mutation frame there while the daemon temporarily has no clients.
		s.frameMu.Lock()
		s.lastBroadcast = nil
		s.frameMu.Unlock()
		persisted := s.queuePersistence(snap)
		s.broadcastMu.Unlock()
		s.didUnlockBroadcast(gen)
		return <-persisted
	}

	// Encode OUTSIDE the lock, once, for everyone. Holding RLock across the encode
	// would block every Apply (the write lock) for its duration, which is the exact
	// contention this package is being pulled apart to remove. A subscriber that
	// arrives in the gap misses nothing: rpc.subscribe hands a brand-new connection
	// its own full snapshot on connect, independently of this path.
	b := NewBroadcast(snap)
	if len(b.JSON) != 0 {
		// Counted here rather than at the publish decision because this is the only
		// place a frame actually exists: the zero-subscriber return above skips the
		// encode entirely, so those publishes have no size to average. publish-stats
		// reports frame_bytes as the mean over frames ENCODED, not over publishes.
		s.countFrame(len(b.JSON))
	}
	s.frameMu.Lock()
	s.lastBroadcast = &b
	s.frameMu.Unlock()

	s.mu.RLock()
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
	s.mu.RUnlock()
	persisted := s.queuePersistence(snap)
	s.broadcastMu.Unlock()
	s.didUnlockBroadcast(gen)
	return <-persisted
}

func (s *Store) didUnlockBroadcast(gen uint64) {
	if s.afterBroadcastUnlock != nil {
		s.afterBroadcastUnlock(gen)
	}
}

// queuePersistence adopts one full replacement into the ordered disk queue.
// Callers invoke it while holding broadcastMu, so pending snapshots can only
// move forward. It returns immediately; waiting happens after broadcastMu is
// released, keeping a slow filesystem out of the live publication path.
func (s *Store) queuePersistence(snap Snapshot) <-chan error {
	done := make(chan error, 1)
	s.persistMu.Lock()
	if s.persistPending == nil {
		s.persistPending = &persistBatch{snapshot: snap}
	} else {
		s.persistPending.snapshot = snap
	}
	s.persistPending.waiters = append(s.persistPending.waiters, done)
	if !s.persistRunning {
		s.persistRunning = true
		go s.runPersistence()
	}
	s.persistMu.Unlock()
	return done
}

func (s *Store) runPersistence() {
	for {
		s.persistMu.Lock()
		batch := s.persistPending
		if batch == nil {
			s.persistRunning = false
			s.persistMu.Unlock()
			return
		}
		s.persistPending = nil
		s.persistMu.Unlock()

		err := s.persistSnapshot(batch.snapshot)
		for _, waiter := range batch.waiters {
			waiter <- err
			close(waiter)
		}
	}
}

// CurrentBroadcast returns the latest published frame. When no cached frame is
// available (notably after a zero-subscriber publish), it builds one from the
// current store state. The double-checked build is serialized with broadcast so
// two readers or a concurrent fanout cannot walk the cache backward.
func (s *Store) CurrentBroadcast() Broadcast {
	s.frameMu.RLock()
	b := s.lastBroadcast
	s.frameMu.RUnlock()
	if b != nil {
		return *b
	}

	s.broadcastMu.Lock()
	defer s.broadcastMu.Unlock()
	s.frameMu.RLock()
	b = s.lastBroadcast
	s.frameMu.RUnlock()
	if b != nil {
		return *b
	}
	built := NewBroadcast(s.PublishedSnapshot())
	s.frameMu.Lock()
	s.lastBroadcast = &built
	s.frameMu.Unlock()
	return built
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
		hydratePiBlock(sess.Pi)
		hydrateUsageLimit(&sess, hydratedAt)
		hydrateAgentGraph(&sess, hydratedAt)
		s.sessions[sess.PID] = &sess
	}
	s.mu.Unlock()
	return nil
}

// hydratePiBlock restores a persisted Pi block's display state only: the
// session id, transcript and last status. Nothing on it may claim live
// authority after a restart, so its status date is re-earned, as for the other
// blocks, and any prompt ownership or subagent count (which Pi never writes) is
// dropped rather than trusted.
func hydratePiBlock(info *AgentInfo) {
	if info == nil {
		return
	}
	*info = AgentInfo{SessionID: info.SessionID, Transcript: info.Transcript, Status: info.Status}
}

// hydratePendingWriters decodes a block's persisted pending_writers back into the
// in-memory Pending map and drops the wire slice, so the map is the single source
// of truth the instant Load returns (enrichForWire re-derives the slice for every
// later snapshot). A block with no persisted writers is left with a nil map, which
// is what tells dropStaleSessions it is reading a pre-T12 mirror.
//
// pending_prompts is decoded back to its bare writer spelling on the same pass,
// and its writers JOIN the key set rather than being filtered against it. The
// union is the direction that cannot invent a false green: this daemon always
// writes the two fields consistently, so they can only disagree in a
// hand-edited or partially-written mirror, and reading a record whose owner the
// key set forgot as "no red here" would drop a prompt nothing else can re-raise.
// The reverse pruning still happens on the way out (enrichForWire).
func hydratePendingWriters(info *AgentInfo) {
	if info == nil {
		return
	}
	info.Pending = pendingFromWire(info.PendingWriters)
	info.PendingWriters = nil
	info.PendingPrompts = pendingPromptsFromWire(info.PendingPrompts)
	for _, record := range info.PendingPrompts {
		if _, ok := info.Pending[record.Writer]; ok {
			continue
		}
		if info.Pending == nil {
			info.Pending = make(map[string]PendingPrompt, 1)
		}
		info.Pending[record.Writer] = PendingPrompt{}
	}
	info.derivePendingTool()
}
