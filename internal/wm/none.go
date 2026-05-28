package wm

import "context"

// noneManager is the Observe-only WM backend: no client list, no focus, no
// events. Selected when no supported window manager is detected. Clients
// returns empty (so reconcile leaves sessions WM-less rather than failing) and
// Subscribe returns a channel that stays open until ctx is cancelled, so the
// daemon's WM loop blocks harmlessly instead of spin-retrying.
type noneManager struct{}

// NewNone returns the Observe-only WM backend.
func NewNone() Manager { return noneManager{} }

func (noneManager) Name() string { return "none" }

func (noneManager) Available() bool { return false }

func (noneManager) Clients(context.Context) ([]Window, error) { return nil, nil }

func (noneManager) ActiveWindow(context.Context) (string, error) { return "", nil }

func (noneManager) Focus(context.Context, string) error { return ErrUnsupported }

func (noneManager) Subscribe(ctx context.Context) (<-chan Event, error) {
	ch := make(chan Event)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}
