package testsupport

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// WeztermRuntime is a fake $XDG_RUNTIME_DIR containing a wezterm/ subdirectory,
// for exercising wezterm.Muxes (which enumerates gui-sock-<pid> entries and
// keeps only those whose owning pid is alive). It points XDG_RUNTIME_DIR at a
// temp dir for the duration of the test via t.Setenv.
type WeztermRuntime struct {
	Dir        string // the fake XDG_RUNTIME_DIR
	WeztermDir string // <Dir>/wezterm
}

// NewWeztermRuntime creates the temp runtime dir, sets XDG_RUNTIME_DIR to it,
// and creates the wezterm/ subdirectory.
func NewWeztermRuntime(t testing.TB) *WeztermRuntime {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	weztermDir := filepath.Join(dir, "wezterm")
	if err := os.MkdirAll(weztermDir, 0o755); err != nil {
		t.Fatalf("mkdir wezterm dir: %v", err)
	}
	return &WeztermRuntime{Dir: dir, WeztermDir: weztermDir}
}

// AddMux creates a gui-sock-<pid> entry. wezterm.Muxes only reads the dir name
// and stats /proc/<pid>, so a plain regular file is enough to be discovered;
// liveness is decided by whether pid is a running process. Use LivePID() for a
// mux that should survive the filter and DeadPID() for one that should be
// skipped.
func (w *WeztermRuntime) AddMux(t testing.TB, pid int) {
	t.Helper()
	w.AddEntry(t, fmt.Sprintf("gui-sock-%d", pid))
}

// AddEntry creates an arbitrary entry (e.g. junk that is not a gui-sock-, or a
// gui-sock- with a non-numeric suffix) to test the filter's negative paths.
func (w *WeztermRuntime) AddEntry(t testing.TB, name string) {
	t.Helper()
	path := filepath.Join(w.WeztermDir, name)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("create wezterm entry %s: %v", name, err)
	}
}

// LivePID returns a pid guaranteed to be alive for the duration of the test:
// the test process itself.
func LivePID() int { return os.Getpid() }

// DeadPID returns a pid that is (essentially certainly) not a running process,
// so a gui-sock-<DeadPID> entry is filtered out as a stale socket.
func DeadPID() int {
	// 0x40000000 is far above the default pid_max on Linux/macOS, so /proc/<pid>
	// (and the kernel) will not have it.
	return 1 << 30
}
