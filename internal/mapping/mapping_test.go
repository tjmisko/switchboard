package mapping

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/terminal"
	"github.com/tjmisko/switchboard/internal/wm"
)

// §3.2 matchUniqueClient — without a Switchboard window marker, the legacy
// terminal<->WM join requires BOTH the mux pid and the window title, and
// returns nil on zero or ambiguous matches.
func TestMatchUniqueClient(t *testing.T) {
	clients := []wm.Window{
		{Address: "0xA", PID: 10, Title: "A"},
		{Address: "0xB", PID: 10, Title: "B"},
		{Address: "0xC", PID: 20, Title: "A"},
	}

	if got := matchUniqueClient(clients, 10, 0, "A"); got == nil || got.Address != "0xA" {
		t.Errorf("unique match = %v, want 0xA", got)
	}
	if got := matchUniqueClient(clients, 99, 0, "A"); got != nil {
		t.Errorf("no pid match = %v, want nil", got)
	}
	// Both keys required: pid matches but title doesn't, and vice versa.
	if got := matchUniqueClient(clients, 10, 0, "Z"); got != nil {
		t.Errorf("pid-only match = %v, want nil", got)
	}
	if got := matchUniqueClient(clients, 30, 0, "A"); got != nil {
		t.Errorf("title-only match = %v, want nil", got)
	}

	// Ambiguous: two clients share pid+title → nil (retry next tick).
	ambiguous := []wm.Window{
		{Address: "0xA", PID: 10, Title: "A"},
		{Address: "0xB", PID: 10, Title: "A"},
	}
	if got := matchUniqueClient(ambiguous, 10, 0, "A"); got != nil {
		t.Errorf("ambiguous match = %v, want nil", got)
	}
}

// Foot exposes no title, so its pane arrives with only the owning process. A
// standalone foot window is its own process, which makes the pid alone an exact
// join; a foot server's windows share one pid, which must fail closed.
func TestMatchUniqueClientShouldJoinOnPidAloneWhenThePaneHasNoTitle(t *testing.T) {
	standalone := []wm.Window{
		{Address: "0xA", PID: 2001, Title: "✳ refactor-parser"},
		{Address: "0xB", PID: 2002, Title: "✳ refactor-parser"},
		{Address: "0xC", PID: 4242, Title: ""},
	}
	if got := matchUniqueClient(standalone, 2002, 0, ""); got == nil || got.Address != "0xB" {
		t.Errorf("standalone foot join = %v, want 0xB", got)
	}
	if got := matchUniqueClient(standalone, 9999, 0, ""); got != nil {
		t.Errorf("unknown foot pid = %v, want nil", got)
	}

	server := []wm.Window{
		{Address: "0xA", PID: 3000, Title: "one"},
		{Address: "0xB", PID: 3000, Title: "two"},
	}
	if got := matchUniqueClient(server, 3000, 0, ""); got != nil {
		t.Errorf("foot server join = %v, want nil (two windows share the pid)", got)
	}
	if got := matchUniqueClient(server[:1], 3000, 0, ""); got == nil || got.Address != "0xA" {
		t.Errorf("single-window foot server join = %v, want 0xA", got)
	}
}

// End to end through the batched reconcile: a fresh foot session gains its
// window, and a later ambiguity (a second footclient window opening on the same
// server) keeps the address it already had rather than blanking or guessing.
func TestReconcileFromShouldMapFootSessionsAndHoldThemThroughServerAmbiguity(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	pane := terminal.PaneRef{Backend: "foot", Mux: 3000, PaneID: 4, TTY: "/dev/pts/4"}
	panes := map[string]terminal.PaneRef{"/dev/pts/4": pane}
	sess := state.Session{PID: 100, TTY: "/dev/pts/4"}

	var r *Resolver
	r.ReconcileFrom(&sess, panes, []wm.Window{{Address: "0xA", PID: 3000, Workspace: "2", WorkspaceID: 2}}, now)
	if sess.Hyprland == nil || sess.Hyprland.Address != "0xA" || sess.Hyprland.WorkspaceID != 2 {
		t.Fatalf("Hyprland = %+v, want 0xA on workspace 2", sess.Hyprland)
	}
	if sess.Wezterm != nil {
		t.Fatalf("Wezterm = %+v, want nil for a foot pane", sess.Wezterm)
	}
	if sess.Hyprland.ActivePaneID != nil {
		t.Fatalf("ActivePaneID = %v, want nil: foot has no pane marker", *sess.Hyprland.ActivePaneID)
	}

	r.ReconcileFrom(&sess, panes, []wm.Window{
		{Address: "0xA", PID: 3000, Workspace: "2", WorkspaceID: 2},
		{Address: "0xB", PID: 3000, Workspace: "5", WorkspaceID: 5},
	}, now)
	if sess.Hyprland.Address != "0xA" {
		t.Fatalf("after ambiguity Address = %q, want the prior 0xA", sess.Hyprland.Address)
	}

	fresh := state.Session{PID: 101, TTY: "/dev/pts/4"}
	r.ReconcileFrom(&fresh, panes, []wm.Window{
		{Address: "0xA", PID: 3000},
		{Address: "0xB", PID: 3000},
	}, now)
	if fresh.Hyprland != nil {
		t.Fatalf("fresh session under ambiguity Hyprland = %+v, want nil (Observe-only)", fresh.Hyprland)
	}
}

// The WezTerm formatter changes only the compositor-visible title; `wezterm
// cli list` continues to report the base title. A fresh daemon must therefore
// consume the stable marker rather than requiring those two strings to be
// equal. This is the login-only regression which otherwise leaves every
// session's Hyprland block nil until an old mapping happens to exist.
func TestMatchUniqueClientUsesSwitchboardWindowMarker(t *testing.T) {
	clients := []wm.Window{
		{Address: "0xA", PID: 10, Title: "same title [sbw:10:3]"},
		{Address: "0xB", PID: 10, Title: "same title [sbw:10:4]"},
		{Address: "0xC", PID: 99, Title: "same title [sbw:10:4]"},
	}

	got := matchUniqueClient(clients, 10, 4, "same title")
	if got == nil || got.Address != "0xB" {
		t.Fatalf("marked match = %v, want 0xB", got)
	}

	// The exact marker, not mutable presentation text, owns the join.
	got = matchUniqueClient(clients, 10, 3, "a spinner or title sampled later")
	if got == nil || got.Address != "0xA" {
		t.Fatalf("marked match with changed base title = %v, want 0xA", got)
	}
}

func TestMatchUniqueClientFailsClosedOnDuplicateWindowMarker(t *testing.T) {
	clients := []wm.Window{
		{Address: "0xA", PID: 10, Title: "one [sbw:10:4]"},
		{Address: "0xB", PID: 10, Title: "two [sbw:10:4]"},
	}
	if got := matchUniqueClient(clients, 10, 4, "one"); got != nil {
		t.Fatalf("duplicate marked match = %v, want nil", got)
	}
}

func TestMatchUniqueClientNormalizesActivitySpinners(t *testing.T) {
	const spinners = "◐◑◒◓⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏✳⠂⠐⠁⠈⠠⠄⡀⢀"
	for _, terminalSpinner := range spinners {
		for _, wmSpinner := range spinners {
			clients := []wm.Window{{Address: "win", PID: 10, Title: string(wmSpinner) + " Build Polybar"}}
			got := matchUniqueClient(clients, 10, 0, string(terminalSpinner)+" Build Polybar")
			if got == nil || got.Address != "win" {
				t.Fatalf("terminal spinner %q, WM spinner %q did not match", terminalSpinner, wmSpinner)
			}
		}
	}

	clients := []wm.Window{{Address: "win", PID: 10, Title: "◐ Build Polybar"}}
	if got := matchUniqueClient(clients, 10, 0, "◑ Different task"); got != nil {
		t.Fatalf("normalization matched genuinely different titles: %+v", got)
	}
}

func TestMatchUniqueClientJoinsAnAlreadyNormalizedPaneTitleToASpinningWMTitle(t *testing.T) {
	// The terminal seam already normalizes the pane title. A malformed double
	// spinner on the independently sampled WM side still has to reach the same
	// fixed point or the legacy join fails.
	clients := []wm.Window{{Address: "win", PID: 10, Title: "⠋ ⠙ Build Polybar"}}
	got := matchUniqueClient(clients, 10, 0, "Build Polybar")
	if got == nil || got.Address != "win" {
		t.Fatalf("normalized pane title did not join spinning WM title: %+v", got)
	}
}

// mapLocator serves a pre-built pane table the same way a live locator serves
// the terminal: an unowned tty resolves to no pane, without error (the
// terminal.Locator contract, behavior-spec §13.3). It exists so the equivalence
// test can drive Reconcile and ReconcileFrom off literally the same data.
type mapLocator struct{ panes map[string]terminal.PaneRef }

func (mapLocator) Name() string    { return "map" }
func (mapLocator) Available() bool { return true }

func (l mapLocator) Locate(_ context.Context, tty string) (*terminal.PaneRef, error) {
	pane, ok := l.panes[tty]
	if !ok {
		return nil, nil
	}
	return &pane, nil
}

func (mapLocator) Activate(context.Context, *terminal.PaneRef) error {
	return terminal.ErrUnsupported
}

// sliceManager serves a fixed client list — the WM half of the same fixture.
type sliceManager struct{ clients []wm.Window }

func (sliceManager) Name() string    { return "slice" }
func (sliceManager) Available() bool { return true }

func (m sliceManager) Clients(context.Context) ([]wm.Window, error) { return m.clients, nil }
func (sliceManager) ActiveWindow(context.Context) (string, error)   { return "", nil }
func (sliceManager) Focus(context.Context, string) error            { return wm.ErrUnsupported }
func (sliceManager) Subscribe(context.Context) (<-chan wm.Event, error) {
	ch := make(chan wm.Event)
	close(ch)
	return ch, nil
}

// explodingLocator/explodingManager fail the test if touched. ReconcileFrom's
// whole reason to exist is that it does zero I/O per session, so the batched
// half of the equivalence test runs on a Resolver whose seams are landmines.
type explodingLocator struct{ t *testing.T }

func (explodingLocator) Name() string    { return "exploding" }
func (explodingLocator) Available() bool { return true }

func (l explodingLocator) Locate(context.Context, string) (*terminal.PaneRef, error) {
	l.t.Helper()
	l.t.Fatal("ReconcileFrom called the terminal locator; it must do no I/O")
	return nil, nil
}

func (l explodingLocator) Activate(context.Context, *terminal.PaneRef) error {
	l.t.Helper()
	l.t.Fatal("ReconcileFrom called Activate; it must do no I/O")
	return nil
}

type explodingManager struct{ t *testing.T }

func (explodingManager) Name() string    { return "exploding" }
func (explodingManager) Available() bool { return true }

func (m explodingManager) Clients(context.Context) ([]wm.Window, error) {
	m.t.Helper()
	m.t.Fatal("ReconcileFrom queried the WM; it must do no I/O")
	return nil, nil
}

func (m explodingManager) ActiveWindow(context.Context) (string, error) {
	m.t.Helper()
	m.t.Fatal("ReconcileFrom queried the WM; it must do no I/O")
	return "", nil
}

func (m explodingManager) Focus(context.Context, string) error {
	m.t.Helper()
	m.t.Fatal("ReconcileFrom called Focus; it must do no I/O")
	return nil
}

func (m explodingManager) Subscribe(context.Context) (<-chan wm.Event, error) {
	m.t.Helper()
	m.t.Fatal("ReconcileFrom subscribed to the WM; it must do no I/O")
	return nil, nil
}

// TestReconcileFromMatchesReconcile pins ReconcileFrom to Reconcile as an
// equivalence, not as a re-specification: each case runs BOTH against the same
// pane table and client list and demands the resulting Sessions be identical.
// The batched path is a pure performance change (one enumeration per tick
// instead of one per session), so any observable divergence — a mapping cleared
// on a miss, an ambiguous match resolved, a wezterm block stamped where the
// serial path leaves none — is a regression by definition.
func TestReconcileFromMatchesReconcile(t *testing.T) {
	// The tick clock ReconcileFrom stamps with. Deliberately far from wall clock
	// so a TitleAt sampled inside the call is impossible to confuse with it.
	tick := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	// A TitleAt seeded by an EARLIER tick, used by the cases that must leave the
	// wezterm block alone.
	stale := time.Date(2025, 12, 31, 23, 59, 0, 0, time.UTC)
	started := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	weztermPane := terminal.PaneRef{
		Backend:     "wezterm",
		Mux:         4242,
		MuxSocket:   "/run/wezterm.sock",
		PaneID:      7,
		TabID:       3,
		WindowID:    1,
		Title:       "· claude",
		WindowTitle: "switchboard",
		TTY:         "/dev/pts/5",
		CWD:         "/home/u/Projects/switchboard",
	}

	cases := []struct {
		name string
		// seed builds a FRESH session per run: Session.Hyprland is a pointer, so
		// the two halves must not share one.
		seed    func() state.Session
		panes   map[string]terminal.PaneRef
		clients []wm.Window
	}{
		{
			name: "should fill in wezterm and hyprland when the tty resolves cleanly",
			seed: func() state.Session {
				return state.Session{PID: 100, TTY: "/dev/pts/5", CWD: "/home/u", StartedAt: started}
			},
			panes: map[string]terminal.PaneRef{"/dev/pts/5": weztermPane},
			clients: []wm.Window{
				{Address: "0xAAA", PID: 4242, Title: "switchboard", Workspace: "3", WorkspaceID: 3},
				{Address: "0xBBB", PID: 9999, Title: "switchboard", Workspace: "1", WorkspaceID: 1},
			},
		},
		{
			name: "should fill a fresh hyprland mapping from the formatted window marker",
			seed: func() state.Session {
				return state.Session{PID: 107, TTY: "/dev/pts/5", CWD: "/home/u", StartedAt: started}
			},
			panes: map[string]terminal.PaneRef{"/dev/pts/5": weztermPane},
			clients: []wm.Window{
				{Address: "0xAAA", PID: 4242, Title: "switchboard [sbw:4242:1]", Workspace: "3", WorkspaceID: 3},
				{Address: "0xBBB", PID: 4242, Title: "switchboard [sbw:4242:2]", Workspace: "4", WorkspaceID: 4},
			},
		},
		{
			name: "should preserve active pane zero from the formatted window marker",
			seed: func() state.Session {
				return state.Session{PID: 108, TTY: "/dev/pts/5", StartedAt: started}
			},
			panes: map[string]terminal.PaneRef{"/dev/pts/5": weztermPane},
			clients: []wm.Window{
				{Address: "0xAAA", PID: 4242, Title: "same [sbp:0] [sbw:4242:1]", WorkspaceID: 3},
			},
		},
		{
			name: "should leave the session untouched when no pane owns the tty",
			seed: func() state.Session {
				return state.Session{PID: 101, TTY: "/dev/pts/9", CWD: "/home/u", StartedAt: started}
			},
			panes:   map[string]terminal.PaneRef{"/dev/pts/5": weztermPane},
			clients: []wm.Window{{Address: "0xAAA", PID: 4242, Title: "switchboard"}},
		},
		{
			name: "should leave hyprland unset when two windows share the pane's pid and title",
			seed: func() state.Session {
				return state.Session{PID: 102, TTY: "/dev/pts/5", StartedAt: started}
			},
			panes: map[string]terminal.PaneRef{"/dev/pts/5": weztermPane},
			// Ambiguous (pid, title): matchUniqueClient returns nil rather than
			// guessing, and the session waits for a later tick (decisions.md #4).
			clients: []wm.Window{
				{Address: "0xAAA", PID: 4242, Title: "switchboard", Workspace: "3", WorkspaceID: 3},
				{Address: "0xBBB", PID: 4242, Title: "switchboard", Workspace: "4", WorkspaceID: 4},
			},
		},
		{
			name: "should set no hyprland when the pane reports no mux pid",
			seed: func() state.Session {
				return state.Session{PID: 103, TTY: "/dev/pts/6", StartedAt: started}
			},
			// tmux: the pane pid is the in-pane process, so Mux stays 0 and there
			// is no key to join the WM on — Observe-only on the WM axis.
			panes: map[string]terminal.PaneRef{"/dev/pts/6": {
				Backend:     "tmux",
				Handle:      "%3",
				Mux:         0,
				WindowTitle: "switchboard",
				TTY:         "/dev/pts/6",
			}},
			clients: []wm.Window{{Address: "0xAAA", PID: 0, Title: "switchboard"}},
		},
		{
			name: "should keep the prior mapping when a resolved session's pane disappears",
			seed: func() state.Session {
				return state.Session{
					PID:       104,
					TTY:       "/dev/pts/5",
					StartedAt: started,
					Wezterm:   &state.WeztermInfo{MuxPID: 4242, PaneID: 7, WindowTitle: "switchboard", TitleAt: stale},
					Hyprland:  &state.HyprlandInfo{Address: "0xAAA", Workspace: "3", WorkspaceID: 3},
				}
			},
			// The terminal enumerated fine, it just no longer lists this tty. A
			// transient miss must not blank a live chip.
			panes:   map[string]terminal.PaneRef{},
			clients: []wm.Window{{Address: "0xCCC", PID: 4242, Title: "switchboard"}},
		},
		{
			name: "should return immediately when the session has no tty",
			seed: func() state.Session {
				return state.Session{PID: 105, TTY: "", StartedAt: started}
			},
			panes:   map[string]terminal.PaneRef{"": weztermPane},
			clients: []wm.Window{{Address: "0xAAA", PID: 4242, Title: "switchboard"}},
		},
		{
			name: "should keep the prior address when the WM lists no matching window",
			seed: func() state.Session {
				return state.Session{
					PID:       106,
					TTY:       "/dev/pts/5",
					StartedAt: started,
					Hyprland:  &state.HyprlandInfo{Address: "0xAAA", Workspace: "3", WorkspaceID: 3},
				}
			},
			panes:   map[string]terminal.PaneRef{"/dev/pts/5": weztermPane},
			clients: nil, // also stands in for a WM query that failed at fetch time
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			serial := NewResolver(mapLocator{panes: tc.panes}, sliceManager{clients: tc.clients})
			batched := NewResolver(explodingLocator{t: t}, explodingManager{t: t})

			want := tc.seed()
			before := time.Now()
			serial.Reconcile(context.Background(), &want)
			after := time.Now()

			got := tc.seed()
			batched.ReconcileFrom(&got, tc.panes, tc.clients, tick)

			reconcileTitleAt(t, &got, &want, before, after, tick)

			if !reflect.DeepEqual(got, want) {
				t.Errorf("ReconcileFrom diverged from Reconcile\n got: %+v\nwant: %+v", got, want)
			}
			if got.Hyprland != nil && want.Hyprland != nil && !reflect.DeepEqual(got.Hyprland, want.Hyprland) {
				t.Errorf("hyprland = %+v, want %+v", *got.Hyprland, *want.Hyprland)
			}
		})
	}
}

// reconcileTitleAt checks the one field the two paths CANNOT agree on by
// construction — Reconcile samples time.Now() inside the call, ReconcileFrom
// stamps the tick clock — then zeroes it on both so DeepEqual can compare
// everything else. The rule it enforces: if the serial path stamped a fresh
// TitleAt, the batched path must carry the tick clock; if it left the block
// alone, so must the batched path.
func reconcileTitleAt(t *testing.T, got, want *state.Session, before, after, tick time.Time) {
	t.Helper()
	if want.Wezterm == nil {
		if got.Wezterm != nil {
			t.Fatalf("ReconcileFrom set a wezterm block where Reconcile set none: %+v", got.Wezterm)
		}
		return
	}
	if got.Wezterm == nil {
		t.Fatalf("ReconcileFrom left wezterm nil where Reconcile set %+v", want.Wezterm)
	}

	stamped := !want.Wezterm.TitleAt.Before(before) && !want.Wezterm.TitleAt.After(after)
	if stamped {
		if !got.Wezterm.TitleAt.Equal(tick) {
			t.Errorf("TitleAt = %v, want the caller's tick clock %v", got.Wezterm.TitleAt, tick)
		}
	} else if !got.Wezterm.TitleAt.Equal(want.Wezterm.TitleAt) {
		t.Errorf("TitleAt = %v on an untouched wezterm block, want %v", got.Wezterm.TitleAt, want.Wezterm.TitleAt)
	}

	want.Wezterm.TitleAt = time.Time{}
	got.Wezterm.TitleAt = time.Time{}
}

// failingManager stands in for a WM query that fails at fetch time, which
// Enumerate reports as a nil client list.
type failingManager struct{ sliceManager }

func (failingManager) Clients(context.Context) ([]wm.Window, error) {
	return nil, errors.New("hyprctl unavailable")
}

// The window observer exists so a second consumer can read the enumeration a
// tick already fetched instead of forking its own. It must therefore see
// exactly what Enumerate returns — including the nil that means "the query
// failed", which the observer needs in order to tell that apart from an empty
// desktop.
func TestSetWindowObserverSeesEachTicksEnumerationIncludingFailures(t *testing.T) {
	clients := []wm.Window{{Address: "0xAAA", PID: 4242, Title: "switchboard [sbw:4242:1]", WorkspaceID: 3}}
	var observed [][]wm.Window
	resolver := NewResolver(mapLocator{panes: map[string]terminal.PaneRef{}}, sliceManager{clients: clients})
	resolver.SetWindowObserver(func(seen []wm.Window) { observed = append(observed, seen) })

	if _, got := resolver.Enumerate(context.Background()); !reflect.DeepEqual(got, clients) {
		t.Fatalf("Enumerate clients = %+v, want %+v", got, clients)
	}
	if len(observed) != 1 || !reflect.DeepEqual(observed[0], clients) {
		t.Fatalf("observed = %+v, want one call with %+v", observed, clients)
	}

	failing := NewResolver(mapLocator{panes: map[string]terminal.PaneRef{}}, failingManager{})
	failing.SetWindowObserver(func(seen []wm.Window) { observed = append(observed, seen) })
	failing.Enumerate(context.Background())
	if len(observed) != 2 || observed[1] != nil {
		t.Fatalf("observed after failed query = %+v, want a nil enumeration", observed)
	}
}

// An unobserved resolver is the ordinary case (no federation configured).
func TestEnumerateWithoutAWindowObserverIsUnchanged(t *testing.T) {
	clients := []wm.Window{{Address: "0xAAA", PID: 4242, Title: "switchboard"}}
	resolver := NewResolver(mapLocator{panes: map[string]terminal.PaneRef{}}, sliceManager{clients: clients})
	if _, got := resolver.Enumerate(context.Background()); !reflect.DeepEqual(got, clients) {
		t.Fatalf("Enumerate clients = %+v, want %+v", got, clients)
	}
}

func TestActivePaneMarkerIsScopedToTheExactWindow(t *testing.T) {
	for _, tc := range []struct {
		title string
		want  int
	}{
		{"same [sbp:0] [sbw:10:3]", 0},
		{"same [sbp:12] [sbw:10:3] ", 12},
		{"same [sbp:12] [sbw:10:4]", -1},
		{"same [sbp:12] [sbw:99:3]", -1},
		{"same [sbp:12] unrelated [sbw:10:3]", -1},
		{"same [sbp:-1] [sbw:10:3]", -1},
		{"same [sbp:+1] [sbw:10:3]", -1},
		{"same [sbp:] [sbw:10:3]", -1},
		{"same [sbp:99999999999999999999999999] [sbw:10:3]", -1},
		{"same [sbw:10:3]", -1},
	} {
		t.Run(tc.title, func(t *testing.T) {
			got := activePaneID(tc.title, 10, 3)
			if tc.want < 0 {
				if got != nil {
					t.Fatalf("unexpected pane %d", *got)
				}
				return
			}
			if got == nil || *got != tc.want {
				t.Fatalf("pane = %v, want %d", got, tc.want)
			}
		})
	}
}
