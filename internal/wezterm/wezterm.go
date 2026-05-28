// Package wezterm enumerates panes across every running wezterm mux on the
// box and exposes a focus dispatcher. It works by walking the per-mux Unix
// sockets in $XDG_RUNTIME_DIR/wezterm/gui-sock-<pid>; each mux owns an
// independent pane-id namespace, so every Pane is tagged with its MuxPID +
// MuxSocket.
package wezterm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Pane is one row from `wezterm cli list --format json` from a specific mux.
type Pane struct {
	MuxPID      int    `json:"mux_pid"`
	MuxSocket   string `json:"mux_socket"`
	WindowID    int    `json:"window_id"`
	TabID       int    `json:"tab_id"`
	PaneID      int    `json:"pane_id"`
	Title       string `json:"title"`
	WindowTitle string `json:"window_title"`
	CWDURL      string `json:"cwd"`
	TTYName     string `json:"tty_name"`
	IsActive    bool   `json:"is_active"`
}

type cliPane struct {
	WindowID    int    `json:"window_id"`
	TabID       int    `json:"tab_id"`
	PaneID      int    `json:"pane_id"`
	Title       string `json:"title"`
	WindowTitle string `json:"window_title"`
	CWDURL      string `json:"cwd"`
	TTYName     string `json:"tty_name"`
	IsActive    bool   `json:"is_active"`
}

// Muxes lists every wezterm mux currently exposing a gui socket whose owning
// PID is still alive. Dead-but-not-yet-cleaned sockets (which happen on
// SIGKILL or abrupt shutdown) are silently skipped — connecting to them
// would hang until the per-call timeout.
func Muxes() ([]Mux, error) {
	dir := socketDir()
	if dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var muxes []Mux
	for _, e := range entries {
		name := e.Name()
		rest, ok := strings.CutPrefix(name, "gui-sock-")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(rest)
		if err != nil {
			continue
		}
		if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); err != nil {
			continue
		}
		muxes = append(muxes, Mux{PID: pid, Socket: filepath.Join(dir, name)})
	}
	return muxes, nil
}

type Mux struct {
	PID    int
	Socket string
}

// List runs `wezterm cli list --format json` against every running mux and
// returns the union of panes, each tagged with the mux it came from.
func List(ctx context.Context) ([]Pane, error) {
	muxes, err := Muxes()
	if err != nil {
		return nil, err
	}
	var out []Pane
	for _, m := range muxes {
		panes, err := listOne(ctx, m)
		if err != nil {
			fmt.Fprintf(os.Stderr, "wezterm: mux %d list failed: %v\n", m.PID, err)
			continue
		}
		out = append(out, panes...)
	}
	return out, nil
}

func listOne(ctx context.Context, m Mux) ([]Pane, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "wezterm", "cli", "list", "--format", "json")
	cmd.Env = append(os.Environ(), "WEZTERM_UNIX_SOCKET="+m.Socket)
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var raw []cliPane
	if err := json.Unmarshal(output, &raw); err != nil {
		return nil, err
	}
	panes := make([]Pane, 0, len(raw))
	for _, p := range raw {
		panes = append(panes, Pane{
			MuxPID:      m.PID,
			MuxSocket:   m.Socket,
			WindowID:    p.WindowID,
			TabID:       p.TabID,
			PaneID:      p.PaneID,
			Title:       p.Title,
			WindowTitle: p.WindowTitle,
			CWDURL:      p.CWDURL,
			TTYName:     p.TTYName,
			IsActive:    p.IsActive,
		})
	}
	return panes, nil
}

// FindByTTY returns the pane attached to the given /dev/pts/N tty, or nil if
// no mux owns it. Used by the mapping layer to go from a claude PID's tty to
// the wezterm pane it lives in.
func FindByTTY(ctx context.Context, tty string) (*Pane, error) {
	panes, err := List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range panes {
		if panes[i].TTYName == tty {
			return &panes[i], nil
		}
	}
	return nil, nil
}

// ActivatePane focuses the given pane inside its mux's window. Window-level
// focus (raising it to the front in the WM) is the WM's job — call this after
// `hyprctl dispatch focuswindow`.
func ActivatePane(ctx context.Context, muxSocket string, paneID int) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "wezterm", "cli", "activate-pane", "--pane-id", strconv.Itoa(paneID))
	cmd.Env = append(os.Environ(), "WEZTERM_UNIX_SOCKET="+muxSocket)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("activate-pane %d: %w (%s)", paneID, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func socketDir() string {
	if x := os.Getenv("XDG_RUNTIME_DIR"); x != "" {
		return filepath.Join(x, "wezterm")
	}
	return ""
}
