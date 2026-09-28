package terminal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// fakeProc builds a /proc-shaped tree: an exe link per process, and per fd a
// symlink (targets need not exist — Readlink only reads the link) plus an
// optional fdinfo body.
type fakeProc struct {
	t    *testing.T
	root string
}

func newFakeProc(t *testing.T) fakeProc {
	t.Helper()
	return fakeProc{t: t, root: t.TempDir()}
}

func (p fakeProc) process(pid int, exe string) {
	p.t.Helper()
	dir := filepath.Join(p.root, strconv.Itoa(pid))
	for _, sub := range []string{"fd", "fdinfo"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			p.t.Fatal(err)
		}
	}
	if err := os.Symlink(exe, filepath.Join(dir, "exe")); err != nil {
		p.t.Fatal(err)
	}
}

func (p fakeProc) fd(pid, fd int, target, fdinfo string) {
	p.t.Helper()
	dir := filepath.Join(p.root, strconv.Itoa(pid))
	name := strconv.Itoa(fd)
	if err := os.Symlink(target, filepath.Join(dir, "fd", name)); err != nil {
		p.t.Fatal(err)
	}
	if fdinfo == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "fdinfo", name), []byte(fdinfo), 0o644); err != nil {
		p.t.Fatal(err)
	}
}

func ptmxInfo(index int) string {
	return "pos:\t0\nflags:\t02400002\nmnt_id:\t42\nino:\t88\ntty-index:\t" + strconv.Itoa(index) + "\n"
}

func TestFootShouldLocateTheFootProcessHoldingThePtyMasterWhenStandalone(t *testing.T) {
	p := newFakeProc(t)
	p.process(2001, "/usr/bin/foot")
	p.fd(2001, 0, "/dev/null", "")
	p.fd(2001, 7, "socket:[1234]", "")
	p.fd(2001, 12, "/dev/ptmx", ptmxInfo(4))

	pane, err := newFootAt(p.root).Locate(context.Background(), "/dev/pts/4")
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	want := PaneRef{Backend: "foot", Mux: 2001, PaneID: 4, TTY: "/dev/pts/4"}
	if pane == nil || *pane != want {
		t.Fatalf("Locate = %+v, want %+v", pane, want)
	}
}

func TestFootShouldGiveEachStandaloneWindowItsOwnProcess(t *testing.T) {
	p := newFakeProc(t)
	p.process(2001, "/usr/bin/foot")
	p.fd(2001, 12, "/dev/ptmx", ptmxInfo(4))
	p.process(2002, "/usr/bin/foot")
	p.fd(2002, 12, "/dev/ptmx", ptmxInfo(9))

	panes, err := newFootAt(p.root).Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(panes) != 2 || panes["/dev/pts/4"].Mux != 2001 || panes["/dev/pts/9"].Mux != 2002 {
		t.Fatalf("Snapshot = %+v, want pts/4→2001 and pts/9→2002", panes)
	}
}

// A foot server hosts every footclient window's pty in one process. Each tty
// must still resolve — to the server — so the mapping layer can see the shared
// pid and fail closed on the window join instead of this layer hiding it.
func TestFootShouldResolveEveryServerModeTTYToTheServerProcess(t *testing.T) {
	p := newFakeProc(t)
	p.process(3000, "/usr/bin/foot")
	p.fd(3000, 10, "/dev/ptmx", ptmxInfo(1))
	p.fd(3000, 11, "/dev/ptmx", ptmxInfo(2))
	p.fd(3000, 14, "/dev/ptmx", ptmxInfo(6))

	panes, _ := newFootAt(p.root).Snapshot(context.Background())
	for _, tty := range []string{"/dev/pts/1", "/dev/pts/2", "/dev/pts/6"} {
		if got := panes[tty]; got.Mux != 3000 || got.TTY != tty {
			t.Errorf("panes[%s] = %+v, want the server pid 3000", tty, got)
		}
	}
}

func TestFootShouldAcceptTheDevptsPtmxNodeWhenDevptsHasItsOwn(t *testing.T) {
	p := newFakeProc(t)
	p.process(2001, "/usr/bin/foot")
	p.fd(2001, 12, "/dev/pts/ptmx", ptmxInfo(3))

	pane, _ := newFootAt(p.root).Locate(context.Background(), "/dev/pts/3")
	if pane == nil || pane.Mux != 2001 {
		t.Fatalf("Locate = %+v, want foot pid 2001", pane)
	}
}

// Other terminals and multiplexers hold pty masters too. Claiming theirs would
// route a WezTerm or tmux session through a backend that cannot focus it.
func TestFootShouldIgnorePtyMastersHeldByOtherProcessesWhenScanning(t *testing.T) {
	p := newFakeProc(t)
	p.process(4000, "/usr/bin/wezterm-gui")
	p.fd(4000, 16, "/dev/ptmx", ptmxInfo(0))
	p.process(4001, "/usr/bin/tmux")
	p.fd(4001, 9, "/dev/ptmx", ptmxInfo(5))
	p.process(4002, "/usr/bin/footclient")
	p.fd(4002, 3, "socket:[99]", "")
	p.process(4003, "/usr/bin/footx")
	p.fd(4003, 4, "/dev/ptmx", ptmxInfo(8))

	l := newFootAt(p.root)
	panes, err := l.Snapshot(context.Background())
	if err != nil || len(panes) != 0 {
		t.Fatalf("Snapshot = (%+v, %v), want an empty complete set", panes, err)
	}
	if l.Available() {
		t.Fatal("Available() = true with no foot process, want false")
	}
}

// A slave tty held open by foot (or anything reading the terminal) is not the
// master; only the multiplexor side identifies the terminal drawing a pty.
func TestFootShouldIgnoreSlaveTTYsWhenFootHoldsThem(t *testing.T) {
	p := newFakeProc(t)
	p.process(2001, "/usr/bin/foot")
	p.fd(2001, 0, "/dev/pts/7", "pos:\t0\nflags:\t02\n")

	if panes, _ := newFootAt(p.root).Snapshot(context.Background()); len(panes) != 0 {
		t.Fatalf("Snapshot = %+v, want no pane for a slave fd", panes)
	}
}

func TestFootShouldSkipMastersWhoseFdinfoIsMissingOrMalformed(t *testing.T) {
	p := newFakeProc(t)
	p.process(2001, "/usr/bin/foot")
	p.fd(2001, 10, "/dev/ptmx", "")                                // raced away
	p.fd(2001, 11, "/dev/ptmx", "pos:\t0\nflags:\t02\n")           // kernel without tty-index
	p.fd(2001, 12, "/dev/ptmx", "tty-index:\tnope\n")              // garbage
	p.fd(2001, 13, "/dev/ptmx", "tty-index:\t-1\n")                // impossible
	p.fd(2001, 14, "/dev/ptmx", "tty-index:\t   11   \nino:\t3\n") // whitespace tolerated

	panes, err := newFootAt(p.root).Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(panes) != 1 || panes["/dev/pts/11"].Mux != 2001 {
		t.Fatalf("Snapshot = %+v, want only pts/11", panes)
	}
}

// A foot window closing between the /proc listing and the fd read is the
// common race; the process's ptys died with it, so it contributes nothing.
func TestFootShouldSkipAProcessWhenItExitsMidScan(t *testing.T) {
	p := newFakeProc(t)
	p.process(2001, "/usr/bin/foot")
	p.fd(2001, 12, "/dev/ptmx", ptmxInfo(4))
	p.process(2002, "/usr/bin/foot")
	if err := os.RemoveAll(filepath.Join(p.root, "2002", "fd")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(p.root, "2003"), 0o755); err != nil { // no exe link at all
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.root, "self"), nil, 0o644); err != nil { // non-numeric entry
		t.Fatal(err)
	}

	panes, err := newFootAt(p.root).Snapshot(context.Background())
	if err != nil || len(panes) != 1 || panes["/dev/pts/4"].Mux != 2001 {
		t.Fatalf("Snapshot = (%+v, %v), want only pts/4 from pid 2001", panes, err)
	}
}

func TestFootShouldReturnNoPaneWithoutErrorWhenTTYIsUnknown(t *testing.T) {
	p := newFakeProc(t)
	p.process(2001, "/usr/bin/foot")
	p.fd(2001, 12, "/dev/ptmx", ptmxInfo(4))

	pane, err := newFootAt(p.root).Locate(context.Background(), "/dev/pts/99")
	if err != nil || pane != nil {
		t.Fatalf("Locate(unknown) = (%+v, %v), want (nil, nil)", pane, err)
	}
}

func TestFootShouldReportAnEmptyCompleteSetWhenProcIsUnreadable(t *testing.T) {
	l := newFootAt(filepath.Join(t.TempDir(), "missing"))
	panes, err := l.Snapshot(context.Background())
	if err != nil || panes == nil || len(panes) != 0 {
		t.Fatalf("Snapshot = (%+v, %v), want a non-nil empty map", panes, err)
	}
	if l.Available() {
		t.Fatal("Available() = true over an unreadable /proc, want false")
	}
}

func TestFootShouldBecomeAvailableWhenAFootProcessAppears(t *testing.T) {
	p := newFakeProc(t)
	l := newFootAt(p.root)
	if l.Available() {
		t.Fatal("precondition: no foot process yet")
	}
	p.process(2001, "/usr/bin/foot")
	if !l.Available() {
		t.Fatal("Available() = false after foot started, want true")
	}
}

// Focus is the WM seam's job; the terminal step must say it has nothing finer
// to do rather than claim success, so a session with no resolved window is
// reported as unfocusable instead of silently "focused".
func TestFootActivateShouldReportUnsupportedWhenAskedToSelectAPane(t *testing.T) {
	err := NewFoot().Activate(context.Background(), &PaneRef{Backend: "foot", Mux: 2001, TTY: "/dev/pts/4"})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Activate err = %v, want ErrUnsupported", err)
	}
}

// Nesting: a claude inside tmux inside foot has the tmux pane's tty, whose
// master tmux holds. The chain must hand that tty to tmux and the foot-hosted
// tty to foot, never the reverse.
func TestFootShouldNotShadowTmuxWhenChainedInnermostFirst(t *testing.T) {
	p := newFakeProc(t)
	p.process(2001, "/usr/bin/foot")
	p.fd(2001, 12, "/dev/ptmx", ptmxInfo(4))
	tmux := &fakeLocator{name: "tmux", available: true, wantTTY: "/dev/pts/8",
		pane: &PaneRef{Backend: "tmux", Handle: "%1", TTY: "/dev/pts/8"}}

	c := NewChain(tmux, newFootAt(p.root))
	inner, _ := c.Locate(context.Background(), "/dev/pts/8")
	outer, _ := c.Locate(context.Background(), "/dev/pts/4")
	if inner == nil || inner.Backend != "tmux" {
		t.Errorf("tmux tty resolved to %+v, want the tmux pane", inner)
	}
	if outer == nil || outer.Backend != "foot" {
		t.Errorf("foot tty resolved to %+v, want the foot window", outer)
	}
}

// The exe link names the binary actually running: a package upgrade marks the
// old one "(deleted)", and a Nix-style install lives outside /usr/bin. Both are
// still foot driving live ptys; a foot-named directory is not.
func TestFootShouldRecognizeTheFootBinaryWhenUpgradedOrInstalledElsewhere(t *testing.T) {
	p := newFakeProc(t)
	p.process(2001, "/usr/bin/foot (deleted)")
	p.fd(2001, 12, "/dev/ptmx", ptmxInfo(4))
	p.process(2002, "/nix/store/abc123-foot-1.27.0/bin/foot")
	p.fd(2002, 12, "/dev/ptmx", ptmxInfo(5))
	p.process(2003, "/opt/foot/bin/other")
	p.fd(2003, 12, "/dev/ptmx", ptmxInfo(6))

	panes, _ := newFootAt(p.root).Snapshot(context.Background())
	if len(panes) != 2 || panes["/dev/pts/4"].Mux != 2001 || panes["/dev/pts/5"].Mux != 2002 {
		t.Fatalf("Snapshot = %+v, want pts/4→2001 and pts/5→2002 only", panes)
	}
}
