//go:build !linux && !darwin

package wezterm

import (
	"context"
	"errors"
)

func guiSocketPeerPID(context.Context, string) (int, error) {
	return 0, errors.New("unix socket peer pid is unsupported on this platform")
}
