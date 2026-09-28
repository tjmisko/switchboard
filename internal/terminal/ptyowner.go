package terminal

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ptyOwnerLocator resolves ttys hosted by a terminal with no IPC and no panes
// below the window (foot, Alacritty). With nothing to ask, it reads the answer
// from the kernel: the process holding a pty's MASTER side is the terminal
// drawing it, and the master's /proc/<pid>/fdinfo/<fd> names the pty it drives
// ("tty-index: N" → /dev/pts/N). That makes the tty → terminal process hop
// exact, with no title guessing and no dependence on the agent's ancestry.
//
// One window shows exactly one pty, so focusing the session is focusing its OS
// window, which the WM seam does. The window join keys on the terminal pid
// alone (PaneRef.WindowTitle is left empty — there is no title to read). That is
// exact when every window is its own process, and ambiguous when one process
// hosts several windows (`foot --server`, `alacritty msg create-window`); the
// mapping layer fails closed on that ambiguity rather than guessing.
type ptyOwnerLocator struct {
	name string // backend name reported in capabilities and PaneRef.Backend
	exe  string // basename of the terminal binary
	scan *processScan
}

func newPtyOwner(name, exe string, scan *processScan) ptyOwnerLocator {
	return ptyOwnerLocator{name: name, exe: exe, scan: scan}
}

func (l ptyOwnerLocator) Name() string { return l.name }

// Available reports whether any process runs the terminal's binary. It reads
// /proc without spawning, so auto can re-probe it every call and light the
// backend up once the first window opens after the daemon.
func (l ptyOwnerLocator) Available() bool {
	return len(l.scan.pidsRunning(l.exe)) > 0
}

func (l ptyOwnerLocator) Locate(ctx context.Context, tty string) (*PaneRef, error) {
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

// Snapshot indexes every pty the terminal's processes drive by slave tty. Locate
// is answered from the same scan so the single and batch paths cannot drift.
//
// A process that exits mid-scan simply contributes nothing: its ptys are gone
// with it, so omitting them is the true answer, not an incomplete one. The map
// is therefore always complete and the scan never errors.
func (l ptyOwnerLocator) Snapshot(context.Context) (map[string]PaneRef, error) {
	panes := make(map[string]PaneRef)
	for _, pid := range l.scan.pidsRunning(l.exe) {
		// The process list may be up to one TTL old; re-check that the pid still
		// runs this binary so a recycled pid can never lend its ptys to us.
		if !l.scan.runs(pid, l.exe) {
			continue
		}
		for _, index := range l.scan.ptyMasterIndexes(pid) {
			tty := "/dev/pts/" + strconv.Itoa(index)
			panes[tty] = PaneRef{
				Backend: l.name,
				Mux:     pid,
				PaneID:  index,
				TTY:     tty,
			}
		}
	}
	return panes, nil
}

// Activate has nothing to select: the window is a single pty, and raising it is
// the WM seam's job. ErrUnsupported tells the caller no step finer than the
// window exists, so focus succeeds exactly when the WM step does.
func (ptyOwnerLocator) Activate(context.Context, *PaneRef) error { return ErrUnsupported }

// processScanTTL bounds how long one /proc walk answers for. Within a reconcile
// the auto locator asks every backend Available() and then Snapshot(), so an
// uncached walk would repeat per backend per question. A quarter second
// collapses those into one walk and is far below any cadence a user notices: a
// terminal opened just after a walk is seen on the next reconcile.
const processScanTTL = 250 * time.Millisecond

// processScan indexes /proc by executable, shared by every pty-owner backend so
// adding a terminal does not add a walk over every process on the host.
//
// The discriminator is the exe link, read with one raw readlink per process:
// the obvious os.ReadFile of comm measured ~11x slower (3.9 ms vs 0.34 ms over
// 364 pids) for its open/fstat/read/close. It is also the more faithful key — it
// names the binary actually executing whatever the process was launched as, and
// a process of another user simply fails the readlink and is skipped, as it
// would be when its fds were read.
type processScan struct {
	root string
	ttl  time.Duration
	now  func() time.Time

	mu    sync.Mutex
	at    time.Time
	byExe map[string][]int // exe basename → pids, from the last walk
}

func newProcessScan(root string, ttl time.Duration) *processScan {
	return &processScan{root: root, ttl: ttl, now: time.Now}
}

// pidsRunning returns the pids whose executable basename is exe, walking /proc
// again once the last walk is older than the TTL (or the clock stepped back).
func (s *processScan) pidsRunning(exe string) []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if s.byExe == nil || now.Sub(s.at) >= s.ttl || now.Before(s.at) {
		s.byExe = s.walk()
		s.at = now
	}
	return s.byExe[exe]
}

func (s *processScan) walk() map[string][]int {
	byExe := make(map[string][]int)
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return byExe
	}
	buf := make([]byte, 512)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		if exe, ok := s.exeBase(pid, buf); ok {
			byExe[exe] = append(byExe[exe], pid)
		}
	}
	return byExe
}

// runs re-reads one pid's exe link: the per-use check that keeps a cached pid
// list from outliving a pid's reuse.
func (s *processScan) runs(pid int, exe string) bool {
	got, ok := s.exeBase(pid, make([]byte, 512))
	return ok && got == exe
}

// exeBase reads the basename of a pid's executable. A binary replaced by a
// package upgrade while it runs reads "<path> (deleted)"; that process is still
// the terminal and still drives its ptys.
func (s *processScan) exeBase(pid int, buf []byte) (string, bool) {
	n, err := syscall.Readlink(filepath.Join(s.root, strconv.Itoa(pid), "exe"), buf)
	if err != nil || n <= 0 || n == len(buf) {
		return "", false
	}
	return filepath.Base(strings.TrimSuffix(string(buf[:n]), " (deleted)")), true
}

// ptyMasterIndexes returns the pts index of every pty master pid holds open.
// The fd link names the multiplexor (/dev/ptmx, or /dev/pts/ptmx when devpts is
// mounted with its own ptmx node); the fdinfo's tty-index names the slave.
func (s *processScan) ptyMasterIndexes(pid int) []int {
	fdDir := filepath.Join(s.root, strconv.Itoa(pid), "fd")
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
		info := filepath.Join(s.root, strconv.Itoa(pid), "fdinfo", entry.Name())
		if index, ok := readTTYIndex(info); ok {
			indexes = append(indexes, index)
		}
	}
	return indexes
}

// socketInodes returns the inode of every socket pid holds open, read from its
// fd links ("socket:[N]") — the key the kernel socket table is joined on.
func (s *processScan) socketInodes(pid int) []uint64 {
	fdDir := filepath.Join(s.root, strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return nil
	}
	var inodes []uint64
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join(fdDir, entry.Name()))
		if err != nil {
			continue
		}
		digits, ok := strings.CutPrefix(target, "socket:[")
		if !ok {
			continue
		}
		inode, err := strconv.ParseUint(strings.TrimSuffix(digits, "]"), 10, 64)
		if err == nil {
			inodes = append(inodes, inode)
		}
	}
	return inodes
}

// statFields returns /proc/<pid>/stat's fields after the parenthesised comm,
// which may itself contain spaces and parentheses: index 0 is field 3 (state).
func (s *processScan) statFields(pid int) []string {
	data, err := os.ReadFile(filepath.Join(s.root, strconv.Itoa(pid), "stat"))
	if err != nil {
		return nil
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return nil
	}
	return strings.Fields(string(data[end+1:]))
}

// controllingTTY returns pid's controlling terminal as "/dev/pts/N" when it is a
// pty. It reads stat's tty_nr rather than fd links: the controlling terminal is
// the kernel's answer, where a process's stdio may be redirected anywhere.
func (s *processScan) controllingTTY(pid int) (string, bool) {
	fields := s.statFields(pid)
	if pid <= 0 || len(fields) < 5 {
		return "", false
	}
	ttyNr, err := strconv.ParseUint(fields[4], 10, 32)
	if err != nil || ttyNr == 0 {
		return "", false
	}
	index, ok := ptsIndexFromDev(uint32(ttyNr))
	if !ok {
		return "", false
	}
	return "/dev/pts/" + strconv.Itoa(index), true
}

// startTime returns pid's start time in clock ticks since boot (stat field 22),
// or 0 when unreadable.
func (s *processScan) startTime(pid int) uint64 {
	fields := s.statFields(pid)
	if len(fields) < 20 {
		return 0
	}
	start, _ := strconv.ParseUint(fields[19], 10, 64)
	return start
}

// unix98PTYSlaveMajor is the character major of every /dev/pts/N.
const unix98PTYSlaveMajor = 136

// ptsIndexFromDev decodes a kernel-encoded dev_t (new_encode_dev: 12-bit major,
// 20-bit minor split around it) and returns the pts index when it is a pty slave.
func ptsIndexFromDev(dev uint32) (int, bool) {
	major := (dev >> 8) & 0xfff
	minor := (dev & 0xff) | ((dev >> 12) & 0xfff00)
	if major != unix98PTYSlaveMajor {
		return 0, false
	}
	return int(minor), true
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
