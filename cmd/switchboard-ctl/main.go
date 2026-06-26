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

	sblabel "github.com/tjmisko/switchboard/internal/label"
	"github.com/tjmisko/switchboard/internal/projectname"
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
	// diagnose reads the journal (not the daemon socket), so it runs before the
	// mandatory dial too — it must work even when the daemon is down.
	if args[0] == "diagnose" {
		cmdDiagnose(args[1:])
		return
	}
	// name resolves/edits project abbreviations from the projectname config and
	// the filesystem only — no daemon needed, so it runs before the dial.
	if args[0] == "name" {
		cmdName(args[1:])
		return
	}
	// history reads/manages the on-disk activity log directly (like diagnose), so
	// it runs before the dial — it must work whether or not the daemon is up.
	if args[0] == "history" {
		cmdHistory(args[1:])
		return
	}
	// timeline derives swimlanes + attention stats from the on-disk activity log;
	// also file-only, so it runs before the dial.
	if args[0] == "timeline" {
		cmdTimeline(args[1:])
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
		cmdHook(c, args[1], state.AgentKindClaude)
	case "codex-hook":
		if len(args) < 2 {
			fail("codex-hook requires an event name")
		}
		cmdHook(c, args[1], state.AgentKindCodex)
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
// LABEL is the project-prefixed, de-duplicated session name (see
// internal/label). Intended to be piped into fzf with `--with-nth=2..` so the
// user sees the label but the PID stays in the selected line for the focus call.
func cmdPick(c *rpc.Client) {
	snap := mustList(c)
	cfg := projectname.Load()
	for _, s := range snap.Sessions {
		label := sblabel.Chip(cfg, s)
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
// no reds, every orange) in turn. When nothing needs attention but every
// session is working (green), the green sessions become the tier so the
// shortcut still does something useful — it cycles through the running
// sessions. It is a no-op only when a session is unknown (grey), or when there
// are no sessions at all. Bound to mod+Shift+a in Hyprland.
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
// exist, otherwise the idle sessions (orange), otherwise — only when every
// session is working — the green sessions; an unknown (grey) session is never
// a target and suppresses the all-green fallback. Members keep snapshot order
// (oldest-first per state.Snapshot).
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
// sessions — in snapshot order. As a last resort, when *every* session is
// working (green), all of them form the tier so the shortcut still has
// somewhere to go and instead cycles through the running sessions. A single
// unknown (grey) session suppresses the green tier, so a half-discovered
// snapshot stays a no-op rather than jumping somewhere arbitrary. Returns nil
// when nothing needs attention.
func topAttentionTier(sessions []state.Session) []*state.Session {
	var permission, idle, working []*state.Session
	for i := range sessions {
		switch sessionStatus(sessions[i]) {
		case "permission":
			permission = append(permission, &sessions[i])
		case "idle":
			idle = append(idle, &sessions[i])
		case "working":
			working = append(working, &sessions[i])
		}
	}
	if len(permission) > 0 {
		return permission
	}
	if len(idle) > 0 {
		return idle
	}
	if len(working) > 0 && len(working) == len(sessions) {
		return working
	}
	return nil
}

// sessionStatus normalizes a missing or empty agent status to "unknown",
// matching switchboard-waybar's rendering.
func sessionStatus(s state.Session) string {
	info := s.Enrichment()
	if info == nil || info.Status == "" {
		return "unknown"
	}
	return info.Status
}

// cmdHook is intended to be invoked from a coding agent's hook command (Claude
// Code or Codex — agent selects which). It reads the hook's JSON payload from
// stdin, looks up its own getppid() to identify which agent process called the
// hook, and forwards an enrichment message to the daemon. Both agents share the
// snake_case stdin fields (session_id, transcript_path). Failures are silenced
// so a broken hook can never block the agent.
func cmdHook(c *rpc.Client, event, agent string) {
	pid := os.Getppid()
	sessionID := ""
	transcript := ""
	toolName := ""
	if body, err := io.ReadAll(os.Stdin); err == nil && len(body) > 0 {
		var payload struct {
			SessionID      string `json:"session_id"`
			TranscriptPath string `json:"transcript_path"`
			// tool_name is present on PermissionRequest/PostToolUse payloads. It
			// lets the daemon clear a red chip at hook speed when the approved tool
			// itself completes (see rpc.clearsPermission); absent on other events,
			// which just disables that fast path.
			ToolName string `json:"tool_name"`
		}
		if json.Unmarshal(body, &payload) == nil {
			sessionID = payload.SessionID
			transcript = payload.TranscriptPath
			toolName = payload.ToolName
		}
	}
	_ = c.Send(rpc.Request{
		Cmd:        "hook",
		Event:      event,
		PID:        pid,
		SessionID:  sessionID,
		Transcript: transcript,
		ToolName:   toolName,
		Agent:      agent,
	})
	var resp rpc.Response
	_ = c.Recv(&resp)
}

// cmdName resolves or edits project abbreviations. Subcommands:
//
//	resolve --cwd <dir> --name <name>   print the prefixed, de-duplicated name
//	abbrev  --cwd <dir>                  print the project's canonical abbrev
//	set     <dir> <abbrev>               persist an abbrev for the dir's git root
func cmdName(args []string) {
	if len(args) == 0 {
		fail("name requires a subcommand: resolve|abbrev|set")
	}
	switch args[0] {
	case "resolve":
		fs := flag.NewFlagSet("name resolve", flag.ExitOnError)
		cwd := fs.String("cwd", "", "project directory (default: current)")
		name := fs.String("name", "", "desired session name to prefix")
		_ = fs.Parse(args[1:])
		fmt.Println(projectname.ResolveForDir(projectname.Load(), dirOrCwd(*cwd), *name))
	case "abbrev":
		fs := flag.NewFlagSet("name abbrev", flag.ExitOnError)
		cwd := fs.String("cwd", "", "project directory (default: current)")
		_ = fs.Parse(args[1:])
		fmt.Println(projectname.CanonicalForDir(projectname.Load(), dirOrCwd(*cwd)))
	case "set":
		rest := args[1:]
		if len(rest) < 2 {
			fail("usage: name set <dir> <abbrev>")
		}
		root := projectname.ProjectRoot(rest[0])
		if err := projectname.SetAbbrev(root, rest[1]); err != nil {
			fail("name set: %v", err)
		}
		fmt.Printf("%s -> %s\n", root, projectname.CanonicalForDir(projectname.Load(), root))
	default:
		fail("unknown name subcommand %q (resolve|abbrev|set)", args[0])
	}
}

// dirOrCwd returns dir when non-empty, else the current working directory.
func dirOrCwd(dir string) string {
	if dir != "" {
		return dir
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
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
                            the top tier: permission (red), else idle (orange),
                            else — only if all are green — working sessions;
                            repeated presses visit each member in turn;
                            no-op if any session is unknown (grey)
  name <sub>              project abbreviations: resolve --cwd --name,
                            abbrev --cwd, or set <dir> <abbrev>
  hook <event>            forward Claude Code hook enrichment (stdin = JSON)
  codex-hook <event>      forward Codex hook enrichment (stdin = JSON)
  bottombar [sub]         manage the bottom waybar lifecycle:
                            watch      long-running; show/hide bar with sessions
                            reconcile  one-shot; re-derive bar visibility (F8)
                            stop       kill the bottom bar
  diagnose [flags] [desc] explain a wrong chip color: pull the status-decision
                            log lines for a time window and name the Tuning knob
                            behind each, e.g. diagnose --around 14:32 red stuck.
                            Reads the journal (or --file); needs no daemon.
  history <sub>           the durable activity log (opt-in): path, tail, stat,
                            purge. Reads the on-disk files; needs no daemon.
  timeline [flags]        render the activity log as per-session swimlanes plus
                            attention stats, e.g. timeline --day 2026-06-26.
                            --json emits the structured data; needs no daemon.

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
