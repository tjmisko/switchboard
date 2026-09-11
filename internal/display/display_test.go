package display

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/sessionview"
	"github.com/tjmisko/switchboard/internal/state"
)

func TestModesPreserveCompleteOrderAndNavigation(t *testing.T) {
	snap := state.Snapshot{}
	started := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 32; i++ {
		snap.Sessions = append(snap.Sessions, state.Session{PID: 100 + i, Hostname: "local", Navigable: true, Focused: i == 31, StartedAt: started, Claude: &state.AgentInfo{Status: state.StatusWorking}})
	}
	for _, mode := range []Mode{Chips, Circles} {
		frame := Build(snap, mode, true, true, func(s state.Session) string { return fmt.Sprint(s.PID) })
		if len(frame.Sessions) != 32 {
			t.Fatalf("%s truncated to %d", mode, len(frame.Sessions))
		}
		for i, row := range frame.Sessions {
			if row.PID != 100+i || row.Index != i {
				t.Fatalf("%s reordered row %d: %+v", mode, i, row)
			}
		}
		next, ok := sessionview.Cycle(snap.Sessions, "next")
		if !ok || next.PID != 100 {
			t.Fatalf("%s next = %+v, %v", mode, next, ok)
		}
		if !strings.Contains(frame.Sessions[0].FocusSelector, ":started:") {
			t.Fatal("click target lacks generation fence")
		}
	}
}
func TestSurfaceChoiceRespectsMasterVisibility(t *testing.T) {
	for _, mode := range []Mode{Chips, Circles} {
		for _, visible := range []bool{true, false} {
			for _, count := range []int{0, 1, 17} {
				want := mode == Chips && visible && count > 0
				if got := WantsBottom(mode, visible, count); got != want {
					t.Fatalf("mode=%s visible=%v count=%d: %v", mode, visible, count, got)
				}
			}
		}
	}
}
func TestEmptyAndInformationalSessionsRemainWellDefined(t *testing.T) {
	snap := state.Snapshot{Sessions: []state.Session{{PID: 1, Headless: true}, {PID: 2, Hostname: "offline", Remote: true}}}
	frame := Build(snap, Circles, true, false, func(state.Session) string { return "session" })
	if len(frame.Sessions) != 2 || frame.Sessions[0].Navigable || frame.Sessions[1].Navigable {
		t.Fatalf("informational rows: %+v", frame)
	}
	if _, ok := sessionview.Cycle(snap.Sessions, "prev"); ok {
		t.Fatal("informational session became focusable")
	}
	empty := Build(state.Snapshot{}, Circles, true, true, func(state.Session) string { return "" })
	b, err := empty.JSON()
	if err != nil || !strings.Contains(string(b), `"sessions":[]`) {
		t.Fatalf("empty frame: %s %v", b, err)
	}
}
func TestWaybarAdapterEscapesUserText(t *testing.T) {
	frame := Frame{Version: 1, Mode: Circles, Sessions: []Session{{Key: "id", Tooltip: "a;b\n[display]\nmode=chips\\tail", Classes: []string{"working", "focused"}}}}
	text := string(frame.WaybarData())
	if strings.Count(text, "[display]\n") != 1 || !strings.Contains(text, `tooltip=a;b\n[display]\nmode=chips\\tail`) {
		t.Fatalf("INI frame permits injection or corrupts text: %s", text)
	}
}
