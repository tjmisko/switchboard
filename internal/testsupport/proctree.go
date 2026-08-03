package testsupport

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// stateLabels maps the kernel's single-char run-state codes to the
// parenthetical labels /proc/<pid>/status writes beside them, so a fixture
// exercises a parser against the real two-token shape rather than a bare
// letter.
var stateLabels = map[string]string{
	"R": "running",
	"S": "sleeping",
	"D": "disk sleep",
	"T": "stopped",
	"t": "tracing stop",
	"Z": "zombie",
	"X": "dead",
	"I": "idle",
}

// ProcStatus returns a realistic /proc/<pid>/status body whose PPid line
// carries ppid, in the sleeping state. The surrounding fields mirror the
// kernel's format (tab-aligned values, a Name line, an adjacent Tgid) so a
// parser keyed on the "PPid:" prefix is exercised against true-to-life input
// rather than a bare line.
func ProcStatus(ppid int) string { return ProcStatusState(ppid, "S") }

// ProcStatusState is ProcStatus with a settable run state — "T" for a
// Ctrl-Z'd process, "Z" for a zombie, and so on. An unrecognized code is
// written without a parenthetical label.
func ProcStatusState(ppid int, state string) string {
	stateLine := state
	if label, ok := stateLabels[state]; ok {
		stateLine = state + " (" + label + ")"
	}
	return fmt.Sprintf("Name:\tclaude\nUmask:\t0022\nState:\t%s\nTgid:\t%d\nNgid:\t0\nPid:\t%d\nPPid:\t%d\nTracerPid:\t0\n",
		stateLine, ppid+1, ppid+1, ppid)
}

// ProcStat returns a realistic /proc/<pid>/stat line: the pid, the comm in
// parens, the run state, the ppid, and enough trailing numeric fields to look
// like the real thing.
//
// comm is written verbatim inside the parens. The kernel permits spaces and
// parens there and real processes use them ("(Web Content)", "((sd-pam))"), so
// a fixture can carry one and catch a parser that counts fields from the left
// instead of scanning back from the last ')'.
func ProcStat(pid, ppid int, comm, state string) string {
	if state == "" {
		state = "S"
	}
	return fmt.Sprintf("%d (%s) %s %d %d %d 0 -1 4194304 177 0 0 0 0 0 0 0 20 0 2 0 30634763 79282176 295\n",
		pid, comm, state, ppid, pid, pid)
}

// SmapsRollup returns a realistic /proc/<pid>/smaps_rollup body reporting
// pssKB and swapPssKB, in the kB the kernel actually writes.
//
// The neighbouring Pss_Dirty/Pss_Anon/Pss_File/Pss_Shmem lines and the Swap
// line are deliberately given values that differ from Pss and SwapPss, so a
// parser that prefix-matches "Pss" or "Swap" reads a wrong number rather than
// coincidentally the right one.
func SmapsRollup(pssKB, swapPssKB int) string {
	return fmt.Sprintf(`00200000-ffffde214000 ---p 00000000 00:00 0                              [rollup]
Rss:            %7d kB
Pss:            %7d kB
Pss_Dirty:      %7d kB
Pss_Anon:       %7d kB
Pss_File:       %7d kB
Pss_Shmem:            0 kB
Shared_Clean:     34544 kB
Shared_Dirty:         0 kB
Private_Clean:    41904 kB
Private_Dirty:  %7d kB
Referenced:     %7d kB
Anonymous:      %7d kB
KSM:                  0 kB
LazyFree:             0 kB
AnonHugePages:        0 kB
ShmemPmdMapped:       0 kB
FilePmdMapped:        0 kB
Shared_Hugetlb:       0 kB
Private_Hugetlb:      0 kB
Swap:           %7d kB
SwapPss:        %7d kB
Locked:               0 kB
`,
		pssKB+2000, pssKB, pssKB/3, pssKB/2, pssKB/7,
		pssKB/2, pssKB+1000, pssKB/2,
		swapPssKB+500, swapPssKB)
}

// MemInfo returns a realistic /proc/meminfo body reporting availKB as
// MemAvailable. MemFree and MemTotal are given different values so a parser
// that matches "Mem" by prefix is caught.
func MemInfo(availKB int) string {
	return fmt.Sprintf("MemTotal:       15960352 kB\nMemFree:        %8d kB\nMemAvailable:   %8d kB\nBuffers:           31776 kB\nCached:          3128560 kB\nSwapCached:       288640 kB\nSwapTotal:      24348624 kB\nSwapFree:       13798512 kB\n",
		availKB/2, availKB)
}

// PressureMemory returns a realistic /proc/pressure/memory body. The `full`
// line is given values that differ from the `some` line so a parser that reads
// whichever it sees last, or matches the wrong one, is caught.
func PressureMemory(someAvg10 float64, someTotalUS int64) string {
	return fmt.Sprintf("some avg10=%.2f avg60=1.11 avg300=2.22 total=%d\nfull avg10=%.2f avg60=3.33 avg300=4.44 total=%d\n",
		someAvg10, someTotalUS, someAvg10/2, someTotalUS/2)
}

// ProcSpec describes one process to lay down in a FakeProcTree.
type ProcSpec struct {
	Comm   string // /proc/<pid>/comm contents (newline appended automatically)
	PPid   int    // value embedded in /proc/<pid>/{status,stat}
	Exe    string // target of the /proc/<pid>/exe symlink ("" to omit)
	CWD    string // target of the /proc/<pid>/cwd symlink ("" to omit)
	TTY    string // target of /proc/<pid>/fd/0 ("" to omit; e.g. "/dev/pts/3")
	State  string // single-char run state for status/stat ("" defaults to "S")
	Rollup *RollupKB
}

// RollupKB is the smaps_rollup reading for one process, in the kB the kernel
// reports. A nil *RollupKB on a ProcSpec omits the file entirely, which is how
// a kernel thread, a process we may not read, and one that died mid-walk all
// present — the cases the tree walk must skip rather than fail on.
type RollupKB struct {
	Pss     int
	SwapPss int
}

// FakeProcTree is a temp directory shaped like a subset of /proc. Hand its
// Root to proc.NewReader to point the real readers at it.
type FakeProcTree struct {
	Root string
}

// NewFakeProcTree creates an empty fake /proc rooted at a temp dir.
func NewFakeProcTree(t testing.TB) *FakeProcTree {
	t.Helper()
	return &FakeProcTree{Root: t.TempDir()}
}

// AddProcess writes the comm/status/stat files, the optional smaps_rollup, and
// the exe/cwd/fd symlinks for pid according to spec. Symlink targets need not
// exist (matching how /proc points at deleted exes / unreachable cwds).
func (p *FakeProcTree) AddProcess(t testing.TB, pid int, spec ProcSpec) {
	t.Helper()
	dir := p.PIDDir(pid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	WriteFile(t, filepath.Join(dir, "comm"), spec.Comm+"\n")
	WriteFile(t, filepath.Join(dir, "status"), ProcStatusState(spec.PPid, statusState(spec.State)))
	WriteFile(t, filepath.Join(dir, "stat"), ProcStat(pid, spec.PPid, spec.Comm, spec.State))
	if spec.Rollup != nil {
		WriteFile(t, filepath.Join(dir, "smaps_rollup"), SmapsRollup(spec.Rollup.Pss, spec.Rollup.SwapPss))
	}
	symlinkIf(t, spec.Exe, filepath.Join(dir, "exe"))
	symlinkIf(t, spec.CWD, filepath.Join(dir, "cwd"))
	if spec.TTY != "" {
		fdDir := filepath.Join(dir, "fd")
		if err := os.MkdirAll(fdDir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", fdDir, err)
		}
		symlinkIf(t, spec.TTY, filepath.Join(fdDir, "0"))
	}
}

// RemoveProcess deletes pid's directory, standing in for a process that exits
// between two reads of the tree.
func (p *FakeProcTree) RemoveProcess(t testing.TB, pid int) {
	t.Helper()
	if err := os.RemoveAll(p.PIDDir(pid)); err != nil {
		t.Fatalf("remove %s: %v", p.PIDDir(pid), err)
	}
}

// SetMemInfo writes body to <root>/meminfo.
func (p *FakeProcTree) SetMemInfo(t testing.TB, body string) {
	t.Helper()
	WriteFile(t, filepath.Join(p.Root, "meminfo"), body)
}

// SetPressureMemory writes body to <root>/pressure/memory. Simply not calling
// it is how a kernel built without CONFIG_PSI presents.
func (p *FakeProcTree) SetPressureMemory(t testing.TB, body string) {
	t.Helper()
	WriteFile(t, filepath.Join(p.Root, "pressure", "memory"), body)
}

// PIDDir returns the directory for pid within the tree.
func (p *FakeProcTree) PIDDir(pid int) string {
	return filepath.Join(p.Root, fmt.Sprintf("%d", pid))
}

func statusState(state string) string {
	if state == "" {
		return "S"
	}
	return state
}

func symlinkIf(t testing.TB, target, link string) {
	t.Helper()
	if target == "" {
		return
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink %s -> %s: %v", link, target, err)
	}
}
