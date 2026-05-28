package wm

import (
	"context"
	"os"
	"strings"

	"github.com/tjmisko/switchboard/internal/hyprland"
)

// Hyprland wraps the internal/hyprland IPC driver behind the neutral Manager.
// It is the concrete type (not the Manager interface) so the conformance suite
// can drive its normalization/translation helpers directly; NewHyprland returns
// it typed as Manager for the daemon.
type Hyprland struct{}

// NewHyprland returns the Hyprland window-manager backend.
func NewHyprland() *Hyprland { return &Hyprland{} }

func (*Hyprland) Name() string { return "hyprland" }

func (*Hyprland) Available() bool { return os.Getenv("HYPRLAND_INSTANCE_SIGNATURE") != "" }

func (*Hyprland) Clients(ctx context.Context) ([]Window, error) {
	cs, err := hyprland.Clients(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Window, 0, len(cs))
	for _, c := range cs {
		out = append(out, Window{
			Address:     c.Address,
			PID:         c.PID,
			Title:       c.Title,
			Workspace:   c.Workspace.Name,
			WorkspaceID: c.Workspace.ID,
		})
	}
	return out, nil
}

func (*Hyprland) ActiveWindow(ctx context.Context) (string, error) {
	return hyprland.ActiveWindowAddress(ctx)
}

func (*Hyprland) Focus(ctx context.Context, ref string) error {
	return hyprland.FocusWindow(ctx, ref)
}

// Subscribe translates Hyprland's raw socket2 events into neutral Events,
// normalizing each address into Clients() form so the daemon never reconstructs
// the 0x prefix itself (decisions.md #1).
func (h *Hyprland) Subscribe(ctx context.Context) (<-chan Event, error) {
	raw, err := hyprland.Subscribe(ctx)
	if err != nil {
		return nil, err
	}
	out := make(chan Event, 32)
	go func() {
		defer close(out)
		for ev := range raw {
			kind, ok := rawEventKinds[ev.Name]
			if !ok {
				continue
			}
			e := Event{Kind: kind}
			if kind != EventLayoutChanged {
				e.Address = h.NormalizeEventAddress(ev.Data)
			}
			select {
			case out <- e:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// rawEventKinds maps the raw socket2 event names the backend recognizes to the
// neutral kind they translate to.
var rawEventKinds = map[string]EventKind{
	"closewindow":    EventWindowClosed,
	"activewindowv2": EventFocusChanged,
	"movewindowv2":   EventLayoutChanged,
	"windowtitlev2":  EventLayoutChanged,
	"openwindow":     EventLayoutChanged,
}

// NormalizeEventAddress converts a socket2 event address (emitted WITHOUT the
// 0x prefix) into the 0x-prefixed form `hyprctl clients` reports, so an event
// address compares equal to a Clients() address. This is the seam's ownership
// of the Phase-0 ⚠ #1 cross-layer contract.
func (*Hyprland) NormalizeEventAddress(raw string) string {
	if strings.HasPrefix(raw, "0x") {
		return raw
	}
	return "0x" + raw
}

// RawForm is the inverse of NormalizeEventAddress for a Clients() address. Used
// by the conformance suite to synthesize the event-stream form of an address.
func (*Hyprland) RawForm(address string) string {
	return strings.TrimPrefix(address, "0x")
}

// CanonicalEvents lists the raw socket2 event names the backend handles.
func (*Hyprland) CanonicalEvents() []string {
	return []string{"closewindow", "activewindowv2", "movewindowv2", "windowtitlev2", "openwindow"}
}

// TranslateEvent reports whether a raw socket2 event name is one the backend
// handles, returning it unchanged when so. (The neutral kind it maps to is an
// internal detail of Subscribe; this method exposes the recognized vocabulary
// for the conformance contract.)
func (*Hyprland) TranslateEvent(rawName string) (string, bool) {
	if _, ok := rawEventKinds[rawName]; ok {
		return rawName, true
	}
	return "", false
}
