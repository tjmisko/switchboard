package testsupport

import (
	"os/exec"
	"strconv"
	"sync"
	"testing"
	"time"
)

// Child is a real short-lived process used by death-watch tests (procwatch,
// and the osproc conformance suite). It wraps an *exec.Cmd and guarantees the
// process is killed and reaped by the time the test ends, so a failed
// assertion never leaks a process or a zombie.
type Child struct {
	PID  int
	cmd  *exec.Cmd
	once sync.Once
}

// SpawnSleep starts `sleep <seconds>` and returns it. Its std streams are left
// nil, so os/exec wires them to /dev/null — the child has no controlling tty
// (the "non-interactive child / empty tty" case). The caller drives its death
// explicitly via Kill; t.Cleanup reaps it otherwise. Pass a duration longer
// than the test's window so the child only dies when the test kills it.
func SpawnSleep(t testing.TB, d time.Duration) *Child {
	t.Helper()
	cmd := exec.Command("sleep", strconv.Itoa(sleepSeconds(d)))
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn sleep: %v", err)
	}
	c := &Child{PID: cmd.Process.Pid, cmd: cmd}
	t.Cleanup(func() { c.Kill(t) })
	return c
}

func sleepSeconds(d time.Duration) int {
	secs := int(d.Seconds())
	if secs < 1 {
		return 1
	}
	return secs
}

// Kill sends SIGKILL and reaps the child. Idempotent and safe to call from
// cleanup even after the child already exited. After Kill returns the process
// has been waited on, so its pid is eligible for reuse.
func (c *Child) Kill(t testing.TB) {
	t.Helper()
	c.once.Do(func() {
		_ = c.cmd.Process.Kill()
		_ = c.cmd.Wait()
	})
}
