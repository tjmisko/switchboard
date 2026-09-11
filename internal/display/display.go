// Package display describes presentation choices and a live, ordered session
// view. A mode selects a surface; it never changes session order or navigation.
package display

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tjmisko/switchboard/internal/sessionview"
	"github.com/tjmisko/switchboard/internal/state"
)

type Mode string

const (
	Chips   Mode = "chips"
	Circles Mode = "circles"
)

func ParseMode(text string) (Mode, error) {
	mode := Mode(strings.TrimSpace(text))
	if mode != Chips && mode != Circles {
		return "", fmt.Errorf("display mode must be chips or circles (got %q)", text)
	}
	return mode, nil
}
func ModePath() string {
	if path := os.Getenv("SWITCHBOARD_DISPLAY_MODE_FILE"); path != "" {
		return path
	}
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "switchboard", "display-mode")
}
func ReadMode(path string) (Mode, error) {
	if path == "" {
		return Chips, nil
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Chips, nil
	}
	if err != nil {
		return "", err
	}
	return ParseMode(string(b))
}
func WantsBottom(mode Mode, topVisible bool, count int) bool {
	return mode == Chips && topVisible && count > 0
}

// Frame is a full replacement in canonical session order, never a patch or a
// limited slot set. Consumers choose their own geometry, labels and shortcuts.
// Publisher identity lets independent consumers detect an abruptly dead broker.
type Frame struct {
	Version          int       `json:"version"`
	Mode             Mode      `json:"mode"`
	Connected        bool      `json:"connected"`
	Visible          bool      `json:"visible"`
	PublisherPID     int       `json:"publisher_pid"`
	PublisherStarted uint64    `json:"publisher_started"`
	Sessions         []Session `json:"sessions"`
}
type Session struct {
	Key           string    `json:"key"`
	Index         int       `json:"index"`
	Hostname      string    `json:"hostname"`
	PID           int       `json:"pid"`
	StartedAt     time.Time `json:"started_at"`
	Label         string    `json:"label"`
	Status        string    `json:"status"`
	Classes       []string  `json:"classes"`
	Focused       bool      `json:"focused"`
	Navigable     bool      `json:"navigable"`
	FocusSelector string    `json:"focus_selector,omitempty"`
	Tooltip       string    `json:"tooltip"`
}

// Build preserves the producer's complete order. Naming is injected so each
// long-lived producer can reuse its own naming cache without I/O in this model.
func Build(snapshot state.Snapshot, mode Mode, connected, visible bool, name func(state.Session) string) Frame {
	frame := Frame{Version: 1, Mode: mode, Connected: connected, Visible: visible, Sessions: make([]Session, 0, len(snapshot.Sessions))}
	for i, item := range sessionview.Prepare(snapshot, 0) {
		s := item.Session
		label := name(s)
		key := fmt.Sprintf("host:%s:pid:%d:started:%s", s.Hostname, s.PID, s.StartedAt.UTC().Format(time.RFC3339Nano))
		selector := item.Selector
		tooltip := fmt.Sprintf("%s\n%s", label, item.Status)
		if s.Hostname != "" {
			tooltip += " · " + s.Hostname
		}
		if s.CWD != "" {
			tooltip += "\n" + s.CWD
		}
		frame.Sessions = append(frame.Sessions, Session{Key: key, Index: i, Hostname: s.Hostname, PID: s.PID, StartedAt: s.StartedAt, Label: label, Status: item.Status, Classes: item.Classes, Focused: s.Focused, Navigable: sessionview.Navigable(s), FocusSelector: selector, Tooltip: tooltip})
	}
	return frame
}
func (f Frame) JSON() ([]byte, error) { b, err := json.Marshal(f); return append(b, '\n'), err }

// WaybarData is a GKeyFile adapter for the GTK module. The public JSON frame
// above remains independent of Waybar and has exactly the same order/identity.
// GLib is already loaded by Waybar, so the adapter needs no extra JSON library.
func (f Frame) WaybarData() []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "[display]\nversion=%d\nmode=%s\nconnected=%t\nvisible=%t\npublisher_pid=%d\npublisher_started=%d\ncount=%d\n", f.Version, f.Mode, f.Connected, f.Visible, f.PublisherPID, f.PublisherStarted, len(f.Sessions))
	for i, s := range f.Sessions {
		fmt.Fprintf(&b, "\n[session-%d]\nkey=%s\nselector=%s\ntooltip=%s\nclasses=", i, keyString(s.Key), keyString(s.FocusSelector), keyString(s.Tooltip))
		for _, class := range s.Classes {
			b.WriteString(strings.ReplaceAll(keyString(class), ";", "\\;"))
			b.WriteByte(';')
		}
		b.WriteByte('\n')
	}
	return []byte(b.String())
}
func keyString(s string) string {
	return strings.NewReplacer("\\", "\\\\", "\n", "\\n", "\r", "\\r", "\t", "\\t", " ", "\\s").Replace(s)
}
