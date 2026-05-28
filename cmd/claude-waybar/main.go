// Command claude-waybar subscribes to the daemon and emits waybar JSON. It
// runs in one of two modes:
//
//	(no flag)        aggregate mode — emits a single chip-set in one module
//	                 (kept for debugging; not used by the live bar)
//	--slot N         slot mode — emits only the Nth session's chip, or an
//	                 empty class if no session exists at that index. Used by
//	                 ~/.config/waybar/config.jsonc, which declares N slot
//	                 modules so each can carry real GTK CSS (border, hover,
//	                 padding).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tjmisko/claude-tracker/internal/rpc"
	"github.com/tjmisko/claude-tracker/internal/state"
)

type waybarOutput struct {
	Text    string   `json:"text"`
	Tooltip string   `json:"tooltip,omitempty"`
	Class   []string `json:"class"`
	Alt     string   `json:"alt,omitempty"`
}

func main() {
	socketPath := flag.String("socket", defaultSocketPath(), "daemon socket")
	slot := flag.Int("slot", -1, "emit JSON for Nth session only (waybar slot mode)")
	flag.Parse()

	for {
		runOnce(*socketPath, *slot)
		// Daemon socket dropped — emit a degraded chip so waybar shows
		// something while we wait, then retry.
		if *slot >= 0 {
			emit(waybarOutput{Text: "", Class: []string{"empty"}})
		} else {
			emit(waybarOutput{Text: "✕", Tooltip: "claude-tracker not running", Class: []string{"tracker-down"}})
		}
		time.Sleep(2 * time.Second)
	}
}

func runOnce(socketPath string, slot int) {
	c, err := rpc.Dial(socketPath)
	if err != nil {
		return
	}
	defer c.Close()
	if err := c.Send(rpc.Request{Cmd: "subscribe"}); err != nil {
		return
	}
	for {
		var resp rpc.Response
		if err := c.Recv(&resp); err != nil {
			return
		}
		if resp.Snapshot == nil {
			continue
		}
		if slot >= 0 {
			emit(renderSlot(*resp.Snapshot, slot))
		} else {
			emit(renderAggregate(*resp.Snapshot))
		}
	}
}

// renderSlot emits JSON for the Nth session. The class array carries the
// status and a "focused" flag so waybar CSS can paint the chip. Empty slots
// get class=["empty"] so the CSS can collapse them.
func renderSlot(snap state.Snapshot, slot int) waybarOutput {
	if slot >= len(snap.Sessions) {
		return waybarOutput{Text: "", Class: []string{"empty"}}
	}
	s := snap.Sessions[slot]
	classes := []string{sessionStatus(s)}
	if s.Focused {
		classes = append(classes, "focused")
	}
	return waybarOutput{
		Text:    shortName(s),
		Tooltip: sessionTooltip(s),
		Class:   classes,
		Alt:     sessionStatus(s),
	}
}

// renderAggregate is the original single-module mode. Kept for ad-hoc
// inspection (`claude-waybar | jq .`) but not driven by the live bar.
func renderAggregate(snap state.Snapshot) waybarOutput {
	if len(snap.Sessions) == 0 {
		return waybarOutput{Text: "", Tooltip: "no claude sessions", Class: []string{"empty"}}
	}
	var parts []string
	for _, s := range snap.Sessions {
		mark := ""
		if s.Focused {
			mark = "*"
		}
		parts = append(parts, mark+shortName(s))
	}
	return waybarOutput{
		Text:  strings.Join(parts, "  "),
		Class: []string{"multi"},
		Alt:   fmt.Sprintf("%d", len(snap.Sessions)),
	}
}

func sessionStatus(s state.Session) string {
	if s.Claude == nil || s.Claude.Status == "" {
		return "unknown"
	}
	return s.Claude.Status
}

func sessionTooltip(s state.Session) string {
	name := shortName(s)
	ws := "-"
	if s.Hyprland != nil && s.Hyprland.Workspace != "" {
		ws = s.Hyprland.Workspace
	}
	status := sessionStatus(s)
	return fmt.Sprintf("%s [%s] — ws %s — %s (pid %d)", name, status, ws, s.CWD, s.PID)
}

// shortName picks the human label for a session. Prefer the wezterm window
// title (which Claude Code itself writes via OSC) over a cwd basename.
func shortName(s state.Session) string {
	if s.Wezterm != nil && s.Wezterm.WindowTitle != "" {
		title := s.Wezterm.WindowTitle
		for _, prefix := range []string{"✳ ", "⠂ ", "⠐ ", "⠁ ", "⠈ ", "⠠ ", "⠄ ", "⡀ ", "⢀ "} {
			if rest, ok := strings.CutPrefix(title, prefix); ok {
				title = rest
				break
			}
		}
		return title
	}
	if s.CWD != "" {
		return filepath.Base(s.CWD)
	}
	return fmt.Sprintf("pid %d", s.PID)
}

func emit(o waybarOutput) {
	b, _ := json.Marshal(o)
	fmt.Println(string(b))
}

func defaultSocketPath() string {
	if x := os.Getenv("XDG_RUNTIME_DIR"); x != "" {
		return filepath.Join(x, "claude-tracker.sock")
	}
	return fmt.Sprintf("/tmp/claude-tracker-%d.sock", os.Getuid())
}
