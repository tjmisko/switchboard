package terminal

import "context"

// auto is a Locator that re-selects its concrete backend on every call. The
// daemon autostarts at graphical-session login and routinely wins the boot
// race against the terminal emulator; one-shot detection would then freeze
// terminal="none" for the whole session, leaving every chip stuck on its cwd
// basename. auto re-probes instead, so the Navigate tier — and the window
// titles the chips display — light up on the next reconcile tick once a
// supported terminal appears, with no restart.
//
// Probing is the same cheap, side-effect-free check detect uses (a socket stat
// / runtime-dir read), so paying it per call at the reconcile cadence is fine.
type auto struct {
	candidates []Locator
}

// NewAuto returns a self-redetecting terminal locator that composes whichever
// backends are live, mirroring detect's auto precedence: tmux (innermost — it
// owns the pane a claude actually runs in) then wezterm (the outer window).
func NewAuto() Locator {
	return auto{candidates: []Locator{NewTmux(), NewWezterm()}}
}

// current resolves the currently-live backend set into a single Locator,
// exactly as a one-shot detect would for this instant: none, the sole live
// backend, or a chain when several compose (per-session nesting means the right
// one varies by tty).
func (a auto) current() Locator {
	var live []Locator
	for _, c := range a.candidates {
		if c.Available() {
			live = append(live, c)
		}
	}
	switch len(live) {
	case 0:
		return NewNone()
	case 1:
		return live[0]
	default:
		return NewChain(live...)
	}
}

func (a auto) Name() string { return a.current().Name() }

func (a auto) Available() bool { return a.current().Name() != "none" }

func (a auto) Locate(ctx context.Context, tty string) (*PaneRef, error) {
	return a.current().Locate(ctx, tty)
}

func (a auto) Activate(ctx context.Context, ref *PaneRef) error {
	return a.current().Activate(ctx, ref)
}
