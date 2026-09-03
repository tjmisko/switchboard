//go:build darwin

package osproc

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestReadShouldReportAccurateMetadataWhenProcessIsSelf(t *testing.T) {
	src := newDarwinSource()
	info, err := src.Read(os.Getpid())
	if err != nil {
		t.Fatalf("Read(self): %v", err)
	}
	if info.PID != os.Getpid() {
		t.Errorf("PID = %d, want %d", info.PID, os.Getpid())
	}
	if info.PPID != os.Getppid() {
		t.Errorf("PPID = %d, want %d", info.PPID, os.Getppid())
	}
	if info.Comm == "" {
		t.Error("Comm is empty")
	}
	wantCWD, _ := os.Getwd()
	if info.CWD != wantCWD {
		t.Errorf("CWD = %q, want %q", info.CWD, wantCWD)
	}
	if info.Exe == "" {
		t.Error("Exe is empty")
	}
	if len(info.Args) == 0 {
		t.Fatal("Args is empty; argv[0] is the identity signal discovery relies on")
	}
}

// The cwd of a child is read from the PARENT's process, which is the case
// discovery actually exercises — it never reads its own cwd in anger.
func TestReadShouldReportChildWorkingDirectoryWhenChildIsOwnedBySameUser(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "30")
	cmd.Dir = "/usr"
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	src := newDarwinSource()
	deadline := time.Now().Add(2 * time.Second)
	var info Info
	for time.Now().Before(deadline) {
		var err error
		if info, err = src.Read(cmd.Process.Pid); err == nil && info.CWD != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if info.CWD != "/usr" {
		t.Errorf("child CWD = %q, want /usr", info.CWD)
	}
	if filepath.Base(info.Exe) != "sleep" {
		t.Errorf("child Exe = %q, want basename sleep", info.Exe)
	}
	if len(info.Args) == 0 || filepath.Base(info.Args[0]) != "sleep" {
		t.Errorf("child Args = %q, want argv[0] basename sleep", info.Args)
	}
}

func TestReadShouldReturnErrGoneWhenProcessHasExited(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()

	src := newDarwinSource()
	if _, err := src.Read(pid); !errors.Is(err, ErrGone) {
		t.Errorf("Read(dead pid) error = %v, want ErrGone", err)
	}
}

func TestAllPIDsShouldIncludeSelfAndCostOneSyscall(t *testing.T) {
	src := newDarwinSource()
	pids, err := src.AllPIDs()
	if err != nil {
		t.Fatalf("AllPIDs: %v", err)
	}
	if len(pids) < 10 {
		t.Fatalf("AllPIDs returned %d pids, want a real process table", len(pids))
	}
	for _, pid := range pids {
		if pid == os.Getpid() {
			return
		}
	}
	t.Error("AllPIDs did not include self")
}

func TestWatchShouldFireOnDeathExactlyOnceWhenProcessExits(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid

	var calls atomic.Int32
	fired := make(chan struct{}, 4)
	src := newDarwinSource()
	if err := src.Watch(context.Background(), pid, func() {
		calls.Add(1)
		fired <- struct{}{}
	}); err != nil {
		t.Fatalf("Watch: %v", err)
	}

	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()

	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("onDeath never fired")
	}
	// Give any duplicate a chance to arrive before asserting exactly-once.
	time.Sleep(300 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Errorf("onDeath fired %d times, want exactly 1", got)
	}
}

func TestWatchShouldFireOnDeathWhenProcessIsAlreadyGone(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()

	fired := make(chan struct{}, 1)
	src := newDarwinSource()
	if err := src.Watch(context.Background(), pid, func() { fired <- struct{}{} }); err != nil {
		t.Fatalf("Watch: %v", err)
	}
	select {
	case <-fired:
	case <-time.After(3 * time.Second):
		t.Fatal("onDeath never fired for an already-dead pid")
	}
}

func TestStopShouldCancelWatcherWithoutFiringOnDeath(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	var calls atomic.Int32
	src := newDarwinSource()
	if err := src.Watch(context.Background(), cmd.Process.Pid, func() { calls.Add(1) }); err != nil {
		t.Fatalf("Watch: %v", err)
	}
	src.Stop(cmd.Process.Pid)
	// The watcher wakes on its own timeout, so allow more than one tick.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(src.Watched()) == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := len(src.Watched()); got != 0 {
		t.Errorf("still watching %d pids after Stop", got)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("onDeath fired %d times after Stop, want 0", got)
	}
}

func TestTTYNameShouldRenderPtySlavePathWhenDeviceIsPty(t *testing.T) {
	tests := []struct {
		name string
		tdev int32
		want string
	}{
		{"no controlling terminal", -1, ""},
		{"pty slave 0", 0x10000000, "/dev/ttys000"},
		{"pty slave 1", 0x10000001, "/dev/ttys001"},
		{"pty slave 137", 0x10000089, "/dev/ttys137"},
		{"non-pty major is not guessed", 0x02000000, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ttyName(tt.tdev); got != tt.want {
				t.Errorf("ttyName(%#x) = %q, want %q", tt.tdev, got, tt.want)
			}
		})
	}
}

// The tty string is the join key the terminal seam matches against tmux's
// #{pane_tty} and wezterm's tty_name, so it must agree with what the OS itself
// reports. Cross-check every process the kernel says owns a pty against ps(1).
func TestTTYNameShouldAgreeWithPsForLivePtyProcesses(t *testing.T) {
	src := newDarwinSource()
	infos, err := src.Enumerate()
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	out, err := exec.Command("ps", "-Ao", "pid=,tty=").Output()
	if err != nil {
		t.Skipf("ps unavailable: %v", err)
	}
	psTTY := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 {
			psTTY[f[0]] = f[1]
		}
	}

	checked := 0
	for _, info := range infos {
		if info.TTY == "" {
			continue
		}
		got, ok := psTTY[itoa(info.PID)]
		if !ok || got == "??" {
			continue // exited between the two reads, or ps hides it
		}
		if want := "/dev/" + got; info.TTY != want {
			t.Errorf("pid %d TTY = %q, ps says %q", info.PID, info.TTY, want)
		}
		checked++
	}
	if checked == 0 {
		t.Skip("no live pty-owning processes to cross-check")
	}
	t.Logf("cross-checked %d pty-owning processes against ps", checked)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
