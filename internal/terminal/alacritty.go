package terminal

// Alacritty has no tabs or splits, and its `alacritty msg` IPC can create
// windows and change config but cannot list or focus them. So, like foot, it is
// a pty-owner backend: see ptyOwnerLocator.
//
// Omarchy launches plain `alacritty`, one process per window, which makes every
// session's window join exact. Windows opened with `alacritty msg
// create-window` (including under `alacritty --daemon`) share one process; a
// session in such a process hosting several windows stays Observe-only.
// ALACRITTY_WINDOW_ID does not help there: on Wayland it is a client-side
// pointer that the compositor never sees.

// NewAlacritty returns the Alacritty terminal locator over the real /proc.
func NewAlacritty() Locator { return newAlacritty(newProcessScan("/proc", processScanTTL)) }

func newAlacritty(scan *processScan) ptyOwnerLocator {
	return newPtyOwner("alacritty", "alacritty", scan)
}

// newAlacrittyAt roots an uncached Alacritty locator at a fixture tree.
func newAlacrittyAt(root string) ptyOwnerLocator { return newAlacritty(newProcessScan(root, 0)) }
