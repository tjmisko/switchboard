//go:build linux

package terminal

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// socketInode reads the inode behind a connected socket from /proc/self/fd.
func socketInode(t *testing.T, conn *net.UnixConn) uint64 {
	t.Helper()
	f, err := conn.File()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	link, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(int(f.Fd())))
	if err != nil {
		t.Fatal(err)
	}
	inode, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]"), 10, 64)
	if err != nil {
		t.Fatalf("fd link %q is not a socket", link)
	}
	return inode
}

// Against the real kernel: the join the herdr backend depends on is that the
// accepted end of a connection carries the listener's path and names the
// client's inode as its peer.
func TestUnixSocketTableShouldPairAClientWithTheListenersAcceptedEndWhenConnected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "herdr-client.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	client, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	accepted, err := ln.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer accepted.Close()
	clientInode, acceptedInode := socketInode(t, client), socketInode(t, accepted)

	table, err := unixSocketTable()
	if err != nil {
		t.Fatalf("unixSocketTable: %v", err)
	}
	var listening, serverEnd, clientEnd *unixSocket
	for i := range table {
		s := &table[i]
		switch {
		case s.Inode == acceptedInode:
			serverEnd = s
		case s.Inode == clientInode:
			clientEnd = s
		case s.Name == path && s.State == unixStateListen:
			listening = s
		}
	}
	if listening == nil {
		t.Fatal("listening socket missing from the table")
	}
	if serverEnd == nil || serverEnd.Name != path || serverEnd.Peer != clientInode || serverEnd.State != unixStateEstablished {
		t.Fatalf("accepted end = %+v, want name %q, peer %d, established", serverEnd, path, clientInode)
	}
	if clientEnd == nil || clientEnd.Name != "" || clientEnd.Peer != acceptedInode {
		t.Fatalf("client end = %+v, want unnamed with peer %d", clientEnd, acceptedInode)
	}
}
