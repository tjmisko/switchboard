package conformance_test

import (
	"context"
	"testing"

	"github.com/tjmisko/switchboard/internal/conformance"
	"github.com/tjmisko/switchboard/internal/wm"
)

// wmBackend is the common surface every WM backend exposes for the conformance
// suite: the production Manager methods used by the contract plus the
// normalization/translation helpers (which are concrete-type methods, not on
// the Manager interface). Both *wm.Hyprland and *wm.I3 satisfy it; the X11
// backend will too.
type wmBackend interface {
	Available() bool
	Clients(context.Context) ([]wm.Window, error)
	ActiveWindow(context.Context) (string, error)
	Focus(context.Context, string) error
	NormalizeEventAddress(string) string
	RawForm(string) string
	CanonicalEvents() []string
	TranslateEvent(string) (string, bool)
}

// wmAdapter wraps any wmBackend behind the neutral conformance.Manager (the
// only real work is converting []wm.Window to []conformance.Window). The
// normalization assertions drive the backend's REAL helpers, so the contract
// verifies each seam owns its address quirk.
type wmAdapter struct{ b wmBackend }

func (a wmAdapter) Available() bool { return a.b.Available() }

func (a wmAdapter) Clients(ctx context.Context) ([]conformance.Window, error) {
	ws, err := a.b.Clients(ctx)
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

func (a wmAdapter) ActiveWindow(ctx context.Context) (string, error) { return a.b.ActiveWindow(ctx) }

func (a wmAdapter) Focus(ctx context.Context, ref string) error { return a.b.Focus(ctx, ref) }

func (a wmAdapter) NormalizeEventAddress(raw string) string { return a.b.NormalizeEventAddress(raw) }

func (a wmAdapter) RawForm(address string) string { return a.b.RawForm(address) }

func (a wmAdapter) CanonicalEvents() []string { return a.b.CanonicalEvents() }

func (a wmAdapter) TranslateEvent(rawName string) (string, bool) { return a.b.TranslateEvent(rawName) }

func TestHyprlandManagerConformance(t *testing.T) {
	conformance.RunManagerContract(t, wmAdapter{b: wm.NewHyprland()})
}

func TestI3ManagerConformance(t *testing.T) {
	conformance.RunManagerContract(t, wmAdapter{b: wm.NewI3()})
}

func TestX11ManagerConformance(t *testing.T) {
	conformance.RunManagerContract(t, wmAdapter{b: wm.NewX11()})
}
