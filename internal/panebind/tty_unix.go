//go:build unix

package panebind

import (
	"errors"
	"io"

	"golang.org/x/sys/unix"
)

// directTTY deliberately bypasses os.File: os.File.Write can attach an
// O_NONBLOCK descriptor to Go's poller and wait on EAGAIN. Direct unix.Write
// preserves the Announcer's explicit bounded-attempt contract.
type directTTY struct {
	fd int
}

func (t *directTTY) Write(p []byte) (int, error) { return unix.Write(t.fd, p) }

func (t *directTTY) Close() error {
	if t.fd < 0 {
		return nil
	}
	fd := t.fd
	t.fd = -1
	return unix.Close(fd)
}

func openTTY(path string) (io.WriteCloser, error) {
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_NOCTTY|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	tty := &directTTY{fd: fd}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		tty.Close()
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFCHR {
		tty.Close()
		return nil, ErrNotTTY
	}
	return tty, nil
}

func retryableWriteError(err error) bool {
	return errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN)
}
