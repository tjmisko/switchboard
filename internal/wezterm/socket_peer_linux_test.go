//go:build linux

package wezterm

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestGUISocketPeerPIDReadsKernelCredential(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gui.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skip("sandbox does not permit unix listeners")
		}
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	pid, err := guiSocketPeerPID(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if pid != os.Getpid() {
		t.Fatalf("peer PID = %d, want %d", pid, os.Getpid())
	}
	select {
	case conn := <-accepted:
		conn.Close()
	default:
	}
}
