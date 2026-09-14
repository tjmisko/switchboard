package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/mapping"
	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/sessionview"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/terminal"
	"github.com/tjmisko/switchboard/internal/waybarchip"
	"github.com/tjmisko/switchboard/internal/wm"
)

// One GUI window with two identically titled tabs; the second has two splits.
// The active CLI pane in each tab is insufficient to describe GUI focus. Only
// the title marker changes when the visible tab or split changes.
type tabFocusTerminal struct {
	active int
	fail   bool
}

func (*tabFocusTerminal) Name() string    { return "wezterm" }
func (*tabFocusTerminal) Available() bool { return true }
func (l *tabFocusTerminal) Snapshot(context.Context) (map[string]terminal.PaneRef, error) {
	panes := map[string]terminal.PaneRef{}
	for id := range 4 {
		tty := fmt.Sprintf("/dev/pts/%d", id)
		panes[tty] = terminal.PaneRef{Backend: "wezterm", Mux: 100,
			WindowID: 0, TabID: (id + 1) / 2, PaneID: id, TTY: tty, WindowTitle: "same"}
	}
	return panes, nil
}
func (l *tabFocusTerminal) Locate(ctx context.Context, tty string) (*terminal.PaneRef, error) {
	panes, _ := l.Snapshot(ctx)
	pane, ok := panes[tty]
	if !ok {
		return nil, nil
	}
	return &pane, nil
}
func (l *tabFocusTerminal) Activate(_ context.Context, pane *terminal.PaneRef) error {
	if l.fail {
		return errors.New("activation failed")
	}
	l.active = pane.PaneID
	return nil
}

type tabFocusManager struct {
	stubManager
	term   *tabFocusTerminal
	active string
}

func (m *tabFocusManager) Clients(context.Context) ([]wm.Window, error) {
	return []wm.Window{{Address: "0xshared", PID: 100, WorkspaceID: 1,
		Title: fmt.Sprintf("same [sbp:%d] [sbw:100:0]", m.term.active)}}, nil
}
func (m *tabFocusManager) ActiveWindow(context.Context) (string, error) { return m.active, nil }
func (m *tabFocusManager) Focus(_ context.Context, addr string) error {
	m.active = addr
	return nil
}

func TestWaybarAndCycleFollowTheActiveWeztermTabAndSplit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := state.New("")
	started := time.Now()
	store.Apply(func(sessions map[int]*state.Session) {
		for id := range 3 {
			pid := 1000 + id
			sessions[pid] = &state.Session{PID: pid, TTY: fmt.Sprintf("/dev/pts/%d", id),
				StartedAt: started.Add(time.Duration(id) * time.Second), CWD: "/same",
				Agent: state.AgentKindClaude, Claude: &state.AgentInfo{SessionID: fmt.Sprint(pid)}}
		}
	})
	term := &tabFocusTerminal{}
	manager := &tabFocusManager{term: term, active: "0xshared"}
	resolver := mapping.NewResolver(term, manager)
	turn := &resolveTurn{}
	server := rpc.New(store, "", term, manager)
	server.SetFocusObserver(func(ctx context.Context) { reresolveAll(ctx, store, resolver, turn, nil) })
	renderer := waybarchip.NewRenderer(2000)
	assertFocus := func(want int) {
		t.Helper()
		snap := store.Snapshot()
		chips := renderer.RenderSlotsAt(snap, 3, time.Now())
		for i, sess := range snap.Sessions {
			expect := sess.PID == want
			if sess.Focused != expect || slices.Contains(chips[i].Class, "focused") != expect {
				t.Fatalf("pid %d: focused=%v classes=%v; want only pid %d", sess.PID, sess.Focused, chips[i].Class, want)
			}
		}
	}
	reresolveAll(t.Context(), store, resolver, turn, nil)
	assertFocus(1000) // pane, tab and window zero are all valid
	for _, step := range []struct {
		dir  string
		want int
	}{{"next", 1001}, {"next", 1002}, {"next", 1000}, {"prev", 1002}, {"prev", 1001}, {"prev", 1000}} {
		target, ok := sessionview.Cycle(store.Snapshot().Sessions, step.dir)
		if !ok || target.PID != step.want {
			t.Fatalf("cycle %s = pid %d, want %d", step.dir, target.PID, step.want)
		}
		if err := server.FocusLocalSession(t.Context(), target.PID, target.StartedAt); err != nil {
			t.Fatal(err)
		}
		// No WM focus event or periodic tick between consecutive keypresses.
		assertFocus(step.want)
	}
	term.fail = true
	target := store.Snapshot().Sessions[1]
	if err := server.FocusLocalSession(t.Context(), target.PID, target.StartedAt); err == nil {
		t.Fatal("failed activation acknowledged")
	}
	assertFocus(1000)
	term.fail = false

	// Native tab/split selection sends a title event, not a window focus event.
	for _, id := range []int{2, 1, 3, 0} {
		term.active = id
		handleWMEvent(t.Context(), store, resolver, wm.Event{Kind: wm.EventLayoutChanged}, nil, nil, nil, turn)
		want := 1000 + id
		if id == 3 {
			want = 0 // ordinary shell tab: none of the agent chips is selected
		}
		assertFocus(want)
	}
	for _, addr := range []string{"0xother", "0xshared", ""} {
		manager.active = addr
		handleWMEvent(t.Context(), store, resolver, wm.Event{Kind: wm.EventFocusChanged, Address: addr}, nil, nil, nil, turn)
		want := 0
		if addr == "0xshared" {
			want = 1000
		}
		assertFocus(want)
	}
}

func TestLegacySharedWindowDoesNotInventFocusHistory(t *testing.T) {
	store, tick := sharedWindowFixture(t)
	tick(nil)
	histDir := t.TempDir()
	sink, flush := newRecordingSink(t, histDir)
	before := observableSnapshot(t, store)
	for range 40 {
		tick(sink)
	}
	flush()
	if got := eventsOfType(readEvents(t, histDir), history.EventFocus); len(got) != 0 {
		t.Fatalf("unchanged shared window produced focus events: %+v", got)
	}
	if after := observableSnapshot(t, store); before != after {
		t.Fatal("unchanged shared window changed the snapshot")
	}
}
