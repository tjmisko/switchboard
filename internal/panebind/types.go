// Package panebind owns the small correlation seam between an exact remote
// Switchboard session and the local WezTerm pane that displays its TTY stream.
//
// It intentionally does not own SSH, remote snapshots, RPC, or navigation
// policy. Callers feed it complete per-host live sets and explicit pane signals.
package panebind

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

const MaxHostnameBytes = 253

var (
	ErrInvalidSession   = errors.New("panebind: invalid session")
	ErrInvalidPane      = errors.New("panebind: invalid local pane")
	ErrSessionNotLive   = errors.New("panebind: exact session is not live")
	ErrSessionUnbound   = errors.New("panebind: exact session is unbound")
	ErrSessionAmbiguous = errors.New("panebind: exact session has multiple pane bindings")
	ErrRouteChanged     = errors.New("panebind: route changed during validation")
	ErrDuplicateLivePID = errors.New("panebind: live host snapshot contains a duplicate pid")
	ErrPaneNotFound     = errors.New("panebind: local wezterm pane not found")
	ErrPaneAmbiguous    = errors.New("panebind: local wezterm pane is ambiguous")
	ErrWindowNotFound   = errors.New("panebind: marked window not found")
	ErrWindowAmbiguous  = errors.New("panebind: marked window is ambiguous")
	ErrNoLiveValidator  = errors.New("panebind: live validator is required")
	ErrStaleEmitTarget  = errors.New("panebind: emit target is no longer live")
	ErrNotTTY           = errors.New("panebind: target is not a character device")
)

// SessionKey is the identity of one live row. PID is unique only within a
// running host, so both fields are required.
type SessionKey struct {
	Hostname string `json:"host"`
	PID      int    `json:"pid"`
}

// ExactSessionKey adds Switchboard's discovery timestamp as a stale-action
// fence. It distinguishes replacements observed during daemon continuity, but
// it is not a kernel process-birth token and is not a second namespace.
type ExactSessionKey struct {
	Hostname  string    `json:"host"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
}

func (k ExactSessionKey) SessionKey() SessionKey {
	return SessionKey{Hostname: k.Hostname, PID: k.PID}
}

// Canonical returns a map-safe form: UTC and with any monotonic clock reading
// stripped. JSON round trips and state.Store values then compare identically.
func (k ExactSessionKey) Canonical() ExactSessionKey {
	k.StartedAt = k.StartedAt.Round(0).UTC()
	return k
}

func (k ExactSessionKey) Equal(other ExactSessionKey) bool {
	return k.Hostname == other.Hostname && k.PID == other.PID && k.StartedAt.Equal(other.StartedAt)
}

func (k ExactSessionKey) Validate() error {
	if err := validateHostname(k.Hostname); err != nil {
		return err
	}
	if k.PID <= 0 || k.StartedAt.IsZero() {
		return ErrInvalidSession
	}
	return nil
}

func validateHostname(host string) error {
	if host == "" || len(host) > MaxHostnameBytes || strings.TrimSpace(host) != host {
		return ErrInvalidSession
	}
	// Remote snapshots and terminal signals must use the same canonical spelling:
	// lowercase and without a DNS absolute-name trailing dot.
	if host != strings.ToLower(host) || strings.HasSuffix(host, ".") {
		return ErrInvalidSession
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 {
			return ErrInvalidSession
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
				continue
			}
			return ErrInvalidSession
		}
	}
	return nil
}

// LocalPaneRef contains only values derived by the local WezTerm Lua process.
// Pane IDs are scoped by GUI process; WindowID makes validation stricter and
// supplies the exact window-title marker.
type LocalPaneRef struct {
	GUIPID   int `json:"gui_pid"`
	WindowID int `json:"window_id"`
	PaneID   int `json:"pane_id"`
}

func (p LocalPaneRef) Validate() error {
	if p.GUIPID <= 0 || p.WindowID < 0 || p.PaneID < 0 {
		return ErrInvalidPane
	}
	return nil
}

// WindowMarker is appended by the WezTerm integration and matched together
// with the GUI PID. It contains no remote-controlled text.
func WindowMarker(p LocalPaneRef) string {
	return fmt.Sprintf("[sbw:%d:%d]", p.GUIPID, p.WindowID)
}
