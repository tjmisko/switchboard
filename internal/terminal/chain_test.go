package terminal

import (
	"context"
	"errors"
	"testing"
)

// fakeLocator is a programmable Locator for chain tests.
type fakeLocator struct {
	name      string
	available bool
	pane      *PaneRef // returned by Locate when tty == wantTTY
	wantTTY   string
	locateErr error
	activated *PaneRef // records the ref passed to Activate
}

func (f *fakeLocator) Name() string    { return f.name }
func (f *fakeLocator) Available() bool { return f.available }
func (f *fakeLocator) Locate(_ context.Context, tty string) (*PaneRef, error) {
	if f.locateErr != nil {
		return nil, f.locateErr
	}
	if f.pane != nil && tty == f.wantTTY {
		return f.pane, nil
	}
	return nil, nil
}
func (f *fakeLocator) Activate(_ context.Context, ref *PaneRef) error {
	f.activated = ref
	return nil
}

func TestChainLocateFirstMatchWins(t *testing.T) {
	inner := &fakeLocator{name: "tmux", available: true, wantTTY: "/dev/pts/5", pane: &PaneRef{Backend: "tmux", TTY: "/dev/pts/5"}}
	outer := &fakeLocator{name: "wezterm", available: true, wantTTY: "/dev/pts/5", pane: &PaneRef{Backend: "wezterm", TTY: "/dev/pts/5"}}
	c := NewChain(inner, outer)

	pane, err := c.Locate(context.Background(), "/dev/pts/5")
	if err != nil || pane == nil {
		t.Fatalf("Locate = (%v, %v), want a pane", pane, err)
	}
	if pane.Backend != "tmux" {
		t.Errorf("Locate matched %q, want the innermost (tmux) first", pane.Backend)
	}
}

// A locator that errors must not blank a later locator that owns the tty.
func TestChainErrorDoesNotBlankHealthy(t *testing.T) {
	broken := &fakeLocator{name: "tmux", available: true, locateErr: errors.New("boom")}
	healthy := &fakeLocator{name: "wezterm", available: true, wantTTY: "/dev/pts/9", pane: &PaneRef{Backend: "wezterm", TTY: "/dev/pts/9"}}
	c := NewChain(broken, healthy)

	pane, err := c.Locate(context.Background(), "/dev/pts/9")
	if err != nil || pane == nil || pane.Backend != "wezterm" {
		t.Fatalf("Locate = (%v, %v), want the healthy wezterm pane", pane, err)
	}
}

// An unknown tty resolves to no pane and surfaces no error when no locator errs.
func TestChainUnknownTTYNoError(t *testing.T) {
	a := &fakeLocator{name: "tmux", available: true, wantTTY: "/dev/pts/5"}
	b := &fakeLocator{name: "wezterm", available: true, wantTTY: "/dev/pts/5"}
	pane, err := NewChain(a, b).Locate(context.Background(), "/dev/pts/999")
	if pane != nil || err != nil {
		t.Errorf("Locate(unknown) = (%v, %v), want (nil, nil)", pane, err)
	}
}

func TestChainActivateRoutesByBackend(t *testing.T) {
	tmux := &fakeLocator{name: "tmux", available: true}
	wez := &fakeLocator{name: "wezterm", available: true}
	c := NewChain(tmux, wez)

	ref := &PaneRef{Backend: "wezterm", Handle: "x"}
	if err := c.Activate(context.Background(), ref); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if wez.activated != ref {
		t.Error("Activate did not route to the wezterm backend")
	}
	if tmux.activated != nil {
		t.Error("Activate wrongly called the tmux backend")
	}

	if err := c.Activate(context.Background(), &PaneRef{Backend: "kitty"}); err == nil {
		t.Error("Activate with an unknown backend = nil err, want error")
	}
}

func TestChainName(t *testing.T) {
	c := NewChain(&fakeLocator{name: "tmux"}, &fakeLocator{name: "wezterm"})
	if got := c.Name(); got != "tmux+wezterm" {
		t.Errorf("Name = %q, want tmux+wezterm", got)
	}
}
