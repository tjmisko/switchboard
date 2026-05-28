package conformance_test

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/tjmisko/switchboard/internal/conformance"
	"github.com/tjmisko/switchboard/internal/terminal"
	"github.com/tjmisko/switchboard/internal/wezterm"
)

// terminalLocator wraps the Phase-1 internal/terminal wezterm backend behind
// the neutral conformance.Locator. The tmux backend adopts the same contract in
// Phase 3. SomeTTY is a live-fixture helper (not part of the production
// Locator), so it reaches into the underlying wezterm driver to enumerate panes.
type terminalLocator struct{ l terminal.Locator }

func (a terminalLocator) Available() bool { return a.l.Available() }

func (a terminalLocator) Locate(ctx context.Context, tty string) (*conformance.Pane, error) {
	p, err := a.l.Locate(ctx, tty)
	if err != nil || p == nil {
		return nil, err
	}
	return &conformance.Pane{
		Mux:         p.Mux,
		PaneID:      p.PaneID,
		TTY:         p.TTY,
		WindowTitle: p.WindowTitle,
	}, nil
}

func (terminalLocator) SomeTTY(ctx context.Context) (string, bool) {
	panes, err := wezterm.List(ctx)
	if err != nil {
		return "", false
	}
	for _, p := range panes {
		if p.TTYName != "" {
			return p.TTYName, true
		}
	}
	return "", false
}

func TestWeztermLocatorConformance(t *testing.T) {
	conformance.RunLocatorContract(t, terminalLocator{l: terminal.NewWezterm()})
}

// tmuxLocatorFixture wraps the tmux backend; SomeTTY enumerates live tmux panes.
type tmuxLocatorFixture struct{ l terminal.Locator }

func (a tmuxLocatorFixture) Available() bool { return a.l.Available() }

func (a tmuxLocatorFixture) Locate(ctx context.Context, tty string) (*conformance.Pane, error) {
	p, err := a.l.Locate(ctx, tty)
	if err != nil || p == nil {
		return nil, err
	}
	// tmux's stable id is the pane handle ("%3"); map its numeric part into the
	// neutral Pane's PaneID. tmux pane ids are 0-based, and the contract treats
	// (mux,pane)=(0,0) as "no identity", so offset by 1 to keep "%0" identifiable.
	id, _ := strconv.Atoi(strings.TrimPrefix(p.Handle, "%"))
	return &conformance.Pane{PaneID: id + 1, TTY: p.TTY, WindowTitle: p.WindowTitle}, nil
}

func (tmuxLocatorFixture) SomeTTY(ctx context.Context) (string, bool) {
	out, err := exec.Command("tmux", "list-panes", "-a", "-F", "#{pane_tty}").Output()
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			return line, true
		}
	}
	return "", false
}

func TestTmuxLocatorConformance(t *testing.T) {
	conformance.RunLocatorContract(t, tmuxLocatorFixture{l: terminal.NewTmux()})
}
