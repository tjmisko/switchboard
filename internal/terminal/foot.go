package terminal

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// footLocator resolves ttys hosted by foot (codeberg.org/dnkl/foot), the
// default terminal on Omarchy. Foot has no IPC — nothing to list or focus
// windows with — so this backend reads the answer from the kernel instead: the
// process holding a pty's MASTER side is the terminal drawing it, and the
// master's /proc/<pid>/fdinfo/<fd> names the pty it drives ("tty-index: N" →
// /dev/pts/N). That makes the tty → foot process hop exact, with no title
// guessing and no dependence on the agent's process ancestry.
//
// Foot has no tabs or splits: one window shows exactly one pty. Focusing the
// session is therefore focusing its OS window, which the WM seam does. The
// window join keys on the foot pid alone (PaneRef.WindowTitle is left empty —
// foot exposes no title to read). That is exact for standalone foot, where every
// window is its own process (Omarchy launches `foot` this way), and ambiguous
// for `foot --server`, whose footclient windows all share the server's pid. The
// mapping layer fails closed on that ambiguity rather than guessing.
type footLocator struct {
	procRoot string
}

// NewFoot returns the foot terminal locator over the real /proc.
func NewFoot() Locator { return footLocator{procRoot: "/proc"} }

// newFootAt roots the locator at a /proc-shaped fixture tree.
func newFootAt(root string) footLocator { return footLocator{procRoot: root} }

func (footLocator) Name() string { return "foot" }

// Available reports whether any foot process is running. It is a read-only scan
// of /proc — no spawning — so auto can re-probe it every call and light foot up
// once the first window opens after the daemon.
func (l footLocator) Available() bool {
	return len(l.footPIDs()) > 0
}

func (l footLocator) Locate(ctx context.Context, tty string) (*PaneRef, error) {
	panes, err := l.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	pane, ok := panes[tty]
	if !ok {
		return nil, nil
	}
	return &pane, nil
}

// Snapshot indexes every pty a foot process drives by its slave tty. Locate is
// answered from the same scan so the single and batch paths cannot drift.
//
// A process that exits mid-scan simply contributes nothing: its ptys are gone
// with it, so omitting them is the true answer, not an incomplete one. The map
// is therefore always complete and the scan never errors.
func (l footLocator) Snapshot(context.Context) (map[string]PaneRef, error) {
	panes := make(map[string]PaneRef)
	for _, pid := range l.footPIDs() {
		for _, index := range l.ptyMasterIndexes(pid) {
			tty := "/dev/pts/" + strconv.Itoa(index)
			panes[tty] = PaneRef{
				Backend: "foot",
				Mux:     pid,
				PaneID:  index,
				TTY:     tty,
			}
		}
	}
	return panes, nil
}

// Activate has nothing to select: a foot window is a single pty, and raising the
// window is the WM seam's job. ErrUnsupported tells the caller that no step
// finer than the window exists, so focus succeeds exactly when the WM step does.
func (footLocator) Activate(context.Context, *PaneRef) error { return ErrUnsupported }

// footPIDs lists the processes running the foot binary. This covers both
// standalone foot and a `foot --server` (the process that owns the ptys);
// footclient is a different binary and owns no pty, so it is correctly excluded.
//
// The discriminator is the exe link, read with one raw readlink per process:
// it runs at reconcile cadence over every pid on the host, and the obvious
// os.ReadFile of comm measured ~11x slower (3.9 ms vs 0.34 ms over 364 pids)
// for its open/fstat/read/close. It is also the more faithful key — it names the
// binary actually executing whatever the process was launched as, and a
// process of another user simply fails the readlink and is skipped, as it
// would be when its fds were read.
func (l footLocator) footPIDs() []int {
	entries, err := os.ReadDir(l.procRoot)
	if err != nil {
		return nil
	}
	var pids []int
	buf := make([]byte, 512)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		n, err := syscall.Readlink(filepath.Join(l.procRoot, entry.Name(), "exe"), buf)
		if err != nil || n <= 0 || n == len(buf) {
			continue
		}
		if isFootExe(string(buf[:n])) {
			pids = append(pids, pid)
		}
	}
	return pids
}

// isFootExe reports whether an exe link target is the foot binary. A binary
// replaced by a package upgrade while foot runs reads "<path> (deleted)"; that
// process is still foot and still drives its ptys.
func isFootExe(target string) bool {
	return filepath.Base(strings.TrimSuffix(target, " (deleted)")) == "foot"
}

// ptyMasterIndexes returns the pts index of every pty master pid holds open.
// The fd link names the multiplexor (/dev/ptmx, or /dev/pts/ptmx when devpts is
// mounted with its own ptmx node); the fdinfo's tty-index names the slave.
func (l footLocator) ptyMasterIndexes(pid int) []int {
	fdDir := filepath.Join(l.procRoot, strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return nil
	}
	var indexes []int
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join(fdDir, entry.Name()))
		if err != nil || (target != "/dev/ptmx" && target != "/dev/pts/ptmx") {
			continue
		}
		info := filepath.Join(l.procRoot, strconv.Itoa(pid), "fdinfo", entry.Name())
		if index, ok := readTTYIndex(info); ok {
			indexes = append(indexes, index)
		}
	}
	return indexes
}

// readTTYIndex parses the "tty-index:" line of a pty master's fdinfo. A kernel
// too old to print the field yields no index, which leaves the session
// Observe-only rather than guessing.
func readTTYIndex(path string) (int, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		value, ok := strings.CutPrefix(sc.Text(), "tty-index:")
		if !ok {
			continue
		}
		index, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || index < 0 {
			return 0, false
		}
		return index, true
	}
	return 0, false
}
