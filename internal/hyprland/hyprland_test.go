package hyprland

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/testsupport"
)

// §6.2 socketPath — the Available()==false precondition: without the Hyprland
// instance signature or XDG_RUNTIME_DIR we cannot locate the IPC sockets and
// must error cleanly rather than dialing a bogus path. The Subscribe parse
// loop gets its harness (ScriptedConn) consumer in §0.4 once it is extracted
// to take an io.Reader.
func TestSocketPathRequiresEnv(t *testing.T) {
	t.Setenv("HYPRLAND_INSTANCE_SIGNATURE", "")
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	if _, err := requestSocketPath(); err == nil {
		t.Error("requestSocketPath should error when HYPRLAND_INSTANCE_SIGNATURE is unset")
	}

	t.Setenv("HYPRLAND_INSTANCE_SIGNATURE", "deadbeef")
	t.Setenv("XDG_RUNTIME_DIR", "")
	if _, err := socket2Path(); err == nil {
		t.Error("socket2Path should error when XDG_RUNTIME_DIR is unset")
	}

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
