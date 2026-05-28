package conformance_test

import (
	"context"
	"testing"

	"github.com/tjmisko/switchboard/internal/conformance"
	"github.com/tjmisko/switchboard/internal/wm"
)

// wmManager wraps the Phase-1 internal/wm Hyprland backend behind the neutral
// conformance.Manager interface. The normalization/translation assertions drive
// the backend's REAL helpers (not a reimplementation), so the contract verifies
// the seam owns the 0x-prefix quirk (decisions.md #1). The sway/i3/X11 backends
// adopt the same contract in Phase 2.
type wmManager struct{ h *wm.Hyprland }

func (a wmManager) Available() bool { return a.h.Available() }

func (a wmManager) Clients(ctx context.Context) ([]conformance.Window, error) {
	ws, err := a.h.Clients(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]conformance.Window, 0, len(ws))
	for _, w := range ws {
		out = append(out, conformance.Window{
			Address:   w.Address,
			PID:       w.PID,
			Title:     w.Title,
			Workspace: w.Workspace,
		})
	}
	return out, nil
}

func (a wmManager) ActiveWindow(ctx context.Context) (string, error) { return a.h.ActiveWindow(ctx) }

func (a wmManager) Focus(ctx context.Context, ref string) error { return a.h.Focus(ctx, ref) }

func (a wmManager) NormalizeEventAddress(raw string) string { return a.h.NormalizeEventAddress(raw) }

func (a wmManager) RawForm(address string) string { return a.h.RawForm(address) }

func (a wmManager) CanonicalEvents() []string { return a.h.CanonicalEvents() }

func (a wmManager) TranslateEvent(rawName string) (string, bool) { return a.h.TranslateEvent(rawName) }

func TestHyprlandManagerConformance(t *testing.T) {
	conformance.RunManagerContract(t, wmManager{h: wm.NewHyprland()})
}
