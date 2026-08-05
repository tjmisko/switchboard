package conformance_test

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/tjmisko/switchboard/internal/conformance"
	"github.com/tjmisko/switchboard/internal/terminal"
	"github.com/tjmisko/switchboard/internal/wezterm"
)

// toPane maps a backend PaneRef into the neutral conformance.Pane. Each backend
// supplies its own because the neutral record's identity is numeric while tmux's
// is a string handle.
type toPane func(terminal.PaneRef) conformance.Pane

// locatorFixture wraps an internal/terminal backend behind the neutral
// conformance.Locator. SomeTTY is a live-fixture helper (not part of the
// production Locator), so each backend passes its own pane enumerator.
type locatorFixture struct {
	l       terminal.Locator
	pane    toPane
	someTTY func(ctx context.Context) (string, bool)
}

func (f locatorFixture) Available() bool { return f.l.Available() }

func (f locatorFixture) Locate(ctx context.Context, tty string) (*conformance.Pane, error) {
	ref, err := f.l.Locate(ctx, tty)
	if err != nil || ref == nil {
		return nil, err
	}
	p := f.pane(*ref)
	return &p, nil
}

func (f locatorFixture) SomeTTY(ctx context.Context) (string, bool) { return f.someTTY(ctx) }

// batchLocatorFixture adds the optional batch adapter. It is kept a separate
// type, applied only by newLocatorFixture when the backend really implements
// terminal.Snapshotter, so the suite's "skip when absent" path stays reachable
// for a backend that never grows one. It reuses the fixture's own single-path
// conversion: converting twice would make the drift assertion test the fixture
// rather than the backend.
type batchLocatorFixture struct{ locatorFixture }

func (f batchLocatorFixture) Snapshot(ctx context.Context) (map[string]conformance.Pane, error) {
	refs, err := f.l.(terminal.Snapshotter).Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	panes := make(map[string]conformance.Pane, len(refs))
	for tty, ref := range refs {
		panes[tty] = f.pane(ref)
	}
	return panes, nil
}

// newLocatorFixture presents the backend to the suite with exactly the surface
// it really has: the batch adapter appears only when the backend implements the
// fast path, mirroring how the daemon type-asserts for it.
func newLocatorFixture(f locatorFixture) conformance.Locator {
	if _, ok := f.l.(terminal.Snapshotter); ok {
		return batchLocatorFixture{f}
	}
	return f
}

// weztermPane carries the full (mux, pane) identity — the contract's stability
// anchor for this backend.
func weztermPane(r terminal.PaneRef) conformance.Pane {
	return conformance.Pane{Mux: r.Mux, PaneID: r.PaneID, TTY: r.TTY, WindowTitle: r.WindowTitle}
}

// tmuxPane maps tmux's stable id — the pane handle ("%3") — into the neutral
// Pane's PaneID. tmux pane ids are 0-based, and the contract treats
// (mux,pane)=(0,0) as "no identity", so offset by 1 to keep "%0" identifiable.
func tmuxPane(r terminal.PaneRef) conformance.Pane {
	id, _ := strconv.Atoi(strings.TrimPrefix(r.Handle, "%"))
	return conformance.Pane{PaneID: id + 1, TTY: r.TTY, WindowTitle: r.WindowTitle}
}

func weztermSomeTTY(ctx context.Context) (string, bool) {
	panes, err := wezterm.List(ctx)
	if err != nil {
		return "", false
	}
	for _, p := range panes {
		if p.TTYName != "" {
			return p.TTYName, true
		}
	}
	return "", false
}

func tmuxSomeTTY(context.Context) (string, bool) {
	out, err := exec.Command("tmux", "list-panes", "-a", "-F", "#{pane_tty}").Output()
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			return line, true
		}
	}
	return "", false
}

func TestWeztermLocatorConformance(t *testing.T) {
	conformance.RunLocatorContract(t, newLocatorFixture(locatorFixture{
		l: terminal.NewWezterm(), pane: weztermPane, someTTY: weztermSomeTTY,
	}))
}

func TestTmuxLocatorConformance(t *testing.T) {
	conformance.RunLocatorContract(t, newLocatorFixture(locatorFixture{
		l: terminal.NewTmux(), pane: tmuxPane, someTTY: tmuxSomeTTY,
	}))
}

// The auto backend is what the daemon actually runs, and it resolves its
// concrete backend per call — so the batch/single agreement has to hold through
// that indirection too, not just for the backends in isolation.
func TestAutoLocatorConformance(t *testing.T) {
	conformance.RunLocatorContract(t, newLocatorFixture(locatorFixture{
		l: terminal.NewAuto(), pane: weztermPane, someTTY: weztermSomeTTY,
	}))
}

// The none backend owns nothing, which the batch path must report as an empty
// set rather than a failure — the assertion that keeps an Observe-tier host on
// the fast path instead of forking per session to be told the same nothing.
func TestNoneLocatorConformance(t *testing.T) {
	conformance.RunLocatorContract(t, newLocatorFixture(locatorFixture{
		l:       terminal.NewNone(),
		pane:    weztermPane,
		someTTY: func(context.Context) (string, bool) { return "", false },
	}))
}

// ---------------------------------------------------------------------------
// Scripted fixtures — the batch contract's teeth on a host with no terminal
// ---------------------------------------------------------------------------

// scriptedLocator is a backend with a fixed pane set. The real backends'
// snapshots are empty on a CI host, which would make the batch/single agreement
// loop vacuous everywhere but a developer's live session; this drives it
// deterministically on every host, and guards the suite itself from silently
// becoming a no-op.
type scriptedLocator struct{ panes map[string]conformance.Pane }

func (s scriptedLocator) Available() bool { return true }

func (s scriptedLocator) Locate(_ context.Context, tty string) (*conformance.Pane, error) {
	p, ok := s.panes[tty]
	if !ok {
		return nil, nil
	}
	return &p, nil
}

func (s scriptedLocator) SomeTTY(context.Context) (string, bool) {
	for tty := range s.panes {
		return tty, true
	}
	return "", false
}

func (s scriptedLocator) Snapshot(context.Context) (map[string]conformance.Pane, error) {
	return s.panes, nil
}

func TestLocatorContractShouldPassWhenBatchAndSingleAgree(t *testing.T) {
	conformance.RunLocatorContract(t, scriptedLocator{panes: map[string]conformance.Pane{
		"/dev/pts/5": {Mux: 1234, PaneID: 7, TTY: "/dev/pts/5", WindowTitle: "switchboard"},
		"/dev/pts/9": {Mux: 1234, PaneID: 8, TTY: "/dev/pts/9", WindowTitle: "notes"},
	}})
}

// singlePathLocator is a backend with no batch fast-path at all — the shape
// every backend has on day one. The suite must SKIP its batch assertions rather
// than fail them; that is the whole point of the fast-path being optional.
type singlePathLocator struct{}

func (singlePathLocator) Available() bool { return false }

func (singlePathLocator) Locate(context.Context, string) (*conformance.Pane, error) {
	return nil, nil
}

func (singlePathLocator) SomeTTY(context.Context) (string, bool) { return "", false }

func TestLocatorContractShouldSkipBatchAssertionsWhenBackendHasNoSnapshotter(t *testing.T) {
	conformance.RunLocatorContract(t, singlePathLocator{})
}
