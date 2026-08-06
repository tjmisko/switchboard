package main

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

// stalledWriter models stderr whose reader has stopped draining: the first write
// blocks until released. That is journald wedged, a terminal that stopped
// consuming, or a `| head` on a manual run — and the daemon logs from inside
// store.Apply, so a blocked write there holds the exclusive lock against every RPC
// reader, hook and waybar subscriber for as long as it lasts.
type stalledWriter struct {
	release chan struct{}
	mu      sync.Mutex
	buf     bytes.Buffer
	stalled bool
}

func (w *stalledWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	first := !w.stalled
	w.stalled = true
	w.mu.Unlock()
	if first {
		<-w.release
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *stalledWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func TestNonBlockingLogNeverBlocksItsCaller(t *testing.T) {
	out := &stalledWriter{release: make(chan struct{})}
	w := newNonBlockingLog(out)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range logBufferLines * 2 {
			w.Write([]byte("a log line\n"))
		}
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		close(out.release)
		t.Fatal("logging blocked while stderr was not draining; in production that write happens with the store lock held")
	}

	if w.dropped.Load() == 0 {
		t.Error("nothing was dropped after overrunning the buffer against a stalled writer, " +
			"so something must have blocked to keep up")
	}
	close(out.release)
}

// A gap in the log must not be silent — the lines that were lost are exactly the
// ones someone would go looking for.
func TestNonBlockingLogReportsWhatItDropped(t *testing.T) {
	out := &stalledWriter{release: make(chan struct{})}
	w := newNonBlockingLog(out)

	for range logBufferLines * 2 {
		w.Write([]byte("a log line\n"))
	}
	if w.dropped.Load() == 0 {
		t.Skip("nothing dropped; the drain kept up with the burst")
	}
	close(out.release)

	// Once draining resumes, the next line through carries the notice.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		w.Write([]byte("after recovery\n"))
		if strings.Contains(out.String(), "dropped") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no drop notice after recovery; output was:\n%s", out.String())
}
