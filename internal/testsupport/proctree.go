package testsupport

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// ProcStatus returns a realistic /proc/<pid>/status body whose PPid line
// carries ppid. The surrounding fields mirror the kernel's format (tab-aligned
// values, a Name line, an adjacent Tgid) so a parser keyed on the "PPid:"
// prefix is exercised against true-to-life input rather than a bare line.
func ProcStatus(ppid int) string {
	return fmt.Sprintf("Name:\tclaude\nUmask:\t0022\nState:\tS (sleeping)\nTgid:\t%d\nNgid:\t0\nPid:\t%d\nPPid:\t%d\nTracerPid:\t0\n",
		ppid+1, ppid+1, ppid)
}

// ProcSpec describes one process to lay down in a FakeProcTree.
type ProcSpec struct {
	Comm string // /proc/<pid>/comm contents (newline appended automatically)
	PPid int    // value embedded in /proc/<pid>/status
	Exe  string // target of the /proc/<pid>/exe symlink ("" to omit)
	CWD  string // target of the /proc/<pid>/cwd symlink ("" to omit)
	TTY  string // target of /proc/<pid>/fd/0 ("" to omit; e.g. "/dev/pts/3")
}

// FakeProcTree is a temp directory shaped like a subset of /proc. It is the
// fixture the future injectable procSource (plan §0.5) reads from; until that
// seam lands, its reader helpers let tests assert the on-disk shape directly.
type FakeProcTree struct {
	Root string
}

// NewFakeProcTree creates an empty fake /proc rooted at a temp dir.
func NewFakeProcTree(t testing.TB) *FakeProcTree {
	t.Helper()
	return &FakeProcTree{Root: t.TempDir()}
}

// AddProcess writes the comm/status files and the exe/cwd/fd symlinks for pid
// according to spec. Symlink targets need not exist (matching how /proc points
// at deleted exes / unreachable cwds).
func (p *FakeProcTree) AddProcess(t testing.TB, pid int, spec ProcSpec) {
	t.Helper()
	dir := filepath.Join(p.Root, fmt.Sprintf("%d", pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	WriteFile(t, filepath.Join(dir, "comm"), spec.Comm+"\n")
	WriteFile(t, filepath.Join(dir, "status"), ProcStatus(spec.PPid))
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

// PIDDir returns the directory for pid within the tree.
func (p *FakeProcTree) PIDDir(pid int) string {
	return filepath.Join(p.Root, fmt.Sprintf("%d", pid))
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
