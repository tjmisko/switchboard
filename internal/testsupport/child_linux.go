//go:build linux

package testsupport

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// SpawnTTYChild starts `sleep` attached to a fresh pseudo-terminal as its
// std streams, so /proc reports a /dev/pts/N controlling terminal for the
// child — the "interactive child / non-empty tty" case. The conformance suite
// asserts the tty is non-empty WITHOUT inspecting the pts path, so the macOS
// backend (with its own pty helper) passes the same contract. The pty master
// is closed on cleanup.
func SpawnTTYChild(t testing.TB, d time.Duration) *Child {
	t.Helper()

	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open /dev/ptmx: %v", err)
	}
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		master.Close()
		t.Fatalf("unlockpt: %v", err)
	}
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		master.Close()
		t.Fatalf("get pts number: %v", err)
	}
	slavePath := fmt.Sprintf("/dev/pts/%d", n)
	slave, err := os.OpenFile(slavePath, os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		t.Fatalf("open %s: %v", slavePath, err)
	}

	cmd := exec.Command("sleep", strconv.Itoa(sleepSeconds(d)))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	if err := cmd.Start(); err != nil {
		slave.Close()
		master.Close()
		t.Fatalf("spawn tty child: %v", err)
	}
	slave.Close() // the child holds its own dup of the slave

	c := &Child{PID: cmd.Process.Pid, cmd: cmd}
	t.Cleanup(func() {
		c.Kill(t)
		master.Close()
	})
	return c
}
