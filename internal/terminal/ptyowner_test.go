package terminal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAlacrittyShouldLocateTheAlacrittyProcessHoldingThePtyMasterWhenStandalone(t *testing.T) {
	p := newFakeProc(t)
	p.process(5001, "/usr/bin/alacritty")
	p.fd(5001, 0, "/dev/null", "")
	p.fd(5001, 9, "/dev/ptmx", ptmxInfo(2))

	pane, err := newAlacrittyAt(p.root).Locate(context.Background(), "/dev/pts/2")
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	want := PaneRef{Backend: "alacritty", Mux: 5001, PaneID: 2, TTY: "/dev/pts/2"}
	if pane == nil || *pane != want {
		t.Fatalf("Locate = %+v, want %+v", pane, want)
	}
}

// `alacritty msg create-window` puts several windows in one process; each tty
// still resolves to that process so the mapping layer can see the shared pid
// and fail closed, exactly as for a foot server.
func TestAlacrittyShouldResolveEveryDaemonWindowTTYToTheSharedProcess(t *testing.T) {
	p := newFakeProc(t)
	p.process(5000, "/usr/bin/alacritty")
	p.fd(5000, 9, "/dev/ptmx", ptmxInfo(2))
	p.fd(5000, 13, "/dev/ptmx", ptmxInfo(7))

	panes, _ := newAlacrittyAt(p.root).Snapshot(context.Background())
	if len(panes) != 2 || panes["/dev/pts/2"].Mux != 5000 || panes["/dev/pts/7"].Mux != 5000 {
		t.Fatalf("Snapshot = %+v, want pts/2 and pts/7 on pid 5000", panes)
	}
}

func TestAlacrittyActivateShouldReportUnsupportedWhenAskedToSelectAPane(t *testing.T) {
	err := NewAlacritty().Activate(context.Background(), &PaneRef{Backend: "alacritty", Mux: 5001})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Activate err = %v, want ErrUnsupported", err)
	}
}

// One walk serves both backends, and each still claims only its own binary's
// ptys: a tty is owned by exactly one terminal, so the chain can never see two
// outer backends answer for it.
func TestPtyOwnersShouldClaimOnlyTheirOwnBinarysPtysWhenSharingAScan(t *testing.T) {
	p := newFakeProc(t)
	p.process(2001, "/usr/bin/foot")
	p.fd(2001, 12, "/dev/ptmx", ptmxInfo(4))
	p.process(5001, "/usr/bin/alacritty")
	p.fd(5001, 9, "/dev/ptmx", ptmxInfo(2))
	scan := newProcessScan(p.root, 0)
	foot, alacritty := newFoot(scan), newAlacritty(scan)

	footPanes, _ := foot.Snapshot(context.Background())
	alacrittyPanes, _ := alacritty.Snapshot(context.Background())
	if len(footPanes) != 1 || footPanes["/dev/pts/4"].Backend != "foot" {
		t.Errorf("foot panes = %+v, want only pts/4", footPanes)
	}
	if len(alacrittyPanes) != 1 || alacrittyPanes["/dev/pts/2"].Backend != "alacritty" {
		t.Errorf("alacritty panes = %+v, want only pts/2", alacrittyPanes)
	}

	c := NewChain(foot, alacritty)
	for tty, want := range map[string]string{"/dev/pts/4": "foot", "/dev/pts/2": "alacritty"} {
		if got, _ := c.Locate(context.Background(), tty); got == nil || got.Backend != want {
			t.Errorf("chain Locate(%s) = %+v, want backend %s", tty, got, want)
		}
	}
}

type steppedClock struct{ now time.Time }

func (c *steppedClock) read() time.Time { return c.now }

// Within the TTL one walk answers every question; a terminal opened after it is
// seen once the TTL lapses, and a clock stepped backwards forces a fresh walk
// rather than trusting an answer from the "future".
func TestProcessScanShouldReuseOneWalkWithinTheTTLAndRewalkAfterIt(t *testing.T) {
	p := newFakeProc(t)
	p.process(2001, "/usr/bin/foot")
	clock := &steppedClock{now: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	scan := newProcessScan(p.root, 250*time.Millisecond)
	scan.now = clock.read

	if got := scan.pidsRunning("foot"); len(got) != 1 {
		t.Fatalf("first walk = %v, want [2001]", got)
	}
	p.process(2002, "/usr/bin/foot")
	clock.now = clock.now.Add(100 * time.Millisecond)
	if got := scan.pidsRunning("foot"); len(got) != 1 {
		t.Fatalf("within TTL = %v, want the cached [2001]", got)
	}
	clock.now = clock.now.Add(150 * time.Millisecond)
	if got := scan.pidsRunning("foot"); len(got) != 2 {
		t.Fatalf("after TTL = %v, want both foot pids", got)
	}
	p.process(2003, "/usr/bin/foot")
	clock.now = clock.now.Add(-time.Second)
	if got := scan.pidsRunning("foot"); len(got) != 3 {
		t.Fatalf("after clock step back = %v, want a fresh walk with three pids", got)
	}
}

// A cached pid may be reused by another program before the TTL lapses. The
// per-use exe check must drop it, or that program's ptys — a tmux server's, say
// — would be reported as terminal windows.
func TestPtyOwnerShouldIgnoreACachedPidWhenItNowRunsAnotherBinary(t *testing.T) {
	p := newFakeProc(t)
	p.process(2001, "/usr/bin/foot")
	p.fd(2001, 12, "/dev/ptmx", ptmxInfo(4))
	clock := &steppedClock{now: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	scan := newProcessScan(p.root, time.Hour)
	scan.now = clock.read
	foot := newFoot(scan)
	if panes, _ := foot.Snapshot(context.Background()); len(panes) != 1 {
		t.Fatalf("precondition: Snapshot = %+v, want pts/4", panes)
	}

	exe := filepath.Join(p.root, "2001", "exe")
	if err := os.Remove(exe); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/bin/tmux", exe); err != nil {
		t.Fatal(err)
	}
	if panes, _ := foot.Snapshot(context.Background()); len(panes) != 0 {
		t.Fatalf("Snapshot after pid reuse = %+v, want nothing", panes)
	}
}
