package terminal

import (
	"context"
	"fmt"
	"strings"
)

// chain composes several locators, trying each in order until one owns the tty.
// It exists because terminal nesting is per-session, not global: a claude inside
// tmux has the tmux pane's tty while a sibling claude in a bare wezterm window
// has wezterm's — so the daemon tries the innermost multiplexer (tmux) first,
// then the outer terminal (wezterm). Activate routes back to the locator that
// produced the ref via PaneRef.Backend.
type chain struct{ locs []Locator }

// NewChain composes locators in priority order (innermost first). With a single
// locator, prefer using it directly; detect only builds a chain for 2+.
func NewChain(locs ...Locator) Locator { return chain{locs: locs} }

func (c chain) Name() string {
	if len(c.locs) == 0 {
		return "none"
	}
	names := make([]string, 0, len(c.locs))
	for _, l := range c.locs {
		names = append(names, l.Name())
	}
	return strings.Join(names, "+")
}

func (c chain) Available() bool {
	for _, l := range c.locs {
		if l.Available() {
			return true
		}
	}
	return false
}

// Locate returns the first locator's pane that owns the tty. A locator that
// errors does not blank the others (the contract's "one failing endpoint
// doesn't blank healthy ones"); the first error is only surfaced when no
// locator found a pane.
func (c chain) Locate(ctx context.Context, tty string) (*PaneRef, error) {
	var firstErr error
	for _, l := range c.locs {
		pane, err := l.Locate(ctx, tty)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if pane != nil {
			return pane, nil
		}
	}
	return nil, firstErr
}

func (c chain) Activate(ctx context.Context, ref *PaneRef) error {
	for _, l := range c.locs {
		if l.Name() == ref.Backend {
			return l.Activate(ctx, ref)
		}
	}
	return fmt.Errorf("terminal: no backend %q to activate pane", ref.Backend)
}
