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
	Address   string `json:"address"`
	Workspace string `json:"workspace"`
	Monitor   string `json:"monitor"`
}

type ClaudeInfo struct {
	SessionID  string `json:"session_id,omitempty"`
	Transcript string `json:"transcript,omitempty"`
	Status     string `json:"status"` // working|idle|permission|unknown
}

type Snapshot struct {
	Sessions  []Session `json:"sessions"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Store struct {
	path        string
	mu          sync.RWMutex
	sessions    map[int]*Session
	subscribers map[chan Snapshot]struct{}
}

func New(statePath string) *Store {
	return &Store{
		path:        statePath,
		sessions:    make(map[int]*Session),
		subscribers: make(map[chan Snapshot]struct{}),
	}
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
	// Sort by StartedAt, with PID as a deterministic tie-break. Equal timestamps
	// would otherwise leave order to map iteration, making positional selectors
	// (rpc.pickSession index, sessions[0]) nondeterministic across snapshots.
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].StartedAt.Equal(sessions[j].StartedAt) {
			return sessions[i].PID < sessions[j].PID
		}
		return sessions[i].StartedAt.Before(sessions[j].StartedAt)
	})
	return Snapshot{Sessions: sessions, UpdatedAt: time.Now()}
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
