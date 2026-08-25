//go:build !unix

package panebind

import (
	"errors"
	"io"
)

var errTTYUnsupported = errors.New("panebind: nonblocking tty emission is unsupported on this platform")

func openTTY(string) (io.WriteCloser, error) { return nil, errTTYUnsupported }

func retryableWriteError(error) bool { return false }
