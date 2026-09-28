package terminal

// Foot (codeberg.org/dnkl/foot) is Omarchy's default terminal. It has no IPC
// and no tabs or splits, so it is a pty-owner backend: see ptyOwnerLocator.
//
// Omarchy launches plain `foot`, one process per window, which makes every
// session's window join exact. Under `foot --server` the footclient windows
// share the server's pid; a session in a server hosting several windows stays
// Observe-only.

// NewFoot returns the foot terminal locator over the real /proc.
func NewFoot() Locator { return newFoot(newProcessScan("/proc", processScanTTL)) }

func newFoot(scan *processScan) ptyOwnerLocator { return newPtyOwner("foot", "foot", scan) }

// newFootAt roots an uncached foot locator at a /proc-shaped fixture tree.
func newFootAt(root string) ptyOwnerLocator { return newFoot(newProcessScan(root, 0)) }
