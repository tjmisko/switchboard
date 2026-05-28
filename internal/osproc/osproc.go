// Package osproc is the Seam-1 OS process layer: enumerate processes, read one
// by pid, and signal exactly once when a watched pid dies. The concrete Source
// is per-OS — Linux uses /proc + pidfd_open(2); macOS will use libproc +
// kqueue (Phase 4) — and is selected at runtime by New(). Only this seam is
// build-tagged per OS; nothing above it is.
//
// Source adopts the backend-agnostic contract in internal/conformance
// (RunSourceContract): assertions are written against neutral observables
// (non-empty tty, ErrGone, onDeath-exactly-once), never against /proc or the
// /dev/pts literal, so the same suite validates the macOS backend unchanged.
package osproc

import (
	"context"
	"errors"
)

// Info is the neutral process record. TTY is an opaque join key whose literal
// form is OS-specific (/dev/pts/N on Linux, /dev/ttysNNN on macOS); consumers
// treat it as a join key, never parse the prefix.
type Info struct {
	PID  int
	PPID int
	Comm string
	Exe  string
	CWD  string
	TTY  string
}

// ErrGone means the process disappeared between enumeration and read (the most
// common race). Callers should treat it as benign.
var ErrGone = errors.New("process gone")

// ErrUnsupported is returned by a backend that does not implement the OS
// process layer on the current platform (the darwin stub until Phase 4).
var ErrUnsupported = errors.New("osproc: unsupported on this platform")

// Source enumerates processes and signals once when a watched pid dies. The
// death signal is observed (via a kernel handle), never inferred from polling
// state — onDeath fires exactly once regardless of how the process died.
type Source interface {
	// Enumerate returns every process the backend can see. Processes that
	// disappear mid-read are skipped (not an error).
	Enumerate() ([]Info, error)
	// Read returns one process by pid, or ErrGone if it has disappeared.
	Read(pid int) (Info, error)
	// Watch calls onDeath exactly once, from a background goroutine, when pid
	// dies. A duplicate Watch for a pid already watched is a no-op.
	Watch(ctx context.Context, pid int, onDeath func()) error
	// Stop cancels the watcher for pid (if any) without firing onDeath.
	Stop(pid int)
}

// New returns the OS process source for the current platform.
func New() Source { return newSource() }
