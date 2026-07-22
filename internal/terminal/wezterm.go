package terminal

import (
	"context"
	"net/url"
	"strings"

	"github.com/tjmisko/switchboard/internal/wezterm"
)

// weztermLocator wraps the internal/wezterm CLI driver behind the neutral
// Locator. wezterm is not OS-specific (it drives the `wezterm cli` binary), so
// no build tag is needed.
type weztermLocator struct{}

// NewWezterm returns the wezterm terminal locator.
func NewWezterm() Locator { return weztermLocator{} }

func (weztermLocator) Name() string { return "wezterm" }

func (weztermLocator) Available() bool {
	ms, err := wezterm.Muxes()
	return err == nil && len(ms) > 0
}

func (weztermLocator) Locate(ctx context.Context, tty string) (*PaneRef, error) {
	p, err := wezterm.FindByTTY(ctx, tty)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, nil
	}
	return &PaneRef{
		Backend:     "wezterm",
		Mux:         p.MuxPID,
		MuxSocket:   p.MuxSocket,
		PaneID:      p.PaneID,
		TabID:       p.TabID,
		WindowID:    p.WindowID,
		Title:       p.Title,
		WindowTitle: p.WindowTitle,
		TTY:         p.TTYName,
		CWD:         decodeCWD(p.CWDURL),
	}, nil
}

func (weztermLocator) Activate(ctx context.Context, ref *PaneRef) error {
	return wezterm.ActivatePane(ctx, ref.MuxSocket, ref.PaneID)
}

// decodeCWD turns wezterm's file:// cwd URL into a filesystem path. Owning cwd
// decoding is the terminal seam's job (the URL is wezterm's). Returns "" for a
// non-file URL, a malformed percent-escape, or — per decisions.md #8 — a URL
// with no path component (a bare "file://host" no longer leaks the host as the
// path, the Phase-0 ⚠ characterization this fix flips).
func decodeCWD(cwdURL string) string {
	rest, ok := strings.CutPrefix(cwdURL, "file://")
	if !ok {
		return ""
	}
	idx := strings.IndexByte(rest, '/')
	if idx < 0 {
		return "" // no path component, e.g. "file://host"
	}
	decoded, err := url.PathUnescape(rest[idx:])
	if err != nil {
		return ""
	}
	return strings.TrimRight(decoded, "/")
}
