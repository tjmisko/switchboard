// Command claude-tui is the reference renderer: a zero-desktop-dependency live
// view of every Claude Code session, driven entirely by the daemon's RPC
// `subscribe` stream. It needs no window manager, no bar, and no terminal
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

	"github.com/tjmisko/switchboard/internal/durfmt"
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
		fmt.Print(renderSnapshot(snap, home, false, time.Now()))
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
	if err := c.Send(rpc.Request{Cmd: "subscribe"}); err != nil {
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

	for ctx.Err() == nil {
		err := streamInto(ctx, socketPath, home)
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
func streamInto(ctx context.Context, socketPath, home string) error {
	c, err := rpc.Dial(socketPath)
	if err != nil {
		return err
	}
	defer c.Close()
	go func() {
		<-ctx.Done()
		c.Close()
	}()
	if err := c.Send(rpc.Request{Cmd: "subscribe"}); err != nil {
		return err
	}
	for {
		var resp rpc.Response
		if err := c.Recv(&resp); err != nil {
			return err
		}
		if resp.Snapshot != nil {
			drawFrame(renderSnapshot(*resp.Snapshot, home, true, time.Now()))
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
func renderSnapshot(snap state.Snapshot, home string, color bool, now time.Time) string {
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
		b.WriteString(c(colGrey, "no claude sessions") + "\r\n")
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
		cwd := fmt.Sprintf("%-40s", abbrevHome(s.CWD, home))
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
		fmt.Fprintf(&b, "%s %s %s %s %s%s%s\r\n",
			focus, c(gcol, glyph), label, cwd,
			c(colGrey, fmt.Sprintf("pid %d", s.PID)), c(colGrey, ws), c(colGrey, dur))
	}
	return b.String()
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
	if info == nil || info.Status == "" {
		return "unknown"
	}
	return info.Status
}

// statusSince returns the wire timestamp the current status began (nil when no
// enrichment block exists or no status edge has stamped it), for the duration
// counter on each session line.
func statusSince(s state.Session) *time.Time {
	if info := s.Enrichment(); info != nil {
		return info.StatusSinceWire
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
