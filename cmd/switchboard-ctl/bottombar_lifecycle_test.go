//go:build linux

package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// This helper is reached through an executable shim named waybar. Its command
// identity intentionally differs from Waybar, as can happen while a launched
// child is still in exec. It never connects to the desktop and ignores SIGTERM
// so shutdown tests can hold the process alive deterministically.
func TestBottomLifecycleHelper(t *testing.T) {
	if os.Getenv("SWITCHBOARD_TEST_BOTTOM_HELPER") != "1" {
		return
	}
	signal.Ignore(syscall.SIGTERM)
	if err := os.WriteFile(os.Getenv("SWITCHBOARD_TEST_BOTTOM_HANDSHAKE"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		os.Exit(2)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func bottomLifecycleConfig(t *testing.T) bottomBarConfig {
	t.Helper()
	dir := t.TempDir()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	shim := "#!/bin/sh\nexec \"$SWITCHBOARD_TEST_BOTTOM_BINARY\" -test.run=^TestBottomLifecycleHelper$\n"
	if err := os.WriteFile(filepath.Join(dir, "waybar"), []byte(shim), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SWITCHBOARD_TEST_BOTTOM_BINARY", binary)
	t.Setenv("SWITCHBOARD_TEST_BOTTOM_HELPER", "1")
	t.Setenv("SWITCHBOARD_TEST_BOTTOM_HANDSHAKE", filepath.Join(dir, "helper.ready"))
	return bottomBarConfig{
		pidFile:      filepath.Join(dir, "bottom.pid"),
		readyFile:    filepath.Join(dir, "bottom.ready"),
		lockFile:     filepath.Join(dir, "bottom.lock"),
		marker:       filepath.Join(dir, "top.hidden"),
		waybarConfig: filepath.Join(dir, "claude.jsonc"),
		slotDir:      dir,
		ops:          defaultOps(),
	}
}

func waitBottomTest(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for " + description)
}

// Capture a pidfd immediately, register cleanup before any assertions, and keep
// it open even when the tested lifecycle discards its record. Cleanup must not
// leave a helper alive or signal a reused numeric PID after a failed test.
func trackBottomTestChild(t *testing.T, cfg bottomBarConfig) bottomProcess {
	t.Helper()
	b, err := os.ReadFile(cfg.pidFile)
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	var started uint64
	if _, err := fmt.Sscan(string(b), &pid, &started); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		defer unix.Close(fd)
		_ = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
		waitBottomTest(t, "helper exit", func() bool { done, err := bottomProcessExited(fd); return err == nil && done })
	})
	waitBottomTest(t, "helper startup", func() bool {
		b, err := os.ReadFile(os.Getenv("SWITCHBOARD_TEST_BOTTOM_HANDSHAKE"))
		return err == nil && string(b) == strconv.Itoa(pid)
	})
	return bottomProcess{pid: pid, pidfd: fd, started: started}
}

func TestOwnedBottomProcessSurvivesUnreadyCommandIdentity(t *testing.T) {
	cfg := bottomLifecycleConfig(t)
	starts := 0
	cfg.ops.start = func(c bottomBarConfig) error { starts++; return startBottom(c) }
	reconcileWith(cfg, 1)
	child := trackBottomTestChild(t, cfg)
	comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", child.pid))
	if err != nil || strings.TrimSpace(string(comm)) == "waybar" {
		t.Fatalf("helper must exercise an unfinished Waybar identity: comm=%q err=%v", comm, err)
	}
	matches, err := bottomCommandMatches(cfg, child.pid)
	if err != nil || matches {
		t.Fatalf("helper unexpectedly has final command identity: matches=%v err=%v", matches, err)
	}

	publisher := newSlotPublisher(cfg.slotDir, 1)
	publisher.dirty[0] = true
	signals := 0
	publisher.signal = func(int, int) error { signals++; return nil }
	for i := 0; i < 3; i++ {
		// This is the original bug's order: readiness probing immediately
		// after a launch, followed by another snapshot reconciliation.
		flushed, pid := flushSlotsWhenReady(cfg, publisher)
		if flushed || pid != child.pid {
			t.Fatalf("unready probe lost ownership: flushed=%v pid=%d want=%d", flushed, pid, child.pid)
		}
		reconcileWith(cfg, 1)
	}
	if starts != 1 || signals != 0 {
		t.Fatalf("before readiness: starts=%d signals=%d", starts, signals)
	}
	if err := os.WriteFile(cfg.readyFile, []byte(fmt.Sprintf("%d\n", child.pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	flushed, pid := flushSlotsWhenReady(cfg, publisher)
	if !flushed || pid != child.pid || signals != 1 {
		t.Fatalf("ready generation did not catch up: flushed=%v pid=%d signals=%d", flushed, pid, signals)
	}
}

func TestBottomStopRetainsOwnershipUntilExit(t *testing.T) {
	cfg := bottomLifecycleConfig(t)
	starts := 0
	cfg.ops.start = func(c bottomBarConfig) error { starts++; return startBottom(c) }
	reconcileWith(cfg, 1)
	child := trackBottomTestChild(t, cfg)
	reconcileWith(cfg, 0)
	for i := 0; i < 3; i++ {
		reconcileWith(cfg, 1)
	}
	if starts != 1 || bottomPID(cfg) != child.pid {
		t.Fatalf("replacement launched before terminating child exited: starts=%d pid=%d", starts, bottomPID(cfg))
	}
	if err := unix.PidfdSendSignal(child.pidfd, unix.SIGKILL, nil, 0); err != nil {
		t.Fatal(err)
	}
	waitBottomTest(t, "confirmed exit", func() bool { done, err := bottomProcessExited(child.pidfd); return err == nil && done })
	reconcileWith(cfg, 1)
	replacement := trackBottomTestChild(t, cfg)
	if starts != 2 || replacement.pid == child.pid {
		t.Fatalf("no replacement after exit: starts=%d old=%d new=%d", starts, child.pid, replacement.pid)
	}
}

func TestOwnedBottomProcessRejectsDifferentGeneration(t *testing.T) {
	cfg := bottomLifecycleConfig(t)
	if err := startBottom(cfg); err != nil {
		t.Fatal(err)
	}
	child := trackBottomTestChild(t, cfg)
	if err := replaceFile(cfg.pidFile, []byte(fmt.Sprintf("%d %d\n", child.pid, child.started+1))); err != nil {
		t.Fatal(err)
	}
	stopBottom(cfg)
	if _, err := os.Stat(cfg.pidFile); !os.IsNotExist(err) {
		t.Fatalf("stale generation retained: %v", err)
	}
	exited, err := bottomProcessExited(child.pidfd)
	if err != nil || exited {
		t.Fatalf("unrelated generation was terminated: exited=%v err=%v", exited, err)
	}
}

func TestBottomOwnershipReadFailureBlocksReplacement(t *testing.T) {
	dir := t.TempDir()
	cfg := bottomBarConfig{pidFile: dir, readyFile: filepath.Join(dir, "ready"), ops: defaultOps()}
	starts := 0
	cfg.ops.start = func(bottomBarConfig) error { starts++; return nil }
	// A directory reliably produces a read error, even when tests run as root.
	ensureStarted(cfg)
	stopBottom(cfg)
	ensureStarted(cfg)
	if starts != 0 {
		t.Fatalf("read failure launched %d replacements", starts)
	}
	if _, err := os.Stat(cfg.pidFile); err != nil {
		t.Fatalf("uncertain ownership discarded: %v", err)
	}
}
