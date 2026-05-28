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

	Wezterm  *WeztermInfo  `json:"wezterm,omitempty"`
	Hyprland *HyprlandInfo `json:"hyprland,omitempty"`
	Claude   *ClaudeInfo   `json:"claude,omitempty"`
}

type WeztermInfo struct {
	MuxPID      int    `json:"mux_pid"`
	MuxSocket   string `json:"mux_socket"`
	PaneID      int    `json:"pane_id"`
	TabID       int    `json:"tab_id"`
	WindowID    int    `json:"window_id"`
	WindowTitle string `json:"window_title"`
}

type HyprlandInfo struct {
	Address     string `json:"address"`
	Workspace   string `json:"workspace"`
	WorkspaceID int    `json:"workspace_id"`
	Monitor     string `json:"monitor"`
}

type ClaudeInfo struct {
	SessionID  string `json:"session_id,omitempty"`
	Transcript string `json:"transcript,omitempty"`
	Status     string `json:"status"` // working|idle|permission|unknown
}

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
		sessions = append(sessions, *sess)
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
