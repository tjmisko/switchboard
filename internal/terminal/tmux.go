package terminal

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// tmuxLocator drives the `tmux` CLI. A claude running inside tmux has its
// controlling tty set to the tmux pane's pty, so the portable tty join still
// works: Locate matches pane_tty, and Activate selects that pane.
//
// Caveat (the tmux↔WM bridge, plan 3.3): the WM window that hosts a tmux pane
// belongs to the tmux *client's* terminal emulator, not the in-pane process, so
// this backend focuses the pane within tmux but does not raise the WM window.
// Raising it requires chaining pane → client tty → WM window, deferred.
type tmuxLocator struct{}

// NewTmux returns the tmux terminal locator.
func NewTmux() Locator { return tmuxLocator{} }

func (tmuxLocator) Name() string { return "tmux" }

// Available reports whether a tmux server is reachable, cheaply and without
// spawning: $TMUX is set inside tmux, otherwise the default server socket
// ($TMUX_TMPDIR or /tmp)/tmux-<uid>/default exists.
func (tmuxLocator) Available() bool {
	if os.Getenv("TMUX") != "" {
		return true
	}
	dir := os.Getenv("TMUX_TMPDIR")
	if dir == "" {
		dir = "/tmp"
	}
	sock := filepath.Join(dir, fmt.Sprintf("tmux-%d", os.Getuid()), "default")
	_, err := os.Stat(sock)
	return err == nil
}

func (l tmuxLocator) Locate(ctx context.Context, tty string) (*PaneRef, error) {
	// No reachable server → owns no panes. Checking first avoids spawning tmux
	// (or erroring where tmux isn't installed, e.g. CI) on every Locate.
	if !l.Available() {
		return nil, nil
	}
	out, noServer, err := tmuxRun(ctx, "list-panes", "-a", "-F", tmuxPaneFormat)
	if noServer {
		return nil, nil // no tmux server → owns no panes
	}
	if err != nil {
		return nil, err
	}
	for _, p := range parseTmuxPanes(out) {
		if p.TTY == tty {
			return &PaneRef{
				Backend:     "tmux",
				Handle:      p.PaneID,
				WindowTitle: p.WindowName,
				TTY:         p.TTY,
				CWD:         p.CWD,
			}, nil
		}
	}
	return nil, nil
}

func (tmuxLocator) Activate(ctx context.Context, ref *PaneRef) error {
	if ref.Handle == "" {
		return fmt.Errorf("tmux: pane ref has no handle")
	}
	// select-window makes the pane's window active in its session; select-pane
	// makes the pane active in that window. Together they bring the pane to the
	// foreground for any client attached to the session.
	if _, _, err := tmuxRun(ctx, "select-window", "-t", ref.Handle); err != nil {
		return err
	}
	if _, _, err := tmuxRun(ctx, "select-pane", "-t", ref.Handle); err != nil {
		return err
	}
	return nil
}

// tmuxPaneFormat lists the fields Locate needs, tab-separated.
const tmuxPaneFormat = "#{pane_tty}\t#{pane_id}\t#{pane_current_path}\t#{window_name}"

type tmuxPane struct {
	TTY        string
	PaneID     string
	CWD        string
	WindowName string
}

// parseTmuxPanes decodes the tab-separated `list-panes` output. Lines with too
// few fields are skipped.
func parseTmuxPanes(out []byte) []tmuxPane {
	var panes []tmuxPane
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Split(sc.Text(), "\t")
		if len(f) < 4 {
			continue
		}
		panes = append(panes, tmuxPane{TTY: f[0], PaneID: f[1], CWD: f[2], WindowName: f[3]})
	}
	return panes
}

// tmuxRun executes a tmux command. noServer is true when tmux reports no running
// server (a normal "nothing to locate" outcome, not an error).
func tmuxRun(ctx context.Context, args ...string) (out []byte, noServer bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tmux", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if strings.Contains(stderr.String(), "no server running") {
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("tmux %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), false, nil
}
