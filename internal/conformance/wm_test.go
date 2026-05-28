package conformance_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/tjmisko/switchboard/internal/conformance"
	"github.com/tjmisko/switchboard/internal/hyprland"
)

// wmManager is the thin adapter wrapping the existing Hyprland concretes behind
// the neutral conformance.Manager interface. Phase 1's internal/wm hyprland
// backend replaces it and reuses RunManagerContract; the sway/i3/X11 backends
// adopt the same contract in Phase 2.
type wmManager struct{}

func (wmManager) Available() bool { return os.Getenv("HYPRLAND_INSTANCE_SIGNATURE") != "" }

func (wmManager) Clients(ctx context.Context) ([]conformance.Window, error) {
	cs, err := hyprland.Clients(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]conformance.Window, 0, len(cs))
	for _, c := range cs {
		out = append(out, conformance.Window{
			Address:   c.Address,
			PID:       c.PID,
			Title:     c.Title,
			Workspace: c.Workspace.Name,
		})
	}
	return out, nil
}

func (wmManager) ActiveWindow(ctx context.Context) (string, error) {
	return hyprland.ActiveWindowAddress(ctx)
}

func (wmManager) Focus(ctx context.Context, ref string) error {
	return hyprland.Dispatch(ctx, "focuswindow address:"+ref)
}

// NormalizeEventAddress mirrors cmd/switchboard's event boundary: socket2 emits
// addresses without the 0x prefix that hyprctl clients reports.
func (wmManager) NormalizeEventAddress(raw string) string { return "0x" + raw }

func (wmManager) RawForm(address string) string { return strings.TrimPrefix(address, "0x") }

func (wmManager) CanonicalEvents() []string {
	return []string{"closewindow", "activewindowv2", "movewindowv2", "windowtitlev2", "openwindow"}
}

func (m wmManager) TranslateEvent(rawName string) (string, bool) {
	for _, name := range m.CanonicalEvents() {
		if name == rawName {
			return name, true
		}
	}
	return "", false
}

func TestHyprlandManagerConformance(t *testing.T) {
	conformance.RunManagerContract(t, wmManager{})
}
