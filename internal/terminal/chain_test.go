package terminal

import (
	"context"
	"errors"
	"testing"
)

// fakeLocator is a programmable Locator for chain tests.
type fakeLocator struct {
	name      string
	available bool
	pane      *PaneRef // returned by Locate when tty == wantTTY
	wantTTY   string
	locateErr error
	activated *PaneRef // records the ref passed to Activate
}

func (f *fakeLocator) Name() string    { return f.name }
func (f *fakeLocator) Available() bool { return f.available }
func (f *fakeLocator) Locate(_ context.Context, tty string) (*PaneRef, error) {
	if f.locateErr != nil {
		return nil, f.locateErr
	}
	if f.pane != nil && tty == f.wantTTY {
		return f.pane, nil
	}
	return nil, nil
}
func (f *fakeLocator) Activate(_ context.Context, ref *PaneRef) error {
	f.activated = ref
	return nil
}

// batchFake adds the optional batch fast-path to a fakeLocator. It is a separate
// type so a bare *fakeLocator still models a backend WITHOUT one — the case the
// chain must refuse outright rather than silently under-report.
type batchFake struct {
	*fakeLocator
	panes   map[string]PaneRef
	snapErr error
}

func (f batchFake) Snapshot(context.Context) (map[string]PaneRef, error) {
	if f.snapErr != nil {
		return nil, f.snapErr
	}
	return f.panes, nil
}

// Locate answers from the same pane set Snapshot returns, overriding the
// embedded fake's single-tty form. A backend whose two paths agree is the
// precondition for asserting that the chain's merge preserves that agreement.
func (f batchFake) Locate(_ context.Context, tty string) (*PaneRef, error) {
	if f.locateErr != nil {
		return nil, f.locateErr
	}
	ref, ok := f.panes[tty]
	if !ok {
		return nil, nil
	}
	return &ref, nil
}

func newBatchFake(name string, panes map[string]PaneRef) batchFake {
	return batchFake{fakeLocator: &fakeLocator{name: name, available: true}, panes: panes}
}

// chainSnapshot reaches the chain's batch path through the same type assertion
// the daemon makes: NewChain hands back the neutral Locator, and the fast-path
// is only ever discovered by asking.
func chainSnapshot(t *testing.T, l Locator) (map[string]PaneRef, error) {
	t.Helper()
	s, ok := l.(Snapshotter)
	if !ok {
		t.Fatal("chain does not implement Snapshotter")
	}
	return s.Snapshot(context.Background())
}

func TestChainLocateFirstMatchWins(t *testing.T) {
	inner := &fakeLocator{name: "tmux", available: true, wantTTY: "/dev/pts/5", pane: &PaneRef{Backend: "tmux", TTY: "/dev/pts/5"}}
	outer := &fakeLocator{name: "wezterm", available: true, wantTTY: "/dev/pts/5", pane: &PaneRef{Backend: "wezterm", TTY: "/dev/pts/5"}}
	c := NewChain(inner, outer)

	pane, err := c.Locate(context.Background(), "/dev/pts/5")
	if err != nil || pane == nil {
		t.Fatalf("Locate = (%v, %v), want a pane", pane, err)
	}
	if pane.Backend != "tmux" {
		t.Errorf("Locate matched %q, want the innermost (tmux) first", pane.Backend)
	}
}

// A locator that errors must not blank a later locator that owns the tty.
func TestChainErrorDoesNotBlankHealthy(t *testing.T) {
	broken := &fakeLocator{name: "tmux", available: true, locateErr: errors.New("boom")}
	healthy := &fakeLocator{name: "wezterm", available: true, wantTTY: "/dev/pts/9", pane: &PaneRef{Backend: "wezterm", TTY: "/dev/pts/9"}}
	c := NewChain(broken, healthy)

	pane, err := c.Locate(context.Background(), "/dev/pts/9")
	if err != nil || pane == nil || pane.Backend != "wezterm" {
		t.Fatalf("Locate = (%v, %v), want the healthy wezterm pane", pane, err)
	}
}

// An unknown tty resolves to no pane and surfaces no error when no locator errs.
func TestChainUnknownTTYNoError(t *testing.T) {
	a := &fakeLocator{name: "tmux", available: true, wantTTY: "/dev/pts/5"}
	b := &fakeLocator{name: "wezterm", available: true, wantTTY: "/dev/pts/5"}
	pane, err := NewChain(a, b).Locate(context.Background(), "/dev/pts/999")
	if pane != nil || err != nil {
		t.Errorf("Locate(unknown) = (%v, %v), want (nil, nil)", pane, err)
	}
}

func TestChainActivateRoutesByBackend(t *testing.T) {
	tmux := &fakeLocator{name: "tmux", available: true}
	wez := &fakeLocator{name: "wezterm", available: true}
	c := NewChain(tmux, wez)

	ref := &PaneRef{Backend: "wezterm", Handle: "x"}
	if err := c.Activate(context.Background(), ref); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if wez.activated != ref {
		t.Error("Activate did not route to the wezterm backend")
	}
	if tmux.activated != nil {
		t.Error("Activate wrongly called the tmux backend")
	}

	if err := c.Activate(context.Background(), &PaneRef{Backend: "kitty"}); err == nil {
		t.Error("Activate with an unknown backend = nil err, want error")
	}
}

// The merge must reproduce Locate's first-match-wins, or a claude nested in tmux
// would batch-resolve to the wezterm pane hosting the tmux client and Navigate
// would focus the outer window instead of the pane.
func TestChainSnapshotShouldPreferTheInnerLocatorWhenBothOwnATTY(t *testing.T) {
	const shared = "/dev/pts/5"
	inner := newBatchFake("tmux", map[string]PaneRef{
		shared:       {Backend: "tmux", TTY: shared},
		"/dev/pts/6": {Backend: "tmux", TTY: "/dev/pts/6"},
	})
	outer := newBatchFake("wezterm", map[string]PaneRef{
		shared:        {Backend: "wezterm", TTY: shared},
		"/dev/pts/11": {Backend: "wezterm", TTY: "/dev/pts/11"},
	})
	c := NewChain(inner, outer)

	panes, err := chainSnapshot(t, c)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := panes[shared].Backend; got != "tmux" {
		t.Errorf("snapshot[%q].Backend = %q, want the innermost (tmux)", shared, got)
	}
	// Disjoint ttys from both locators still merge — precedence is per-tty, not
	// a whole-locator preference.
	if len(panes) != 3 {
		t.Errorf("merged %d panes, want 3 (both locators' disjoint ttys): %+v", len(panes), panes)
	}

	// The batch answer is the one Locate would have given, tty by tty.
	for tty, want := range panes {
		got, err := c.Locate(context.Background(), tty)
		if err != nil || got == nil {
			t.Fatalf("Locate(%q) = (%v, %v), want a pane", tty, got, err)
		}
		if *got != want {
			t.Errorf("batch and single paths disagree for %q: Snapshot = %+v, Locate = %+v", tty, want, *got)
		}
	}
}

// Error discipline, mirroring Locate's: one failing locator must not blank the
// healthy ones.
func TestChainSnapshotShouldKeepHealthyPanesWhenALocatorErrors(t *testing.T) {
	broken := newBatchFake("tmux", nil)
	broken.snapErr = errors.New("boom")
	healthy := newBatchFake("wezterm", map[string]PaneRef{
		"/dev/pts/9": {Backend: "wezterm", TTY: "/dev/pts/9"},
	})

	panes, err := chainSnapshot(t, NewChain(broken, healthy))
	if err != nil {
		t.Fatalf("Snapshot err = %v, want nil while a healthy locator has panes", err)
	}
	if got := panes["/dev/pts/9"].Backend; got != "wezterm" {
		t.Errorf("snapshot[/dev/pts/9].Backend = %q, want wezterm", got)
	}
}

func TestChainSnapshotShouldSurfaceTheErrorWhenEveryLocatorFailsAndNothingIsFound(t *testing.T) {
	first := newBatchFake("tmux", nil)
	first.snapErr = errors.New("first")
	second := newBatchFake("wezterm", nil)
	second.snapErr = errors.New("second")

	panes, err := chainSnapshot(t, NewChain(first, second))
	if err == nil {
		t.Fatalf("Snapshot = (%+v, nil), want the first error when nothing was found", panes)
	}
	if err.Error() != "first" {
		t.Errorf("Snapshot err = %v, want the FIRST error", err)
	}
}

// A locator without the batch path cannot contribute to a merge, and a merged
// map missing its panes is indistinguishable from "those ttys own no pane" — so
// the chain refuses the batch wholesale, as ErrNoBatchPath, and the caller
// degrades to per-session Locate, which still routes through every child. The
// sentinel matters: an undistinguished error here would be read as a transient
// failure and resolve nothing at all.
func TestChainSnapshotShouldRefuseTheBatchPathWhenAMemberHasNoSnapshotter(t *testing.T) {
	singlePath := &fakeLocator{name: "kitty", available: true, wantTTY: "/dev/pts/3", pane: &PaneRef{Backend: "kitty", TTY: "/dev/pts/3"}}
	batch := newBatchFake("wezterm", map[string]PaneRef{
		"/dev/pts/9": {Backend: "wezterm", TTY: "/dev/pts/9"},
	})
	c := NewChain(singlePath, batch)

	_, err := chainSnapshot(t, c)
	if err == nil {
		t.Fatal("Snapshot = nil err, want a refusal when a member has no batch path")
	}
	if !errors.Is(err, ErrNoBatchPath) {
		t.Errorf("Snapshot err = %v, want ErrNoBatchPath so the caller falls back to Locate", err)
	}
	if got, err := Snapshot(context.Background(), c); got != nil || !errors.Is(err, ErrNoBatchPath) {
		t.Errorf("Snapshot = (%+v, %v), want (nil, ErrNoBatchPath)", got, err)
	}
	// The fallback still resolves the single-path locator's tty.
	pane, err := c.Locate(context.Background(), "/dev/pts/3")
	if err != nil || pane == nil || pane.Backend != "kitty" {
		t.Errorf("Locate fallback = (%v, %v), want the kitty pane", pane, err)
	}
}

func TestChainSnapshotShouldReportAnEmptySetWhenNoLocatorOwnsATTY(t *testing.T) {
	c := NewChain(newBatchFake("tmux", nil), newBatchFake("wezterm", nil))

	panes, err := chainSnapshot(t, c)
	if err != nil {
		t.Fatalf("Snapshot err = %v, want nil (owning nothing is success)", err)
	}
	if panes == nil {
		t.Fatal("Snapshot returned a nil map; want non-nil so a missing key means 'no pane owns this tty'")
	}
	if len(panes) != 0 {
		t.Errorf("Snapshot = %+v, want empty", panes)
	}
}

func TestChainName(t *testing.T) {
	c := NewChain(&fakeLocator{name: "tmux"}, &fakeLocator{name: "wezterm"})
	if got := c.Name(); got != "tmux+wezterm" {
		t.Errorf("Name = %q, want tmux+wezterm", got)
	}
}
