package wm

import (
	"context"
	"testing"
)

// The none backend reports unavailable, lists no clients, refuses focus, and
// yields a Subscribe channel that closes only on ctx cancel — so the daemon's
// WM loop blocks harmlessly instead of spin-retrying in the Observe tier.
func TestNoneManager(t *testing.T) {
	m := NewNone()
	if m.Available() {
		t.Error("none.Available() = true, want false")
	}
	if cs, err := m.Clients(t.Context()); err != nil || cs != nil {
		t.Errorf("none.Clients = (%v, %v), want (nil, nil)", cs, err)
	}
	if err := m.Focus(t.Context(), "0xabc"); err != ErrUnsupported {
		t.Errorf("none.Focus err = %v, want ErrUnsupported", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	ch, err := m.Subscribe(ctx)
	if err != nil {
		t.Fatalf("none.Subscribe: %v", err)
	}
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("none.Subscribe emitted an event, want only close")
		}
	case <-t.Context().Done():
		t.Fatal("none.Subscribe channel did not close on ctx cancel")
	}
}
