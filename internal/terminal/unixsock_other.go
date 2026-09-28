//go:build !linux

package terminal

import "errors"

// unixSocketTable needs NETLINK_SOCK_DIAG, which only Linux has. Elsewhere the
// herdr backend cannot join a client to its server, so it reports no panes.
func unixSocketTable() ([]unixSocket, error) {
	return nil, errors.New("terminal: unix socket table unsupported on this platform")
}
