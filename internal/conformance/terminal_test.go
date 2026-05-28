package conformance_test

import (
	"context"
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
