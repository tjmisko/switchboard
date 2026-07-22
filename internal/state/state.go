// Package state owns the in-memory session map and the on-disk state.json
// mirror. All mutations go through Store.Apply, which calls subscribers and
// schedules an atomic write.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type Session struct {
	PID       int       `json:"pid"`
	CWD       string    `json:"cwd"`
	TTY       string    `json:"tty"`
	StartedAt time.Time `json:"started_at"`
	Focused   bool      `json:"focused"`
	// Suspended is true when the agent process is job-control-stopped (Ctrl-Z /
	// SIGSTOP). Renderers grey such chips out. Omitted when false so the common
	// case stays off the wire.
	Suspended bool `json:"suspended,omitempty"`

	// Agent names the coding-agent CLI that owns this session: "claude" or
	// "codex" (the AgentKind* constants). Set at discovery from the process. It
	// selects which enrichment block (claude/codex) hooks write and how a
	// renderer reads status. Omitted only when the kind is not yet known.
	Agent string `json:"agent,omitempty"`

	Wezterm  *WeztermInfo  `json:"wezterm,omitempty"`
	Hyprland *HyprlandInfo `json:"hyprland,omitempty"`
	// Claude and Codex are the per-agent enrichment blocks; they share one shape
	// (AgentInfo). Exactly one is populated, matching Agent — the other is
	// omitted. The split keeps the frozen "claude" wire key intact for existing
	// bar consumers while adding "codex" purely additively.
	Claude *AgentInfo `json:"claude,omitempty"`
	Codex  *AgentInfo `json:"codex,omitempty"`
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
	SessionID  string `json:"session_id,omitempty"`
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

	// InFlightSubagents is how many subagent Tasks the main thread has launched
	// but not yet collected (transcript.InFlightTasks), recomputed each reconcile
	// tick. It is the S dimension: >0 with an idle main thread is the delegating
	// (green) case. Exposed on the wire (omitempty, so absent when 0 — the golden
	// contract is unchanged) so renderers can show "N agents" in the tooltip and
	// `switchboard-ctl list` reveals the true state behind a green chip.
	InFlightSubagents int `json:"in_flight_subagents,omitempty"`

	// StatusSince marks when Status last transitioned to its current value. The
	// reconciler uses it to age out a "permission" chip that Claude Code left
	// latched (a declined question / interrupt fires no clearing hook). Kept
	// in-memory (json:"-") as the source of truth for the duration math; it is
	// projected to the wire as StatusSinceWire (status_since) at snapshot time, so
	// the in-memory value's zero-reads-as-"long ago" reconcile semantics are
	// unchanged (a re-hydrated session is re-evaluated against its transcript on
	// the first reconcile; dropStaleSessions re-stamps it to startup time).
	StatusSince time.Time `json:"-"`

	// PendingTool is the tool_name the current "permission" prompt was raised for
	// (captured from the PermissionRequest hook at red-onset). The hold gate
	// matches a later PostToolUse's tool_name against it to clear red at hook speed
	// when the *approved* tool completes — distinct from a sibling/Task PostToolUse
	// that must keep the chip red (docs/status-color-state-model.md A2/case 12).
	// In-memory only: it is transient onset state, not part of the wire contract.
	PendingTool string `json:"-"`
}

// ClaudeInfo is the original name for AgentInfo, kept as an alias so existing
// callers and tests compile unchanged.
type ClaudeInfo = AgentInfo

type Snapshot struct {
	Sessions     []Session     `json:"sessions"`
	UpdatedAt    time.Time     `json:"updated_at"`
	Capabilities *Capabilities `json:"capabilities,omitempty"`
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

type Store struct {
	path        string
	mu          sync.RWMutex
	sessions    map[int]*Session
	subscribers map[chan Snapshot]struct{}
	caps        *Capabilities
}

func New(statePath string) *Store {
	return &Store{
		path:        statePath,
		sessions:    make(map[int]*Session),
		subscribers: make(map[chan Snapshot]struct{}),
	}
}

// SetCapabilities records the detected backend stack. It is included in every
// subsequent snapshot. Set once at daemon startup, before serving.
func (s *Store) SetCapabilities(c Capabilities) {
	s.mu.Lock()
	s.caps = &c
	s.mu.Unlock()
}

// Apply mutates the store under lock, then notifies subscribers and persists.
func (s *Store) Apply(fn func(map[int]*Session)) {
	s.mu.Lock()
	fn(s.sessions)
	snap := s.snapshotLocked()
	s.mu.Unlock()

	s.broadcast(snap)
	if err := s.persist(snap); err != nil {
		fmt.Fprintf(os.Stderr, "state: persist failed: %v\n", err)
	}
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
		sessions = append(sessions, cp)
	}
	// Sort into chip order (lessChipOrder), which carries a PID tie-break for
	// determinism: equal sort keys would otherwise leave order to map iteration,
	// making positional selectors (rpc.pickSession index, sessions[0])
	// nondeterministic across snapshots.
	sort.Slice(sessions, func(i, j int) bool {
		return lessChipOrder(sessions[i], sessions[j])
	})
	return Snapshot{Sessions: sessions, UpdatedAt: time.Now(), Capabilities: s.caps}
}

// enrichForWire returns a wire-ready copy of an enrichment block: a value copy
// (so the snapshot never shares the live pointer with a concurrent Apply) with
// the in-memory StatusSince projected onto StatusSinceWire — non-nil only once a
// status edge has stamped it, so the wire field omits cleanly before then. nil
// in, nil out.
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

// Subscribe returns a channel that receives every snapshot after a mutation.
// The channel is buffered (cap=4) and drops if the receiver lags. Close the
// returned cancel func to unsubscribe.
func (s *Store) Subscribe() (<-chan Snapshot, func()) {
	ch := make(chan Snapshot, 4)
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

func (s *Store) broadcast(snap Snapshot) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for ch := range s.subscribers {
		select {
		case ch <- snap:
		default:
		}
	}
}

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
	s.mu.Lock()
	for i := range snap.Sessions {
		sess := snap.Sessions[i]
		s.sessions[sess.PID] = &sess
	}
	s.mu.Unlock()
	return nil
}
