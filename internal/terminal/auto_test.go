package terminal

import (
	"context"
	"errors"
	"testing"
)

// autoFake is a Locator whose availability we flip at runtime, so we can model
// a terminal that appears after the daemon (the autostart boot race). Named
// distinctly from chain_test.go's fakeLocator since they share the package.
type autoFake struct {
	name      string
	available bool
	pane      *PaneRef
}

func (f *autoFake) Name() string    { return f.name }
func (f *autoFake) Available() bool { return f.available }
func (f *autoFake) Locate(_ context.Context, _ string) (*PaneRef, error) {
	return f.pane, nil
}
func (f *autoFake) Activate(_ context.Context, _ *PaneRef) error { return nil }

func TestAutoShouldReportNoneWhenNoBackendIsLive(t *testing.T) {
	wez := &autoFake{name: "wezterm", available: false}
	a := auto{candidates: []Locator{wez}}

	if got := a.Name(); got != "none" {
		t.Fatalf("Name() = %q, want none when no backend is live", got)
	}
	if a.Available() {
		t.Fatal("Available() = true, want false when no backend is live")
	}
}

func TestAutoShouldLightUpWhenABackendAppears(t *testing.T) {
	wez := &autoFake{name: "wezterm", available: false}
	a := auto{candidates: []Locator{wez}}

	if a.Available() {
		t.Fatal("precondition: should start unavailable")
	}

	// The terminal comes up after the daemon — the boot-race recovery case.
	wez.available = true

	if got := a.Name(); got != "wezterm" {
		t.Fatalf("Name() = %q, want wezterm after the backend appears", got)
	}
	if !a.Available() {
		t.Fatal("Available() = false, want true after the backend appears")
	}
}

func TestAutoShouldDelegateLocateToTheLiveBackend(t *testing.T) {
	want := &PaneRef{Backend: "wezterm", WindowTitle: "merge-draft-pr-audit"}
	wez := &autoFake{name: "wezterm", available: true, pane: want}
	a := auto{candidates: []Locator{wez}}

	got, err := a.Locate(context.Background(), "/dev/pts/0")
	if err != nil {
		t.Fatalf("Locate() error = %v", err)
	}
	if got != want {
		t.Fatalf("Locate() = %+v, want %+v", got, want)
	}
}

func TestAutoSnapshotShouldDelegateToTheLiveBackend(t *testing.T) {
	want := map[string]PaneRef{"/dev/pts/2": {Backend: "wezterm", TTY: "/dev/pts/2"}}
	wez := newBatchFake("wezterm", want)
	a := auto{candidates: []Locator{wez}}

	got, err := a.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if len(got) != 1 || got["/dev/pts/2"].Backend != "wezterm" {
		t.Fatalf("Snapshot() = %+v, want the live backend's pane set", got)
	}
}

// No backend live → the none backend, which owns nothing. That is an empty set,
// not a failure: the caller stays on the batch path instead of forking per
// session to be told the same nothing.
func TestAutoSnapshotShouldReportAnEmptySetWhenNoBackendIsLive(t *testing.T) {
	a := auto{candidates: []Locator{&autoFake{name: "wezterm", available: false}}}

	got, err := a.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot() error = %v, want nil", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("Snapshot() = %+v, want a non-nil empty set", got)
	}
}

// A live backend with no batch path must produce ErrNoBatchPath, not an empty
// map and not an undistinguished error: an empty map would read as "no tty owns a
// pane" and blank every chip, while a plain error would read as a transient
// failure and skip the per-session fallback this case depends on.
func TestAutoSnapshotShouldReportNoBatchPathWhenTheLiveBackendHasNone(t *testing.T) {
	a := auto{candidates: []Locator{&fakeLocator{name: "kitty", available: true}}}

	_, err := a.Snapshot(context.Background())
	if !errors.Is(err, ErrNoBatchPath) {
		t.Fatalf("Snapshot() err = %v, want ErrNoBatchPath so the caller falls back to Locate", err)
	}
	if got, err := Snapshot(context.Background(), a); got != nil || !errors.Is(err, ErrNoBatchPath) {
		t.Errorf("Snapshot = (%+v, %v), want (nil, ErrNoBatchPath)", got, err)
	}
}

func TestAutoShouldStayLiveWhenSeveralBackendsCompose(t *testing.T) {
	tmux := &autoFake{name: "tmux", available: true}
	wez := &autoFake{name: "wezterm", available: true}
	a := auto{candidates: []Locator{tmux, wez}}

	// Both live: current() composes a chain rather than picking one, so the
	// per-tty nesting is preserved. We only assert Navigate stays on; the
	// chain's own name is its package's concern.
	if !a.Available() {
		t.Fatal("Available() = false, want true when multiple backends are live")
	}
}
