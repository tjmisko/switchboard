// Command switchboard-ctl is the user-facing CLI client. It talks to the
// daemon over its Unix socket and prints either human-friendly text or raw
// JSON.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/state"
)

func main() {
	socketPath := flag.String("socket", defaultSocketPath(), "daemon socket")
	jsonOut := flag.Bool("json", false, "emit JSON instead of human-friendly text")
	flag.Usage = usage
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}

	// bottombar manages the bottom waybar's lifecycle and runs before the
	// mandatory dial: its `watch` mode must tolerate a down daemon and
	// reconnect on its own.
	if args[0] == "bottombar" {
		cmdBottombar(args[1:], *socketPath)
		return
	}

	c, err := rpc.Dial(*socketPath)
	if err != nil {
		fail("connect daemon: %v", err)
	}
	defer c.Close()

	switch args[0] {
	case "list":
		cmdList(c, *jsonOut)
	case "focus":
		selector := "active"
		if len(args) > 1 {
			selector = args[1]
		}
		cmdFocus(c, selector)
	case "status":
		cmdStatus(c)
	case "pick":
		cmdPick(c)
	case "cycle":
		direction := "next"
		if len(args) > 1 {
			direction = args[1]
		}
		cmdCycle(c, direction)
	case "attention":
		cmdAttention(c)
	case "hook":
		if len(args) < 2 {
			fail("hook requires an event name")
		}
		cmdHook(c, args[1])
	default:
		usage()
		os.Exit(2)
	}
}

func cmdList(c *rpc.Client, jsonOut bool) {
	snap := mustList(c)
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(snap)
		return
	}
	if len(snap.Sessions) == 0 {
		fmt.Println("no claude sessions")
		return
	}
	for i, s := range snap.Sessions {
		marker := " "
		if s.Focused {
			marker = "*"
		}
		fmt.Printf("%s [%d] pid=%d cwd=%s\n", marker, i, s.PID, s.CWD)
		if s.Wezterm != nil {
			fmt.Printf("       wezterm: mux=%d pane=%d title=%q\n", s.Wezterm.MuxPID, s.Wezterm.PaneID, s.Wezterm.WindowTitle)
		}
		if s.Hyprland != nil {
			fmt.Printf("       hypr:    addr=%s workspace=%s\n", s.Hyprland.Address, s.Hyprland.Workspace)
		}
	}
}

func cmdFocus(c *rpc.Client, selector string) {
	if err := c.Send(rpc.Request{Cmd: "focus", Selector: selector}); err != nil {
		fail("send: %v", err)
	}
	var resp rpc.Response
	if err := c.Recv(&resp); err != nil {
		fail("recv: %v", err)
	}
	if resp.Error != "" {
		fail("%s", resp.Error)
	}
}

func cmdStatus(c *rpc.Client) {
	snap := mustList(c)
	fmt.Printf("%d session(s)\n", len(snap.Sessions))
}

// cmdPick emits one tab-separated line per session, ordered as the snapshot.
// Format: PID \t LABEL \t CWD \t WORKSPACE
// LABEL is the wezterm window title (with leading spinner glyph stripped) or
// the cwd basename as fallback. Intended to be piped into fzf with
// `--with-nth=2..` so the user sees the label but the PID stays in the
// selected line for the focus call.
func cmdPick(c *rpc.Client) {
	snap := mustList(c)
	for _, s := range snap.Sessions {
		label := shortName(s)
		ws := "-"
		if s.Hyprland != nil && s.Hyprland.Workspace != "" {
			ws = s.Hyprland.Workspace
		}
		focusMark := " "
		if s.Focused {
			focusMark = "*"
		}
		fmt.Printf("%d\t%s %s\tws %s\t%s\n", s.PID, focusMark, label, ws, s.CWD)
	}
}

// cmdCycle focuses the next or previous session, wrapping. Position is
// determined by the focused session; if none is focused, "next" picks the
// first session and "prev" picks the last.
func cmdCycle(c *rpc.Client, direction string) {
	snap := mustList(c)
	if len(snap.Sessions) == 0 {
		return
	}
	idx := -1
	for i, s := range snap.Sessions {
		if s.Focused {
			idx = i
			break
		}
	}
	n := len(snap.Sessions)
	var target int
	switch direction {
	case "next", "up":
		if idx < 0 {
			target = 0
		} else {
			target = (idx + 1) % n
		}
	case "prev", "down":
		if idx < 0 {
			target = n - 1
		} else {
			target = (idx - 1 + n) % n
		}
	default:
		fail("cycle direction must be next|prev (got %q)", direction)
	}
	cmdFocus(c, fmt.Sprintf("%d", snap.Sessions[target].PID))
}

// cmdAttention jumps to a session that needs the user. Priority mirrors the
// waybar chip colors: sessions waiting on a permission prompt (red) outrank
// idle sessions (orange). Among the top-priority tier, focus cycles — each
// press advances from the currently focused session to the next member of that
// tier, wrapping around, so repeated presses visit every red (or, if there are
// no reds, every orange) in turn. It is a deliberate no-op when every session
// is working (green) or unknown (grey) — there is nowhere worth jumping. Bound
// to mod+Shift+a in Hyprland.
func cmdAttention(c *rpc.Client) {
	snap := mustList(c)

	// The focused session anchors the cycle, so a repeat press steps to the
	// next member of the tier instead of re-focusing the same one. 0 (no PID)
	// when nothing is focused, which pickAttention treats as "outside the tier".
	focusedPID := 0
	for _, s := range snap.Sessions {
		if s.Focused {
			focusedPID = s.PID
			break
		}
	}

	target := pickAttention(snap.Sessions, focusedPID)
	if target == nil {
		return
	}
	cmdFocus(c, fmt.Sprintf("%d", target.PID))
}

// pickAttention returns the next session needing attention, cycling within the
// highest-priority tier. The tier is the permission sessions (red) if any
// exist, otherwise the idle sessions (orange); working and unknown sessions
// never qualify. Members keep snapshot order (oldest-first per state.Snapshot).
// When focusedPID names a tier member, the next member is returned, wrapping
// around — so repeated calls cycle through the whole tier, and a single-member
// tier stays put. When the focused session is outside the tier (or nothing is
// focused), the first member is returned, so one press jumps in. Returns nil
// when no session needs attention.
func pickAttention(sessions []state.Session, focusedPID int) *state.Session {
	tier := topAttentionTier(sessions)
	if len(tier) == 0 {
		return nil
	}

	current := -1
	for i, s := range tier {
		if s.PID == focusedPID {
			current = i
			break
		}
	}
	if current == -1 {
		return tier[0]
	}
	return tier[(current+1)%len(tier)]
}

// topAttentionTier returns the highest-priority group of sessions needing
// attention — all permission sessions if any exist, otherwise all idle
// sessions — in snapshot order. Returns nil when nothing needs attention.
func topAttentionTier(sessions []state.Session) []*state.Session {
	var permission, idle []*state.Session
	for i := range sessions {
		switch sessionStatus(sessions[i]) {
		case "permission":
			permission = append(permission, &sessions[i])
		case "idle":
			idle = append(idle, &sessions[i])
		}
	}
	if len(permission) > 0 {
		return permission
	}
	return idle
}

// sessionStatus normalizes a missing or empty Claude status to "unknown",
// matching switchboard-waybar's rendering.
func sessionStatus(s state.Session) string {
	if s.Claude == nil || s.Claude.Status == "" {
		return "unknown"
	}
	return s.Claude.Status
}

// cmdHook is intended to be invoked from a Claude Code hook command. It
// reads the hook's JSON payload from stdin, looks up its own getppid() to
// identify which Claude process called the hook, and forwards an enrichment
// message to the daemon. Failures are silenced so a broken hook can never
// block Claude Code.
func cmdHook(c *rpc.Client, event string) {
	pid := os.Getppid()
	sessionID := ""
	if body, err := io.ReadAll(os.Stdin); err == nil && len(body) > 0 {
		var payload struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal(body, &payload) == nil {
			sessionID = payload.SessionID
		}
	}
	_ = c.Send(rpc.Request{
		Cmd:       "hook",
		Event:     event,
		PID:       pid,
		SessionID: sessionID,
	})
	var resp rpc.Response
	_ = c.Recv(&resp)
}

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

func mustList(c *rpc.Client) state.Snapshot {
	if err := c.Send(rpc.Request{Cmd: "list"}); err != nil {
		fail("send: %v", err)
	}
	var resp rpc.Response
	if err := c.Recv(&resp); err != nil {
		fail("recv: %v", err)
	}
	if resp.Error != "" {
		fail("%s", resp.Error)
	}
	if resp.Snapshot == nil {
		return state.Snapshot{}
	}
	return *resp.Snapshot
}

func usage() {
	fmt.Fprintln(os.Stderr, strings.TrimSpace(`
usage: switchboard-ctl [flags] <cmd> [args]

commands:
  list                    show session list (human-friendly; --json for raw)
  focus [selector]        focus session: "active" (default), <pid>, <index>,
                            or the unambiguous pid:<n> / idx:<n> forms
  status                  one-line summary
  pick                    emit pid<TAB>label<TAB>ws<TAB>cwd lines for fzf
  cycle next|prev         focus the next/previous session, wrapping
  attention               jump to a session needing attention, cycling within
                            the top tier: permission (red), else idle (orange);
                            repeated presses visit each member in turn;
                            no-op if all working (green) or unknown (grey)
  hook <event>            forward Claude Code hook enrichment (stdin = JSON)
  bottombar [sub]         manage the bottom waybar lifecycle:
                            watch      long-running; show/hide bar with sessions
                            reconcile  one-shot; re-derive bar visibility (F8)
                            stop       kill the bottom bar

flags:
  --socket <path>         daemon socket (default: $XDG_RUNTIME_DIR/switchboard.sock)
  --json                  json output for list
`))
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "switchboard-ctl: "+format+"\n", args...)
	os.Exit(1)
}

func defaultSocketPath() string {
	if x := os.Getenv("XDG_RUNTIME_DIR"); x != "" {
		return filepath.Join(x, "switchboard.sock")
	}
	return fmt.Sprintf("/tmp/switchboard-%d.sock", os.Getuid())
}
