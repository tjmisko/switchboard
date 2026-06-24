// Command switchboard-waybar subscribes to the daemon and emits waybar JSON. It
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

	sblabel "github.com/tjmisko/switchboard/internal/label"
	"github.com/tjmisko/switchboard/internal/projectname"
	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/state"
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
			emit(waybarOutput{Text: "✕", Tooltip: "switchboard not running", Class: []string{"tracker-down"}})
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
// status, a "focused" flag, and a "suspended" flag so waybar CSS can paint the
// chip. Empty slots get class=["empty"] so the CSS can collapse them.
func renderSlot(snap state.Snapshot, slot int) waybarOutput {
	if slot >= len(snap.Sessions) {
		return waybarOutput{Text: "", Class: []string{"empty"}}
	}
	s := snap.Sessions[slot]
	cfg := projectname.Load()
	status := sessionStatus(s)
	// The primary class paints the chip's color; delegating reuses working's green
	// (Q1 default: pure green, no CSS change needed). The raw "delegating" rides
	// along as a secondary class so the bar CAN add a badge/different shade later
	// without losing the green underneath.
	classes := []string{chipClass(status)}
	if status == state.StatusDelegating {
		classes = append(classes, "delegating")
	}
	if s.Focused {
		classes = append(classes, "focused")
	}
	if s.Suspended {
		classes = append(classes, "suspended")
	}
	return waybarOutput{
		Text:    sblabel.Chip(cfg, s),
		Tooltip: sessionTooltip(cfg, s),
		Class:   classes,
		Alt:     chipClass(status),
	}
}

// chipClass maps a session status to the CSS class that paints its color.
// delegating shares working's green; everything else maps to itself.
func chipClass(status string) string {
	if status == state.StatusDelegating {
		return state.StatusWorking
	}
	return status
}

// renderAggregate is the original single-module mode. Kept for ad-hoc
// inspection (`switchboard-waybar | jq .`) but not driven by the live bar.
func renderAggregate(snap state.Snapshot) waybarOutput {
	if len(snap.Sessions) == 0 {
		return waybarOutput{Text: "", Tooltip: "no claude sessions", Class: []string{"empty"}}
	}
	cfg := projectname.Load()
	var parts []string
	for _, s := range snap.Sessions {
		mark := ""
		if s.Focused {
			mark = "*"
		}
		parts = append(parts, mark+sblabel.Chip(cfg, s))
	}
	return waybarOutput{
		Text:  strings.Join(parts, "  "),
		Class: []string{"multi"},
		Alt:   fmt.Sprintf("%d", len(snap.Sessions)),
	}
}

func sessionStatus(s state.Session) string {
	info := s.Enrichment()
	if info == nil || info.Status == "" {
		return "unknown"
	}
	return info.Status
}

// sessionTooltip renders the Compact-stacked hover with pango markup:
//
//	<b>arachne</b>   ● working
//	assess-npm-vulnerabilities
//	~/Projects/Arachne · ws 4 · pid 292511
//
// Line 1 is the project abbreviation + a status-colored dot; line 2 is the bare
// task name (the project prefix stripped, since the abbrev already shows it);
// line 3 is dimmed metadata.
func sessionTooltip(cfg projectname.Config, s state.Session) string {
	abbrev := projectname.CanonicalForDir(cfg, s.CWD)
	task := projectname.TaskForDir(cfg, s.CWD, sblabel.RawName(s))
	status := sessionStatus(s)

	statusText := status
	// A delegating chip is green but idle on the main thread; spell out why so the
	// green reads as "N agents working" rather than looking stuck.
	if status == state.StatusDelegating {
		if n := subagentCount(s); n > 0 {
			statusText = fmt.Sprintf("delegating · %d agent%s", n, plural(n))
		}
	}
	if s.Suspended {
		statusText += " · suspended"
	}

	ws := "-"
	if s.Hyprland != nil && s.Hyprland.Workspace != "" {
		ws = s.Hyprland.Workspace
	}
	dot := fmt.Sprintf("<span foreground='%s'>●</span>", statusColor(status))
	meta := fmt.Sprintf("%s · ws %s · pid %d", contractHome(s.CWD), ws, s.PID)
	return fmt.Sprintf(
		"<b>%s</b>   %s %s\n%s\n<span foreground='#6c7086' size='smaller'>%s</span>",
		pangoEscape(abbrev), dot, pangoEscape(statusText),
		pangoEscape(task),
		pangoEscape(meta),
	)
}

// statusColor maps a session status to the pango hex color of its tooltip dot,
// matching the chip palette (working/delegating green, idle amber, permission
// red, otherwise grey).
func statusColor(status string) string {
	switch status {
	case state.StatusWorking, state.StatusDelegating:
		return "#a6e3a1"
	case state.StatusIdle:
		return "#f9e2af"
	case state.StatusPermission:
		return "#f38ba8"
	default:
		return "#6c7086"
	}
}

// contractHome replaces a leading $HOME with ~ for a shorter metadata line.
func contractHome(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if p == home {
			return "~"
		}
		if rest, ok := strings.CutPrefix(p, home+"/"); ok {
			return "~/" + rest
		}
	}
	return p
}

// pangoEscape escapes the pango markup metacharacters in user-controlled text
// (session/project names) so they render literally.
func pangoEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// subagentCount reports the in-flight subagent count from the session's
// enrichment block (0 when absent), for the delegating tooltip.
func subagentCount(s state.Session) int {
	if info := s.Enrichment(); info != nil {
		return info.InFlightSubagents
	}
	return 0
}

// plural returns the plural suffix for n.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func emit(o waybarOutput) {
	b, _ := json.Marshal(o)
	fmt.Println(string(b))
}

func defaultSocketPath() string {
	if x := os.Getenv("XDG_RUNTIME_DIR"); x != "" {
		return filepath.Join(x, "switchboard.sock")
	}
	return fmt.Sprintf("/tmp/switchboard-%d.sock", os.Getuid())
}
