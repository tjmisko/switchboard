// Package proc reads process metadata from /proc.
package proc

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Info struct {
	PID   int
	PPID  int
	Comm  string
	Exe   string
	CWD   string
	TTY   string   // e.g. "/dev/pts/2" or "" if not a tty-attached process
	Args  []string // full argv from /proc/<pid>/cmdline; nil when the kernel masks it (zombies, kernel threads)
	State string   // single-char run state from /proc/<pid>/status (R/S/D/T/t/Z/...)
}

// Reader reads from a /proc-shaped directory tree. Its zero value reads the
// real /proc, so the package-level functions below are the whole API for
// production callers; NewReader roots it at a fixture directory instead, which
// is what makes these paths unit-testable without spawning real processes.
type Reader struct {
	root string // "" means the real /proc
}

// NewReader returns a Reader rooted at root instead of /proc.
func NewReader(root string) *Reader { return &Reader{root: root} }

// hostProc reads the real /proc. Every package-level function delegates to it.
var hostProc = &Reader{}

func (r *Reader) procRoot() string {
	if r == nil || r.root == "" {
		return "/proc"
	}
	return r.root
}

// pidPath joins the per-process path elements under the reader's root, e.g.
// pidPath(42, "fd", "0") -> "/proc/42/fd/0".
func (r *Reader) pidPath(pid int, elem ...string) string {
	parts := make([]string, 0, len(elem)+2)
	parts = append(parts, r.procRoot(), strconv.Itoa(pid))
	parts = append(parts, elem...)
	return filepath.Join(parts...)
}

// Read collects /proc/<pid>/{comm,cmdline,exe,cwd,status,fd/0..2}. Returns
// ErrGone if the process disappeared mid-read (the most common race).
func Read(pid int) (Info, error) { return hostProc.Read(pid) }

func (r *Reader) Read(pid int) (Info, error) {
	out := Info{PID: pid}

	comm, err := readSmallFile(r.pidPath(pid, "comm"))
	if err != nil {
		return out, wrapGone(err)
	}
	out.Comm = strings.TrimRight(comm, "\n")

	out.Args = r.readArgs(pid)

	if exe, err := os.Readlink(r.pidPath(pid, "exe")); err == nil {
		out.Exe = exe
	}
	if cwd, err := os.Readlink(r.pidPath(pid, "cwd")); err == nil {
		out.CWD = cwd
	}

	status, err := readSmallFile(r.pidPath(pid, "status"))
	if err != nil {
		return out, wrapGone(err)
	}
	out.PPID = parsePPID(status)
	out.State = parseState(status)

	out.TTY = r.readTTY(pid)
	return out, nil
}

// State reads only /proc/<pid>/status and returns the single-char run-state
// code ("R", "S", "T", ...). Lighter than Read for the reconcile hot path,
// which re-checks suspension on every live session each tick. Returns ErrGone
// if the process vanished.
func State(pid int) (string, error) { return hostProc.State(pid) }

func (r *Reader) State(pid int) (string, error) {
	status, err := readSmallFile(r.pidPath(pid, "status"))
	if err != nil {
		return "", wrapGone(err)
	}
	return parseState(status), nil
}

// Suspended reports whether a run-state code denotes a job-control-stopped
// process — i.e. one paused by SIGSTOP/SIGTSTP (Ctrl-Z). "t" (tracing stop,
// e.g. under a debugger) is deliberately excluded.
func Suspended(state string) bool {
	return state == "T"
}

// readArgs reads /proc/<pid>/cmdline — the NUL-separated argv with a trailing
// NUL. Returns nil (not an error) when the kernel masks it: zombies and kernel
// threads have an empty cmdline. The argv lets discovery tell an interactive
// `claude` session apart from a `claude daemon …` background process, which
// shares the same comm and exe.
func (r *Reader) readArgs(pid int) []string {
	raw, err := readSmallFile(r.pidPath(pid, "cmdline"))
	if err != nil || raw == "" {
		return nil
	}
	args := strings.Split(strings.TrimRight(raw, "\x00"), "\x00")
	if len(args) == 1 && args[0] == "" {
		return nil
	}
	return args
}

// readTTY tries /proc/<pid>/fd/{0,1,2} for a /dev/pts/N link. Interactive TUIs
// like claude reliably have at least one of these attached to the controlling
// terminal. Returns "" if none of them point at a pts.
func (r *Reader) readTTY(pid int) string {
	for _, fd := range []int{0, 1, 2} {
		link, err := os.Readlink(r.pidPath(pid, "fd", strconv.Itoa(fd)))
		if err != nil {
			continue
		}
		if strings.HasPrefix(link, "/dev/pts/") {
			return link
		}
	}
	return ""
}

// AllPIDs lists every numeric entry under /proc. Cheap (one getdents).
func AllPIDs() ([]int, error) { return hostProc.AllPIDs() }

func (r *Reader) AllPIDs() ([]int, error) {
	entries, err := os.ReadDir(r.procRoot())
	if err != nil {
		return nil, err
	}
	pids := make([]int, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

// ErrGone means the process disappeared between when we listed it and when we
// tried to read it. Callers should treat this as benign.
var ErrGone = errors.New("process gone")

func wrapGone(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return ErrGone
	}
	return err
}

func readSmallFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func parsePPID(status string) int {
	for line := range strings.SplitSeq(status, "\n") {
		if rest, ok := strings.CutPrefix(line, "PPid:"); ok {
			ppid, _ := strconv.Atoi(strings.TrimSpace(rest))
			return ppid
		}
	}
	return 0
}

// parseState extracts the single-char run-state code from the status "State:"
// line, e.g. "State:\tT (stopped)" → "T". Returns "" if the line is absent or
// malformed.
func parseState(status string) string {
	for line := range strings.SplitSeq(status, "\n") {
		if rest, ok := strings.CutPrefix(line, "State:"); ok {
			fields := strings.Fields(rest)
			if len(fields) == 0 {
				return ""
			}
			return fields[0]
		}
	}
	return ""
}

// VmHWMKB reports the kernel's peak resident-set size ("high water mark") for
// THIS process in KB, and 0 when the figure is unreadable — a non-Linux kernel,
// or a /proc that does not carry the field.
//
// It lives here, beside the other /proc/<pid>/status readers, because two
// unrelated telemetry lines need it: fanout's per-seed `fanout-seed` line and
// state's per-minute `publish-stats` line (docs/telemetry.md). A second copy
// would be a second thing to keep honest, and the value is exactly the kind
// that must not quietly diverge between two lines a reader compares.
//
// Peak, not current: it never decreases for the life of the process, so it
// answers "how much has this daemon ever needed" rather than "how much does it
// hold now". That is deliberately the harder question — an OOM kill is decided
// by the peak — and it is why the figure is worth carrying on a line that is
// otherwise about the current minute. Current RSS is VmRSS, which no caller has
// asked for yet.
func VmHWMKB() int64 { return hostProc.VmHWMKB(os.Getpid()) }

func (r *Reader) VmHWMKB(pid int) int64 {
	status, err := readSmallFile(r.pidPath(pid, "status"))
	if err != nil {
		return 0
	}
	return parseVmHWMKB(status)
}

// parseVmHWMKB extracts the KB figure from the status "VmHWM:" line, e.g.
// "VmHWM:\t   51200 kB" → 51200. Returns 0 if the line is absent or malformed;
// there is no sentinel for "unknown" because every caller prints the number and
// a 0 reads correctly as "not available here".
func parseVmHWMKB(status string) int64 {
	for line := range strings.SplitSeq(status, "\n") {
		rest, ok := strings.CutPrefix(line, "VmHWM:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return 0
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || kb < 0 {
			return 0
		}
		return kb
	}
	return 0
}
