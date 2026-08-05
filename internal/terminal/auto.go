package terminal

import (
	"context"
	"fmt"
)

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

// Snapshot delegates to the currently-live backend, so the batch path inherits
// the same per-call re-detection — a terminal that wins the boot race after the
// daemon starts joins the fast path on the next reconcile tick, not on restart.
//
// current() hands back a plain Locator that may or may not carry the fast path,
// so this applies SnapshotOrNil's upgrade/degrade rule itself. When the live
// backend has no batch path it must ERROR rather than return an empty map:
// SnapshotOrNil then reports "no usable batch answer" and the caller falls back
// to Locate, whereas an empty map would be read as "no tty owns a pane" and
// blank every chip.
func (a auto) Snapshot(ctx context.Context) (map[string]PaneRef, error) {
	cur := a.current()
	s, ok := cur.(Snapshotter)
	if !ok {
		return nil, fmt.Errorf("terminal: backend %q provides no batch snapshot path", cur.Name())
	}
	return s.Snapshot(ctx)
}
