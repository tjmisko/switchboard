//go:build linux

package terminal

import (
	"encoding/binary"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// unixSocketTable dumps every AF_UNIX socket in this network namespace through
// NETLINK_SOCK_DIAG — the interface `ss -x` reads — with each socket's bound
// path and its peer's inode. It needs no privilege: the dump covers every
// socket, and the caller joins it only against inodes it can already see in
// its own processes' fd tables.
//
// This is what makes the herdr client → server join exact. A connected client
// socket has no path of its own; the server's accepted end carries the
// listener's path. So "which server is this client attached to" is answered by
// the kernel (client inode → peer inode → that socket's path), not by
// re-deriving herdr's session and socket-override rules from argv and env.
func unixSocketTable() ([]unixSocket, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.NETLINK_SOCK_DIAG)
	if err != nil {
		return nil, fmt.Errorf("sock_diag socket: %w", err)
	}
	defer unix.Close(fd)

	if err := unix.Sendto(fd, unixDiagDumpRequest(), 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return nil, fmt.Errorf("sock_diag request: %w", err)
	}

	var sockets []unixSocket
	buf := make([]byte, 64*1024)
	for {
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			return nil, fmt.Errorf("sock_diag receive: %w", err)
		}
		msgs, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			return nil, fmt.Errorf("sock_diag parse: %w", err)
		}
		for _, msg := range msgs {
			switch msg.Header.Type {
			case unix.NLMSG_DONE:
				return sockets, nil
			case unix.NLMSG_ERROR:
				return nil, fmt.Errorf("sock_diag: %w", netlinkError(msg.Data))
			case unix.SOCK_DIAG_BY_FAMILY:
				if s, ok := parseUnixDiagMsg(msg.Data); ok {
					sockets = append(sockets, s)
				}
			}
		}
	}
}

// Attribute ids from linux/unix_diag.h: which extras the dump carries, and the
// rtattr types they arrive as.
const (
	unixDiagShowName = 0x1
	unixDiagShowPeer = 0x4
	unixDiagAttrName = 0
	unixDiagAttrPeer = 2
)

// unixDiagDumpRequest encodes nlmsghdr + struct unix_diag_req asking for every
// state of every AF_UNIX socket, with its name and peer.
func unixDiagDumpRequest() []byte {
	const hdrLen, reqLen = unix.SizeofNlMsghdr, 24
	b := make([]byte, hdrLen+reqLen)
	ne := binary.NativeEndian
	ne.PutUint32(b[0:], hdrLen+reqLen)
	ne.PutUint16(b[4:], unix.SOCK_DIAG_BY_FAMILY)
	ne.PutUint16(b[6:], unix.NLM_F_REQUEST|unix.NLM_F_DUMP)
	ne.PutUint32(b[8:], 1)                    // seq
	ne.PutUint32(b[12:], uint32(os.Getpid())) // port id
	req := b[hdrLen:]
	req[0] = unix.AF_UNIX
	ne.PutUint32(req[4:], 0xffffffff) // udiag_states: every state
	ne.PutUint32(req[12:], unixDiagShowName|unixDiagShowPeer)
	return b
}

// parseUnixDiagMsg decodes struct unix_diag_msg and its rtattrs.
func parseUnixDiagMsg(data []byte) (unixSocket, bool) {
	const msgLen = 16
	if len(data) < msgLen {
		return unixSocket{}, false
	}
	ne := binary.NativeEndian
	s := unixSocket{
		State: int(data[2]),
		Inode: uint64(ne.Uint32(data[4:])),
	}
	attrs := data[msgLen:]
	for len(attrs) >= unix.SizeofRtAttr {
		attrLen := int(ne.Uint16(attrs[0:]))
		attrType := ne.Uint16(attrs[2:])
		if attrLen < unix.SizeofRtAttr || attrLen > len(attrs) {
			break
		}
		payload := attrs[unix.SizeofRtAttr:attrLen]
		switch attrType {
		case unixDiagAttrName:
			// An abstract name starts with NUL; herdr binds filesystem paths only,
			// so an abstract name is kept verbatim and simply never matches.
			s.Name = string(trimNUL(payload))
		case unixDiagAttrPeer:
			if len(payload) >= 4 {
				s.Peer = uint64(ne.Uint32(payload))
			}
		}
		next := (attrLen + unix.NLMSG_ALIGNTO - 1) &^ (unix.NLMSG_ALIGNTO - 1)
		if next > len(attrs) {
			break
		}
		attrs = attrs[next:]
	}
	return s, true
}

func trimNUL(b []byte) []byte {
	for len(b) > 0 && b[len(b)-1] == 0 {
		b = b[:len(b)-1]
	}
	return b
}

func netlinkError(data []byte) error {
	if len(data) < 4 {
		return fmt.Errorf("truncated error message")
	}
	errno := -int32(binary.NativeEndian.Uint32(data))
	if errno == 0 {
		return fmt.Errorf("unexpected ack")
	}
	return unix.Errno(errno)
}
