package main

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/herdr"
	"github.com/tjmisko/switchboard/internal/mapping"
	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/sessionview"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/terminal"
	"github.com/tjmisko/switchboard/internal/waybarchip"
	"github.com/tjmisko/switchboard/internal/wm"
)

// One OS window runs a herdr client showing four panes, each in its own herdr
// tab: three agents (w1:p1..p3) and a plain shell (w1:p4). The herdr
// counterpart of tabFocusTerminal: the window title says nothing about which
// pane is showing; only herdr's own stream does.
type herdrFocusTerminal struct {
	source *fakeHerdrSource
}

func (*herdrFocusTerminal) Name() string    { return "herdr" }
func (*herdrFocusTerminal) Available() bool { return true }
func (*herdrFocusTerminal) Snapshot(context.Context) (map[string]terminal.PaneRef, error) {
	panes := map[string]terminal.PaneRef{}
	for id := range 4 {
		tty := fmt.Sprintf("/dev/pts/%d", 10+id)
		// Mux and WindowTitle are the host terminal's, copied by the chain.
		panes[tty] = terminal.PaneRef{Backend: "herdr", Handle: fmt.Sprintf("w1:p%d", id+1),
			MuxSocket: testHerdrSock, Mux: 200, TTY: tty, WindowTitle: "herdr", HostTTY: "/dev/pts/1"}
	}
	return panes, nil
}
func (l *herdrFocusTerminal) Locate(ctx context.Context, tty string) (*terminal.PaneRef, error) {
	panes, _ := l.Snapshot(ctx)
	pane, ok := panes[tty]
	if !ok {
		return nil, nil
	}
	return &pane, nil
}

// Activate is herdr's pane.focus: herdr answers at once and announces the move
// on its stream afterwards.
func (l *herdrFocusTerminal) Activate(_ context.Context, pane *terminal.PaneRef) error {
	l.source.focus(pane.MuxSocket, pane.Handle)
	return nil
}

type herdrFocusManager struct {
	stubManager
	active string
}

func (*herdrFocusManager) Clients(context.Context) ([]wm.Window, error) {
	return []wm.Window{{Address: "0xherdr", PID: 200, WorkspaceID: 6, Title: "herdr"}}, nil
}
func (m *herdrFocusManager) ActiveWindow(context.Context) (string, error) { return m.active, nil }
func (m *herdrFocusManager) Focus(_ context.Context, addr string) error {
	m.active = addr
	return nil
}

func TestWaybarAndCycleShouldFollowHerdrsFocusedPaneWhenAgentsShareOneWindow(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := state.New("")
	started := time.Now()
	source := newFakeHerdrSource()
	for id := range 3 {
		source.set(herdr.PaneKey{Socket: testHerdrSock, PaneID: fmt.Sprintf("w1:p%d", id+1)}, "claude", herdr.StatusIdle, started)
	}
	source.set(herdr.PaneKey{Socket: testHerdrSock, PaneID: "w1:p4"}, "", herdr.StatusUnknown, started)
	source.active[testHerdrSock] = "w1:p1"

	term := &herdrFocusTerminal{source: source}
	manager := &herdrFocusManager{active: "0xherdr"}
	resolver := mapping.NewResolver(term, manager)
	turn := &resolveTurn{}
	store.Apply(func(sessions map[int]*state.Session) {
		for id := range 3 {
			pid := 2000 + id
			sess := &state.Session{PID: pid, TTY: fmt.Sprintf("/dev/pts/%d", 10+id),
				StartedAt: started.Add(time.Duration(id) * time.Second), CWD: "/same",
				Agent: state.AgentKindClaude, Claude: &state.AgentInfo{SessionID: fmt.Sprint(pid)}}
			pane, _ := term.Locate(t.Context(), sess.TTY)
			applyHerdrPane(sess, pane, source, nil, started)
			sessions[pid] = sess
		}
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go runHerdrStatus(ctx, store, source, nil, func(ctx context.Context) {
		refocus(ctx, store, resolver, turn, nil)
	})
	server := rpc.New(store, "", term, manager)
	server.SetFocusObserver(func(ctx context.Context) { reresolveAll(ctx, store, resolver, turn, nil) })
	renderer := waybarchip.NewRenderer(2000)

	focusIs := func(want int) (bool, string) {
		snap := store.Snapshot()
		chips := renderer.RenderSlotsAt(snap, 3, time.Now())
		for i, sess := range snap.Sessions {
			expect := sess.PID == want
			if sess.Focused != expect || slices.Contains(chips[i].Class, "focused") != expect {
				return false, fmt.Sprintf("pid %d: focused=%v classes=%v; want only pid %d", sess.PID, sess.Focused, chips[i].Class, want)
			}
		}
		return true, ""
	}
	// herdr announces a move on its stream after the fact, so the chip follows
	// within the event's latency rather than synchronously with the keypress.
	awaitFocus := func(want int) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for {
			ok, why := focusIs(want)
			if ok {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal(why)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}

	reresolveAll(t.Context(), store, resolver, turn, nil)
	awaitFocus(2000) // only the pane herdr shows, not all three in the window

	for _, step := range []struct {
		dir  string
		want int
	}{{"next", 2001}, {"next", 2002}, {"next", 2000}, {"prev", 2002}, {"prev", 2001}, {"prev", 2000}} {
		target, ok := sessionview.Cycle(store.Snapshot().Sessions, step.dir)
		if !ok || target.PID != step.want {
			t.Fatalf("cycle %s = pid %d, want %d", step.dir, target.PID, step.want)
		}
		if err := server.FocusLocalSession(t.Context(), target.PID, target.StartedAt); err != nil {
			t.Fatal(err)
		}
		awaitFocus(step.want)
	}

	// A tab switch inside herdr moves no OS window: herdr's stream alone
	// carries it, with no WM event and no reconcile tick.
	for _, pane := range []string{"w1:p3", "w1:p2", "w1:p4", "w1:p1"} {
		source.focus(testHerdrSock, pane)
		want := map[string]int{"w1:p1": 2000, "w1:p2": 2001, "w1:p3": 2002, "w1:p4": 0}[pane]
		awaitFocus(want) // w1:p4 is a plain shell: no agent chip is selected
	}

	// Another window taking focus unfocuses every herdr chip; coming back
	// restores the one herdr shows.
	for _, addr := range []string{"0xother", "", "0xherdr"} {
		manager.active = addr
		handleWMEvent(t.Context(), store, resolver, wm.Event{Kind: wm.EventFocusChanged, Address: addr}, nil, nil, nil, turn)
		want := 0
		if addr == "0xherdr" {
			want = 2000
		}
		awaitFocus(want)
	}
}

// With herdr's server unfollowed, which of its panes is showing is unknown, and
// focus falls back to the window, as for a WezTerm window with no title marker.
func TestApplyFocusShouldFallBackToTheWindowWhenHerdrHasNotSaidWhichPaneIsShowing(t *testing.T) {
	m := map[int]*state.Session{}
	for id := range 2 {
		m[3000+id] = &state.Session{PID: 3000 + id,
			Hyprland: &state.HyprlandInfo{Address: "0xherdr"},
			Herdr:    &state.HerdrInfo{PaneID: fmt.Sprintf("w1:p%d", id+1), Socket: testHerdrSock}}
	}
	applyFocus(m, "0xherdr", nil, time.Now())
	if !m[3000].Focused || !m[3001].Focused {
		t.Fatalf("focused = %v, %v; want both, as the window-level fallback", m[3000].Focused, m[3001].Focused)
	}
}

// A herdr client running inside a WezTerm tab is two nested layers; each one
// that says which pane it shows narrows focus independently.
func TestShowsInWindowShouldRequireEveryLayerThatHasSpokenToAgree(t *testing.T) {
	five, six := 5, 6
	for _, tc := range []struct {
		name      string
		sess      state.Session
		activeWin string
		want      bool
	}{
		{"another window is active", state.Session{Hyprland: &state.HyprlandInfo{Address: "0xa"}}, "0xb", false},
		{"no window is active", state.Session{Hyprland: &state.HyprlandInfo{Address: "0xa"}}, "", false},
		{"headless", state.Session{Headless: true, Hyprland: &state.HyprlandInfo{Address: "0xa"}}, "0xa", false},
		{"window only", state.Session{Hyprland: &state.HyprlandInfo{Address: "0xa"}}, "0xa", true},
		{"wezterm tab showing", state.Session{Wezterm: &state.WeztermInfo{PaneID: 5},
			Hyprland: &state.HyprlandInfo{Address: "0xa", ActivePaneID: &five}}, "0xa", true},
		{"wezterm tab hidden", state.Session{Wezterm: &state.WeztermInfo{PaneID: 5},
			Hyprland: &state.HyprlandInfo{Address: "0xa", ActivePaneID: &six}}, "0xa", false},
		{"herdr pane showing", state.Session{Hyprland: &state.HyprlandInfo{Address: "0xa"},
			Herdr: &state.HerdrInfo{PaneID: "w1:p1", ActivePaneID: "w1:p1"}}, "0xa", true},
		{"herdr pane hidden", state.Session{Hyprland: &state.HyprlandInfo{Address: "0xa"},
			Herdr: &state.HerdrInfo{PaneID: "w1:p1", ActivePaneID: "w1:p2"}}, "0xa", false},
		{"herdr silent", state.Session{Hyprland: &state.HyprlandInfo{Address: "0xa"},
			Herdr: &state.HerdrInfo{PaneID: "w1:p1"}}, "0xa", true},
		{"both showing", state.Session{Wezterm: &state.WeztermInfo{PaneID: 5},
			Hyprland: &state.HyprlandInfo{Address: "0xa", ActivePaneID: &five},
			Herdr:    &state.HerdrInfo{PaneID: "w1:p1", ActivePaneID: "w1:p1"}}, "0xa", true},
		{"wezterm tab hidden, herdr pane showing", state.Session{Wezterm: &state.WeztermInfo{PaneID: 5},
			Hyprland: &state.HyprlandInfo{Address: "0xa", ActivePaneID: &six},
			Herdr:    &state.HerdrInfo{PaneID: "w1:p1", ActivePaneID: "w1:p1"}}, "0xa", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := showsInWindow(&tc.sess, tc.activeWin); got != tc.want {
				t.Fatalf("showsInWindow = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestApplyHerdrPaneShouldRecordHerdrsFocusedPaneWhenItsServerIsFollowed(t *testing.T) {
	sess := graphClaudeSession()
	source := newFakeHerdrSource()
	source.set(herdrKey, "claude", herdr.StatusIdle, herdrT0)
	source.active[testHerdrSock] = "w1:p2"
	ref := herdrRef
	applyHerdrPane(sess, &ref, source, nil, herdrT0)
	if sess.Herdr.ActivePaneID != "w1:p2" {
		t.Fatalf("ActivePaneID = %q, want w1:p2", sess.Herdr.ActivePaneID)
	}

	// Only the focus moved: the early return must not swallow it.
	source.active[testHerdrSock] = "w1:p1"
	applyHerdrPane(sess, nil, source, nil, herdrT0.Add(time.Second))
	if sess.Herdr.ActivePaneID != "w1:p1" {
		t.Fatalf("ActivePaneID = %q after focus moved, want w1:p1", sess.Herdr.ActivePaneID)
	}

	// The server stops counting: herdr no longer says which pane is showing.
	source.drop(herdrKey)
	delete(source.active, testHerdrSock)
	applyHerdrPane(sess, nil, source, nil, herdrT0.Add(2*time.Second))
	if sess.Herdr.ActivePaneID != "" {
		t.Fatalf("ActivePaneID = %q with the server unfollowed, want empty", sess.Herdr.ActivePaneID)
	}
}

func TestRunHerdrStatusShouldNotRefocusWhenOnlyAStatusChanged(t *testing.T) {
	store := state.New("")
	store.Apply(func(m map[int]*state.Session) {
		sess := graphClaudeSession()
		sess.Herdr = &state.HerdrInfo{PaneID: "w1:p1", Socket: testHerdrSock}
		m[sess.PID] = sess
	})
	source := newFakeHerdrSource()
	refocused := make(chan struct{}, 4)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go runHerdrStatus(ctx, store, source, nil, func(context.Context) { refocused <- struct{}{} })

	source.set(herdrKey, "claude", herdr.StatusWorking, time.Now())
	source.changes <- herdrKey
	waitFor(t, "working", func() bool { return store.Snapshot().Sessions[0].Claude.Status == state.StatusWorking })
	select {
	case <-refocused:
		t.Fatal("refocused on a status change; want only on a focus move")
	default:
	}

	source.focus(testHerdrSock, "w1:p1")
	select {
	case <-refocused:
	case <-time.After(3 * time.Second):
		t.Fatal("no refocus after herdr's focus moved")
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
