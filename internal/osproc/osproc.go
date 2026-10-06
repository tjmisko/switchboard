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

	"github.com/tjmisko/switchboard/internal/proc"
)

// Info is the neutral process record. TTY is an opaque join key whose literal
// form is OS-specific (/dev/pts/N on Linux, /dev/ttysNNN on macOS); consumers
// treat it as a join key, never parse the prefix. Args is the full argv (argv[0]
// is the program path), used by discovery to tell an interactive agent session
// apart from a background subcommand (e.g. `claude daemon run`); it is nil when
// the kernel masks it (zombies, kernel threads).
type Info struct {
	PID  int
	PPID int
	Comm string
	Exe  string
	CWD  string
	TTY  string
	Args []string
	// StdinTTY is true when fd 0 is the terminal (Linux: a /dev/pts link). It is
	// the interactive gate for agents whose comm, exe and argv cannot tell a TUI
	// from a non-interactive child (discovery.IsPi).
	StdinTTY bool
	// Birth is the OS process-birth token: opaque, and equal across two reads
	// only when they saw the same process lifetime. Linux joins the boot id with
	// /proc/<pid>/stat's starttime; the darwin stub has none. Empty means
	// unverified (see CompareBirth). It is not the session's display start time.
	Birth string
}

// Lifetime names one process lifetime: a pid as held by one process from its
// birth to its death. A reused pid is a different Lifetime because its Birth
// differs. A Lifetime with an empty Birth is unverified.
type Lifetime struct {
	PID   int
	Birth string
}

// Lifetime returns the process lifetime this record was read from.
func (i Info) Lifetime() Lifetime { return Lifetime{PID: i.PID, Birth: i.Birth} }

// Verified reports whether the lifetime carries a birth token.
func (l Lifetime) Verified() bool { return l.Birth != "" }

// BirthMatch is the outcome of comparing two birth tokens of one pid.
type BirthMatch int

const (
	// BirthUnverified means at least one token is missing, so nothing is known
	// about whether the two reads saw the same process. It never counts as a
	// match: an unverified lifetime inherits no authority.
	BirthUnverified BirthMatch = iota
	// BirthSame means both tokens are present and equal: one lifetime.
	BirthSame
	// BirthDifferent means both tokens are present and differ: the pid was
	// reused (or the host rebooted) between the two reads.
	BirthDifferent
)

// CompareBirth compares two birth tokens read for the same pid.
func CompareBirth(a, b string) BirthMatch {
	switch {
	case a == "" || b == "":
		return BirthUnverified
	case a == b:
		return BirthSame
	default:
		return BirthDifferent
	}
}

// FromProc projects a proc.Info onto the neutral record. It lives here rather
// than in the Linux backend (its only caller until now) because callers ABOVE
// the seam hold a proc.Info too — the RPC hook path walks the ppid chain with
// proc.Read and must ask discovery what it is looking at — and internal/proc
// carries no build tags, so this compiles on every platform.
func FromProc(p proc.Info) Info {
	return Info{PID: p.PID, PPID: p.PPID, Comm: p.Comm, Exe: p.Exe, CWD: p.CWD, TTY: p.TTY, Args: p.Args, StdinTTY: p.StdinTTY, Birth: p.Birth}
}

// ErrGone means the process disappeared between enumeration and read (the most
// common race). Callers should treat it as benign.
var ErrGone = errors.New("process gone")

// ErrUnsupported is returned by a backend that does not implement the OS
// process layer on the current platform (the darwin stub until #13).
var ErrUnsupported = errors.New("osproc: unsupported on this platform")

// ErrUnverifiedLifetime is Watch's refusal of a lifetime without a birth
// token: the watcher could not prove which process its handle refers to, so a
// death it reported could belong to a replacement. The daemon's liveness sweep
// closes such a session instead.
var ErrUnverifiedLifetime = errors.New("osproc: lifetime has no birth token")

// Source enumerates processes and signals once when a watched pid dies. The
// death signal is observed (via a kernel handle), never inferred from polling
// state — onDeath fires exactly once regardless of how the process died.
type Source interface {
	// Enumerate returns every process the backend can see. Processes that
	// disappear mid-read are skipped (not an error).
	Enumerate() ([]Info, error)
	// Read returns one process by pid, or ErrGone if it has disappeared.
	Read(pid int) (Info, error)
	// Watch calls onDeath exactly once, from a background goroutine, when the
	// lifetime dies. The backend proves its kernel handle refers to that
	// lifetime by re-reading the birth token after opening it; a pid already
	// held by another lifetime reports the watched one dead at once. A lifetime
	// without a birth token is refused with ErrUnverifiedLifetime. A duplicate
	// Watch for a lifetime already watched is a no-op; a lifetime whose onDeath
	// has begun is no longer watched, so watching it again registers anew.
	Watch(ctx context.Context, lifetime Lifetime, onDeath func()) error
	// Stop cancels the watcher for exactly this lifetime (if any) without
	// firing onDeath. A watcher of another lifetime of the same pid is untouched.
	Stop(lifetime Lifetime)
}

// New returns the OS process source for the current platform.
func New() Source { return newSource() }
