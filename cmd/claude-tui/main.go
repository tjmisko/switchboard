// Command claude-tui is the reference renderer: a zero-desktop-dependency live
// view of every Claude Code session, driven entirely by the daemon's RPC
// `subscribe-all` stream. It needs no window manager, no bar, and no terminal
// integration — it works in any terminal, including over SSH — so it is the
// canonical demo of the Observe tier.
//
// The rendering is hand-rolled ANSI (alt-screen + redraw on each snapshot) to
// keep the binary dependency-free. With -once it prints a single plain frame
// and exits, which is handy for scripting and testing.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/tjmisko/switchboard/internal/barlayout"
	"github.com/tjmisko/switchboard/internal/durfmt"
	sblabel "github.com/tjmisko/switchboard/internal/label"
	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/state"
)

func main() {
	socketPath := flag.String("socket", defaultSocketPath(), "daemon socket")
	once := flag.Bool("once", false, "print one plain frame and exit (no alt-screen)")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	home, _ := os.UserHomeDir()

	if *once {
		snap, err := fetchOnce(*socketPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "claude-tui:", err)
			os.Exit(1)
		}
		// One-shot: no loop to amortize a cache over, so nil (read the disk).
		fmt.Print(renderSnapshot(snap, home, false, time.Now(), nil))
		return
	}

	runLive(ctx, *socketPath, home)
}

// fetchOnce connects, subscribes, and returns the first snapshot.
func fetchOnce(socketPath string) (state.Snapshot, error) {
	c, err := rpc.Dial(socketPath)
	if err != nil {
		return state.Snapshot{}, err
	}
	defer c.Close()
	if err := c.Send(rpc.Request{Cmd: "subscribe-all"}); err != nil {
		return state.Snapshot{}, err
	}
	var resp rpc.Response
	if err := c.Recv(&resp); err != nil {
		return state.Snapshot{}, err
	}
	if resp.Snapshot == nil {
		return state.Snapshot{}, nil
	}
	return *resp.Snapshot, nil
}

// runLive holds an alt-screen view, redrawing on every snapshot and reconnecting
// whenever the daemon is unavailable, until the context is cancelled.
func runLive(ctx context.Context, socketPath, home string) {
	fmt.Print(altScreenEnter + hideCursor)
	defer fmt.Print(showCursor + altScreenLeave)

	go func() {
		<-ctx.Done()
		fmt.Print(showCursor + altScreenLeave)
	}()

	// One cache for the process lifetime: every redraw names any blocked writer,
	// and the meta.json behind a name is written once at spawn and never moves.
	writers := &sblabel.NameCache{}

	for ctx.Err() == nil {
		err := streamInto(ctx, socketPath, home, writers)
		if ctx.Err() != nil {
			return
		}
		// Daemon down or stream ended — show a waiting frame and retry.
		drawFrame(waitingFrame(socketPath, err))
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// streamInto subscribes and redraws each snapshot until the connection ends.
func streamInto(ctx context.Context, socketPath, home string, writers *sblabel.NameCache) error {
	c, err := rpc.Dial(socketPath)
	if err != nil {
		return err
	}
	defer c.Close()
	go func() {
		<-ctx.Done()
		c.Close()
	}()
	if err := c.Send(rpc.Request{Cmd: "subscribe-all"}); err != nil {
		return err
	}
	for {
		var resp rpc.Response
		if err := c.Recv(&resp); err != nil {
			return err
		}
		if resp.Snapshot != nil {
			drawFrame(renderSnapshot(*resp.Snapshot, home, true, time.Now(), writers))
		}
	}
}

// --- rendering ---

const (
	altScreenEnter = "\033[?1049h"
	altScreenLeave = "\033[?1049l"
	hideCursor     = "\033[?25l"
	showCursor     = "\033[?25h"
	clearHome      = "\033[H\033[2J"

	colReset  = "\033[0m"
	colGreen  = "\033[32m"
	colYellow = "\033[33m"
	colRed    = "\033[31m"
	colGrey   = "\033[90m"
	colBold   = "\033[1m"

	maxAgentRows      = 32
	maxAgentTreeDepth = 6
)

func drawFrame(body string) { fmt.Print(clearHome + body) }

func waitingFrame(socketPath string, cause error) string {
	return fmt.Sprintf("%sswitchboard%s\r\n\r\nwaiting for daemon at %s …\r\n(%v)\r\n",
		colBold, colReset, socketPath, cause)
}

// statusStyle maps a session status to a glyph and color.
func statusStyle(status string) (glyph, color string) {
	switch status {
	case "working", "delegating":
		// delegating = idle main thread with subagents in flight: work is happening
		// (by proxy), so it shares working's green — no action needed.
		return "●", colGreen
	case "permission":
		return "●", colRed
	case "idle":
		return "●", colYellow
	default: // "" / unknown
		return "○", colGrey
	}
}

// renderSnapshot turns a snapshot into a printable frame. color toggles ANSI so
// the -once/plain path and tests stay readable. Lines end in CRLF so the frame
// renders correctly in a terminal's raw alt-screen.
//
// writers memoizes the blocked-writer name lookup behind a red row's "blocked:"
// annotation, which would otherwise read a meta.json per blocked writer on every
// redraw — and the live loop redraws on every snapshot. A nil *NameCache is legal
// and simply reads the disk each time, which is what the -once path and the tests
// want.
func renderSnapshot(snap state.Snapshot, home string, color bool, now time.Time, writers *sblabel.NameCache) string {
	c := func(code, s string) string {
		if !color {
			return s
		}
		return code + s + colReset
	}

	var b strings.Builder
	n := len(snap.Sessions)
	fmt.Fprintf(&b, "%s · %d session%s · %s\r\n\r\n",
		c(colBold, "switchboard"), n, plural(n), tierSummary(snap.Capabilities))

	if n == 0 {
		b.WriteString(c(colGrey, "no agent sessions") + "\r\n")
		return b.String()
	}

	for _, s := range snap.Sessions {
		status := sessionStatus(s)
		glyph, gcol := statusStyle(status)
		label := c(gcol, fmt.Sprintf("%-11s", status))
		// A suspended (Ctrl-Z'd) process is greyed out wholesale: pause glyph,
		// grey label, grey cwd — its real status is stale until it resumes.
		if s.Suspended {
			glyph, gcol = "⏸", colGrey
			label = c(colGrey, fmt.Sprintf("%-11s", "suspended"))
		}
		focus := " "
		if s.Focused {
			focus = c(colBold, "*")
		}
		ws := ""
		if s.Hyprland != nil && s.Hyprland.Workspace != "" {
			ws = "  ws " + s.Hyprland.Workspace
		}
		// Pad before coloring: ANSI escapes are zero-width on screen but count
		// against %-40s, so wrapping a pre-padded string keeps columns aligned.
		displayCWD := s.CWD
		if !s.Remote {
			displayCWD = abbrevHome(displayCWD, home)
		}
		cwd := fmt.Sprintf("%-40s", displayCWD)
		if s.Suspended {
			cwd = c(colGrey, cwd)
		}
		// How long the session has held this status ("3m", "45s"). Skipped while
		// suspended — the status and its clock are stale until resume.
		dur := ""
		if !s.Suspended {
			if d := durfmt.Since(statusSince(s), now); d != "" {
				dur = "  " + d
			}
		}
		// A red row says a decision is needed; with teammates running it does not
		// say whose. Name the blocked writer(s) so the row is actionable without a
		// trip to the pane. Last on the line, after the fixed-width columns, so it
		// can be any length without disturbing the alignment above it — and painted
		// in the status color rather than the trailing grey, because it is the one
		// thing on the row the user is meant to act on.
		blocked := ""
		if len(barlayout.AgentRows(s.AgentGraph)) == 0 {
			if w := writers.BlockedWriters(s); w != "" {
				blocked = "  blocked: " + w
			}
		}
		process := fmt.Sprintf("pid %d", s.PID)
		if s.Hostname != "" {
			process = fmt.Sprintf("%s/%d", s.Hostname, s.PID)
		}
		fmt.Fprintf(&b, "%s %s %s %s %s%s%s%s\r\n",
			focus, c(gcol, glyph), label, cwd,
			c(colGrey, process), c(colGrey, ws), c(colGrey, dur), c(gcol, blocked))
		renderAgentTree(&b, s, now, c)
	}
	return b.String()
}

// renderAgentTree adds bounded, non-focusable child rows below their owning
// root session. The state projection already carries canonical DFS order; this
// renderer preserves it instead of inferring provider-specific ordering.
func renderAgentTree(b *strings.Builder, s state.Session, now time.Time, paint func(string, string) string) {
	rows := barlayout.AgentRows(s.AgentGraph)
	rows, folded := barlayout.LimitAgentRows(rows, maxAgentRows)
	if len(rows) == 0 && folded == 0 {
		return
	}
	stale := s.Suspended || !s.AgentGraph.Fresh(now)
	for _, row := range rows {
		kind := barlayout.AgentStateKind(row.Node)
		glyph, color := agentStyle(kind)
		stateText := barlayout.AgentStateText(row.Node)
		if stale {
			color = colGrey
			if s.Suspended {
				stateText += " · stale (root suspended)"
			} else {
				stateText += " · stale"
			}
		} else if at := barlayout.AgentStateAt(row.Node); !at.IsZero() {
			stateText += " · " + durfmt.Compact(now.Sub(at))
		}
		if usage := barlayout.AgentUsageText(row.Node); usage != "" {
			stateText += " · " + usage
		}
		name := fmt.Sprintf("%-16s", barlayout.AgentName(row.Node))
		prefix := row.TreePrefix
		if row.Depth > maxAgentTreeDepth {
			prefix = "  " + strings.Repeat("   ", maxAgentTreeDepth-1) + "… "
		}
		fmt.Fprintf(b, "%s%s %s  %s\r\n",
			paint(colGrey, prefix), paint(color, glyph), name, paint(color, stateText))
	}
	if folded > 0 {
		fmt.Fprintf(b, "  %s\r\n", paint(colGrey, fmt.Sprintf("+%d more agents", folded)))
	}
}

func agentStyle(kind string) (glyph, color string) {
	switch kind {
	case "waiting":
		return "●", colRed
	case "active":
		return "●", colGreen
	case "idle":
		return "●", colYellow
	case "error":
		return "!", colRed
	default:
		return "○", colGrey
	}
}

func tierSummary(caps *state.Capabilities) string {
	if caps == nil {
		return "observe"
	}
	tier := "observe"
	if caps.Navigate {
		tier = "navigate"
	}
	return fmt.Sprintf("%s · wm=%s term=%s", tier, caps.WM, caps.Terminal)
}

func sessionStatus(s state.Session) string {
	info := s.Enrichment()
	if info != nil && info.Status != "" {
		return info.Status
	}
	if s.AgentGraph != nil && s.AgentGraph.Summary.Status != "" {
		return s.AgentGraph.Summary.Status
	}
	return "unknown"
}

// statusSince returns the wire timestamp the current status began (nil when no
// enrichment block exists or no status edge has stamped it), for the duration
// counter on each session line.
func statusSince(s state.Session) *time.Time {
	if info := s.Enrichment(); info != nil {
		if info.StatusSinceWire != nil {
			return info.StatusSinceWire
		}
	}
	if s.AgentGraph != nil && !s.AgentGraph.Summary.Since.IsZero() {
		return &s.AgentGraph.Summary.Since
	}
	return nil
}

func abbrevHome(path, home string) string {
	if path == "" {
		return "(unknown cwd)"
	}
	if home != "" && strings.HasPrefix(path, home) {
		return "~" + path[len(home):]
	}
	return path
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func defaultSocketPath() string {
	if x := os.Getenv("XDG_RUNTIME_DIR"); x != "" {
		return filepath.Join(x, "switchboard.sock")
	}
	return fmt.Sprintf("/tmp/switchboard-%d.sock", os.Getuid())
}
