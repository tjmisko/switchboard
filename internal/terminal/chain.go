package terminal

import (
	"context"
	"errors"
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
	pane, err := c.locateOwn(ctx, tty)
	if pane == nil {
		return nil, err
	}
	bridged := *pane
	c.bridgeByLocate(ctx, &bridged, maxHostHops)
	return &bridged, nil
}

// locateOwn is Locate without the host bridge: the pane as its owning backend
// reports it.
func (c chain) locateOwn(ctx context.Context, tty string) (*PaneRef, error) {
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

// maxHostHops bounds how many client hops a bridge follows (herdr inside a
// terminal inside herdr …). Real nesting is one or two deep; the bound only
// guarantees termination should a HostTTY ever point back into its own chain.
const maxHostHops = 4

// bridgeByLocate gives a pane drawn by a separate client (HostTTY set) the
// window join of the outer pane hosting that client, resolved by Locate. It
// matches bridgeFrom, the batch path's version, hop for hop.
func (c chain) bridgeByLocate(ctx context.Context, ref *PaneRef, hops int) {
	if ref.HostTTY == "" || hops == 0 {
		return
	}
	host, err := c.locateOwn(ctx, ref.HostTTY)
	if err != nil || host == nil {
		return
	}
	outer := *host
	c.bridgeByLocate(ctx, &outer, hops-1)
	copyWindowJoin(ref, outer)
}

// bridgeFrom is bridgeByLocate over an already-merged snapshot.
func bridgeFrom(panes map[string]PaneRef, ref *PaneRef, hops int) {
	if ref.HostTTY == "" || hops == 0 {
		return
	}
	host, ok := panes[ref.HostTTY]
	if !ok {
		return
	}
	bridgeFrom(panes, &host, hops-1)
	copyWindowJoin(ref, host)
}

// copyWindowJoin copies the fields the mapping layer joins a WM window on. The
// pane keeps its own backend, handle, tty and title: focus still routes to it.
func copyWindowJoin(ref *PaneRef, host PaneRef) {
	ref.Mux = host.Mux
	ref.WindowID = host.WindowID
	ref.WindowTitle = host.WindowTitle
}

// Snapshot merges the children's pane sets while preserving both of Locate's
// contracts.
//
// Precedence: the chain is ordered innermost-first (tmux owns the pty a claude
// actually runs in; wezterm merely hosts the tmux client), and Locate returns
// the FIRST locator that owns the tty. So the merge walks in chain order and
// refuses to overwrite a tty an earlier locator already claimed — otherwise a
// nested session would batch-resolve to the outer wezterm pane and Navigate
// would focus the wrong window.
//
// Errors: a locator that errors does not blank the healthy ones (the contract's
// "one failing endpoint doesn't blank healthy ones"); the first error surfaces
// only when nothing was found at all. Note the residual asymmetry this leaves
// versus Locate: when one child errors and another has panes, the merged set is
// returned as a success with the erroring child's ttys simply absent, where
// Locate would have reported that child's error for those ttys specifically.
// The caller sees a miss rather than an error — the same shape as the seam's
// standing "retry next tick" answer to a transient unknown (cf. decisions.md #4,
// which preserves that discipline for an ambiguous WM match).
func (c chain) Snapshot(ctx context.Context) (map[string]PaneRef, error) {
	merged := make(map[string]PaneRef)
	var firstErr error
	for _, l := range c.locs {
		s, ok := l.(Snapshotter)
		if !ok {
			// The batch fast-path is optional per backend, but a chain cannot
			// answer partially: a merged map missing this child's panes is
			// indistinguishable from "those ttys own no pane", so every session
			// on that backend would silently resolve to nothing. Refuse the
			// whole batch instead — SnapshotOrNil turns this into the per-session
			// Locate fallback, which still routes through every child.
			return nil, fmt.Errorf("terminal: chain member %q provides no batch snapshot path", l.Name())
		}
		panes, err := s.Snapshot(ctx)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for tty, ref := range panes {
			if _, claimed := merged[tty]; claimed {
				continue // an inner locator already owns this tty
			}
			merged[tty] = ref
		}
	}
	if len(merged) == 0 && firstErr != nil {
		return nil, firstErr
	}
	// Bridge against the unbridged merge, so the result cannot depend on map
	// iteration order.
	bridged := make(map[string]PaneRef, len(merged))
	for tty, ref := range merged {
		bridgeFrom(merged, &ref, maxHostHops)
		bridged[tty] = ref
	}
	return bridged, nil
}

// Activate focuses the pane in its own backend, then walks out through its host
// clients activating each outer pane, so a herdr client in a WezTerm tab lands
// on that tab too. An outer terminal with no step finer than its window
// (ErrUnsupported) or no longer hosting the client is skipped: the pane itself
// was focused, and the WM step raises the window.
func (c chain) Activate(ctx context.Context, ref *PaneRef) error {
	if err := c.activateOwn(ctx, ref); err != nil {
		return err
	}
	host := ref.HostTTY
	for hops := maxHostHops; host != "" && hops > 0; hops-- {
		outer, err := c.locateOwn(ctx, host)
		if err != nil || outer == nil {
			return nil
		}
		if err := c.activateOwn(ctx, outer); err != nil && !errors.Is(err, ErrUnsupported) {
			return fmt.Errorf("terminal: activate host pane of %s: %w", ref.Backend, err)
		}
		host = outer.HostTTY
	}
	return nil
}

func (c chain) activateOwn(ctx context.Context, ref *PaneRef) error {
	for _, l := range c.locs {
		if l.Name() == ref.Backend {
			return l.Activate(ctx, ref)
		}
	}
	return fmt.Errorf("terminal: no backend %q to activate pane", ref.Backend)
}
