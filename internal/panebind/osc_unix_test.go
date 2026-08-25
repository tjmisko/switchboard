//go:build unix

package panebind

import (
	"errors"
	"io"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

type errnoTTY struct {
	writes int
	closed bool
}

func (w *errnoTTY) Write([]byte) (int, error) {
	w.writes++
	return 0, unix.EAGAIN
}

func (w *errnoTTY) Close() error { w.closed = true; return nil }

func TestAnnounceEAGAINUsesOnlyBoundedDirectAttempts(t *testing.T) {
	tty := &errnoTTY{}
	a := Announcer{OpenTTY: func(string) (io.WriteCloser, error) { return tty, nil }}
	err := a.Announce(t.Context(), Target{
		Session: exact("h", 1, "2026-08-24T20:00:00Z"), TTY: "/dev/pts/1",
	}, func(ExactSessionKey, string) bool { return true })
	if !errors.Is(err, unix.EAGAIN) {
		t.Fatalf("error = %v, want EAGAIN", err)
	}
	if tty.writes != maxWriteAttempts || !tty.closed {
		t.Fatalf("writes=%d closed=%t, want %d,true", tty.writes, tty.closed, maxWriteAttempts)
	}
}

type interruptThenWriteTTY struct {
	recordingTTY
	interrupt int
}

func (w *interruptThenWriteTTY) Write(p []byte) (int, error) {
	w.writes++
	if w.interrupt > 0 {
		w.interrupt--
		return 0, unix.EINTR
	}
	return w.Buffer.Write(p)
}

func TestAnnounceRetriesEINTRWithinAttemptBudget(t *testing.T) {
	tty := &interruptThenWriteTTY{interrupt: 2}
	a := Announcer{OpenTTY: func(string) (io.WriteCloser, error) { return tty, nil }}
	err := a.Announce(t.Context(), Target{
		Session: exact("h", 1, "2026-08-24T20:00:00Z"), TTY: "/dev/pts/1",
	}, func(ExactSessionKey, string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if tty.writes != 3 || !tty.closed {
		t.Fatalf("writes=%d closed=%t, want 3,true", tty.writes, tty.closed)
	}
}

func TestDefaultTTYOpenerRejectsRegularFile(t *testing.T) {
	path := t.TempDir() + "/ordinary"
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := openTTY(path)
	if f != nil {
		f.Close()
	}
	if !errors.Is(err, ErrNotTTY) {
		t.Fatalf("openTTY error = %v, want ErrNotTTY", err)
	}
}
