package conformance_test

import (
	"context"
	"testing"

	"github.com/tjmisko/switchboard/internal/conformance"
	"github.com/tjmisko/switchboard/internal/wezterm"
)

// weztermLocator is the thin adapter wrapping the existing wezterm concretes
// behind the neutral conformance.Locator interface. Phase 1's internal/terminal
// wezterm backend replaces it and reuses RunLocatorContract; the tmux backend
// adopts the same contract in Phase 3.
type weztermLocator struct{}

func (weztermLocator) Available() bool {
	ms, err := wezterm.Muxes()
	return err == nil && len(ms) > 0
}

func (weztermLocator) Locate(ctx context.Context, tty string) (*conformance.Pane, error) {
	p, err := wezterm.FindByTTY(ctx, tty)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, nil
	}
	return &conformance.Pane{
		Mux:         p.MuxPID,
		PaneID:      p.PaneID,
		TTY:         p.TTYName,
		WindowTitle: p.WindowTitle,
	}, nil
}

func (weztermLocator) SomeTTY(ctx context.Context) (string, bool) {
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
	conformance.RunLocatorContract(t, weztermLocator{})
}
