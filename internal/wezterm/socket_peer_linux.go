//go:build linux

package wezterm

import (
	"context"
	"fmt"
	"net"
	"time"

	"golang.org/x/sys/unix"
)

func guiSocketPeerPID(ctx context.Context, path string) (int, error) {
	dialer := net.Dialer{Timeout: 250 * time.Millisecond}
	conn, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("unexpected connection type %T", conn)
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Ucred
	var sockErr error
	if err := raw.Control(func(fd uintptr) {
		cred, sockErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if sockErr != nil {
		return 0, sockErr
	}
	return int(cred.Pid), nil
}
