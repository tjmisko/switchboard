package terminal

import "context"

// noneLocator is the Observe-only terminal backend: no IPC, so no tty ever
// resolves to a pane and focus is unsupported. Selected when no known terminal
// multiplexer is detected. It never errors on Locate (an unknown tty is a
// normal, expected outcome) so the daemon keeps running in the Observe tier.
type noneLocator struct{}

// NewNone returns the Observe-only terminal locator.
func NewNone() Locator { return noneLocator{} }

func (noneLocator) Name() string { return "none" }

func (noneLocator) Available() bool { return false }

func (noneLocator) Locate(context.Context, string) (*PaneRef, error) { return nil, nil }

func (noneLocator) Activate(context.Context, *PaneRef) error { return ErrUnsupported }
