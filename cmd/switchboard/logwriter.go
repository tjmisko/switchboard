package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"sync/atomic"
)

// nonBlockingLog makes log.Printf incapable of blocking its caller.
//
// This is not a performance tweak. The daemon logs from inside store.Apply — the
// liveness sweep names the pid whose lane it closed, and both status self-heals
// log the rule behind every hookless transition, which is the only trace those
// edges leave. log.Printf writes to stderr synchronously, so when stderr is a pipe
// whose reader has stopped draining (journald wedged, a terminal that stopped
// consuming, a `| head` on a manual run), the 64 KiB pipe buffer fills and the
// write(2) blocks WITH THE EXCLUSIVE LOCK HELD. Every RPC reader, every hook and
// every waybar subscriber then waits on a log line — an unbounded stall,
// structurally worse than any of the millisecond reads this daemon works to keep
// out of the lock, and one that no amount of hoisting reads would have found.
//
// history.Sink.Record already answers this question for events: buffer, and DROP
// rather than block the daemon. This applies the same answer to the log, and does
// it at the writer rather than at each call site, so a log.Printf added inside a
// future Apply — the likeliest way for this to come back — is covered by
// construction.
//
// Dropping is the deliberate trade: log lines are diagnostics, and losing some of
// them while the log pipe is wedged is strictly better than freezing the status bar
// until it drains. Drops are counted and reported once the drain recovers, so a gap
// is never silent.
type nonBlockingLog struct {
	lines   chan []byte
	dropped atomic.Int64
}

// logBufferLines bounds the queue. Deep enough to swallow a burst — a tick that
// self-heals every session logs one line each — shallow enough that a wedged
// stderr costs a bounded amount of memory rather than growing until the OOM killer
// notices.
const logBufferLines = 1024

// newNonBlockingLog starts the drain goroutine and returns the writer to install
// with log.SetOutput. It runs for the life of the process: there is no shutdown,
// because a log writer that stops accepting writes during shutdown is exactly the
// failure this exists to prevent.
func newNonBlockingLog(out io.Writer) *nonBlockingLog {
	w := &nonBlockingLog{lines: make(chan []byte, logBufferLines)}
	go w.drain(out)
	return w
}

// Write queues a copy of the line. log.Logger reuses its formatting buffer across
// calls, so the copy is required, not defensive.
func (w *nonBlockingLog) Write(p []byte) (int, error) {
	line := make([]byte, len(p))
	copy(line, p)
	select {
	case w.lines <- line:
	default:
		w.dropped.Add(1)
	}
	return len(p), nil
}

// drain is the only goroutine that touches out, so a blocking write blocks it
// alone. Write errors are discarded: there is nowhere left to report them, and a
// log writer that died on a broken stderr would take the daemon with it.
//
// The drop notice rides the next line that gets through, which is why a gap is
// reported on recovery rather than as it happens — there is by definition no way
// to report it while the writer is wedged.
func (w *nonBlockingLog) drain(out io.Writer) {
	for line := range w.lines {
		if n := w.dropped.Swap(0); n > 0 {
			fmt.Fprintf(out, "switchboard: dropped %d log lines (stderr was not draining)\n", n)
		}
		out.Write(line)
	}
}

// installNonBlockingLog points the standard logger at a writer that cannot block.
func installNonBlockingLog() {
	log.SetOutput(newNonBlockingLog(os.Stderr))
}
