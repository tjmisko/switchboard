package hyprland

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/tjmisko/switchboard/internal/testsupport"
)

// §6.2 socketPath — when $HYPRLAND_INSTANCE_SIGNATURE is set it is used directly,
// joined under $XDG_RUNTIME_DIR/hypr.
func TestSocketPathFromEnv(t *testing.T) {
	t.Setenv("HYPRLAND_INSTANCE_SIGNATURE", "deadbeef")
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	got, err := requestSocketPath()
	if err != nil {
		t.Fatalf("requestSocketPath with env set: %v", err)
	}
	want := filepath.Join("/run/user/1000", "hypr", "deadbeef", ".socket.sock")
	if got != want {
		t.Errorf("requestSocketPath = %q, want %q", got, want)
	}
}

// §6.2 socketPath — without XDG_RUNTIME_DIR we cannot locate the IPC sockets and
// must error cleanly rather than dialing a bogus path.
func TestSocketPathErrorsWhenXDGUnset(t *testing.T) {
	t.Setenv("HYPRLAND_INSTANCE_SIGNATURE", "deadbeef")
	t.Setenv("XDG_RUNTIME_DIR", "")
	if _, err := socket2Path(); err == nil {
		t.Error("socket2Path should error when XDG_RUNTIME_DIR is unset")
	}
}

// §6.2 socketPath — when the env var is absent (a bare user-manager restart that
// never re-ran the compositor's import-environment), the signature is discovered
// from the live lock file under $XDG_RUNTIME_DIR/hypr.
func TestSocketPathDiscoversInstanceWhenEnvUnset(t *testing.T) {
	xdg := t.TempDir()
	sig := "abc123"
	writeInstance(t, filepath.Join(xdg, "hypr", sig), os.Getpid(), time.Now())
	t.Setenv("HYPRLAND_INSTANCE_SIGNATURE", "")
	t.Setenv("XDG_RUNTIME_DIR", xdg)

	got, err := requestSocketPath()
	if err != nil {
		t.Fatalf("requestSocketPath should discover instance: %v", err)
	}
	if want := filepath.Join(xdg, "hypr", sig, ".socket.sock"); got != want {
		t.Errorf("requestSocketPath = %q, want %q", got, want)
	}
}

// §6.2 socketPath — no env var and no discoverable instance must still error
// cleanly rather than fabricate a path.
func TestSocketPathErrorsWhenNoInstance(t *testing.T) {
	t.Setenv("HYPRLAND_INSTANCE_SIGNATURE", "")
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir()) // no hypr/ subtree
	if _, err := requestSocketPath(); err == nil {
		t.Error("requestSocketPath should error when no Hyprland instance is discoverable")
	}
}

// writeInstance creates a fake hypr/<sig> instance dir: a hyprland.lock whose
// first line is pid (the format Hyprland writes — PID, then address lines) and a
// .socket.sock stamped at mod.
func writeInstance(t *testing.T, dir string, pid int, mod time.Time) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lock := fmt.Sprintf("%d\n127.0.0.1\nHyprland\n", pid)
	if err := os.WriteFile(filepath.Join(dir, "hyprland.lock"), []byte(lock), 0o600); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, ".socket.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(sock, mod, mod); err != nil {
		t.Fatal(err)
	}
}

// deadPID returns a PID that is not currently alive, so its lock looks stale. The
// value sits above the kernel's pid_max ceiling on any realistic host; the
// liveness probe confirms it is dead before the test relies on it.
func deadPID(t *testing.T) int {
	t.Helper()
	const pid = 1 << 30 // 1073741824
	if unix.Kill(pid, 0) == nil {
		t.Fatalf("pid %d unexpectedly alive", pid)
	}
	return pid
}

// discoverInstance — the live instance is the one whose hyprland.lock names a PID
// still present (the systemd unit's /proc check), even when a stale dir from a
// prior session carries a newer socket. Liveness must beat mtime.
func TestDiscoverInstancePicksLiveLock(t *testing.T) {
	hyprDir := t.TempDir()
	writeInstance(t, filepath.Join(hyprDir, "stale"), deadPID(t), time.Now())
	writeInstance(t, filepath.Join(hyprDir, "live"), os.Getpid(), time.Now().Add(-time.Hour))

	got, err := discoverInstance(hyprDir)
	if err != nil {
		t.Fatalf("discoverInstance: %v", err)
	}
	if got != "live" {
		t.Errorf("discoverInstance = %q, want %q (live lock PID wins over newer stale socket)", got, "live")
	}
}

// discoverInstance — when several instances are live (should not happen in
// practice) the freshest .socket.sock breaks the tie.
func TestDiscoverInstanceTieBreaksOnFreshestSocket(t *testing.T) {
	hyprDir := t.TempDir()
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	writeInstance(t, filepath.Join(hyprDir, "older"), os.Getpid(), old)
	writeInstance(t, filepath.Join(hyprDir, "newer"), os.Getpid(), recent)

	got, err := discoverInstance(hyprDir)
	if err != nil {
		t.Fatalf("discoverInstance: %v", err)
	}
	if got != "newer" {
		t.Errorf("discoverInstance = %q, want %q (freshest socket breaks live tie)", got, "newer")
	}
}

// discoverInstance — with no live lock anywhere it falls back to the newest
// .socket.sock as a last resort rather than failing outright.
func TestDiscoverInstanceFallsBackToNewestSocket(t *testing.T) {
	hyprDir := t.TempDir()
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	writeInstance(t, filepath.Join(hyprDir, "old"), deadPID(t), old)
	writeInstance(t, filepath.Join(hyprDir, "new"), deadPID(t), recent)

	got, err := discoverInstance(hyprDir)
	if err != nil {
		t.Fatalf("discoverInstance: %v", err)
	}
	if got != "new" {
		t.Errorf("discoverInstance = %q, want %q (newest socket fallback)", got, "new")
	}
}

// discoverInstance — an empty hypr dir yields a clean error rather than a bogus
// path.
func TestDiscoverInstanceErrorsWhenEmpty(t *testing.T) {
	if _, err := discoverInstance(t.TempDir()); err == nil {
		t.Error("discoverInstance should error on an empty hypr dir")
	}
}

// collectEvents drains parseEvents over a finite reader (which returns at EOF)
// and returns the events it forwarded.
func collectEvents(r io.Reader) []Event {
	ch := make(chan Event, 16)
	parseEvents(context.Background(), r, ch)
	close(ch)
	var evs []Event
	for e := range ch {
		evs = append(evs, e)
	}
	return evs
}

// §6.1 parseEvents — splits each line on the FIRST ">>"; the remainder
// (including further ">>") stays in Data.
func TestParseEventsSplitsOnFirstDelimiter(t *testing.T) {
	evs := collectEvents(testsupport.LineReader("activewindowv2>>0x5,foo>>bar", "openwindow>>0x6"))
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(evs), evs)
	}
	if evs[0].Name != "activewindowv2" || evs[0].Data != "0x5,foo>>bar" {
		t.Errorf("event 0 = %+v, want {activewindowv2, 0x5,foo>>bar}", evs[0])
	}
	if evs[1].Name != "openwindow" || evs[1].Data != "0x6" {
		t.Errorf("event 1 = %+v, want {openwindow, 0x6}", evs[1])
	}
}

// §6.1 parseEvents — drops lines with no ">>" delimiter.
func TestParseEventsDropsDelimiterlessLines(t *testing.T) {
	evs := collectEvents(testsupport.LineReader("no delimiter here", "good>>1", ""))
	if len(evs) != 1 || evs[0].Name != "good" || evs[0].Data != "1" {
		t.Errorf("events = %+v, want only {good, 1}", evs)
	}
}

// §6.1 parseEvents — a line larger than bufio's default 64 KiB token but under
// the 1 MiB cap is delivered intact, not dropped or errored.
func TestParseEventsHandlesLongLines(t *testing.T) {
	name := strings.Repeat("a", 200_000)
	evs := collectEvents(testsupport.LineReader(name + ">>payload"))
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	if len(evs[0].Name) != 200_000 || evs[0].Data != "payload" {
		t.Errorf("long-line event Name len = %d, Data = %q", len(evs[0].Name), evs[0].Data)
	}
}

// §6.1 parseEvents — returns when the reader reaches EOF (the caller then
// closes the channel; Subscribe relies on this).
func TestParseEventsReturnsOnEOF(t *testing.T) {
	done := make(chan struct{})
	go func() {
		ch := make(chan Event, 4)
		parseEvents(context.Background(), testsupport.LineReader("a>>1"), ch)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("parseEvents did not return on EOF")
	}
}

// §6.1 parseEvents — stops when ctx is cancelled while a send is pending
// (subscriber gone / shutting down).
func TestParseEventsStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	conn := testsupport.ScriptedLines("activewindowv2>>0x5")
	defer conn.Close()

	ch := make(chan Event) // unbuffered, never read → the send blocks
	done := make(chan struct{})
	go func() {
		parseEvents(ctx, conn, ch)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("parseEvents did not stop on ctx cancel")
	}
}
