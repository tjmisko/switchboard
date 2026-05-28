package rpc

import (
	"context"
	"errors"
	"testing"

	"github.com/tjmisko/switchboard/internal/proc"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/terminal"
	"github.com/tjmisko/switchboard/internal/wm"
)

// §1.5 graceful degradation: on an Observe-only stack (wm or terminal is the
// none backend) focus returns the typed ErrNavigateUnsupported up front, rather
// than the confusing "session has no hyprland address yet" — even when sessions
// exist and one is fully resolved.
func TestFocusNavigateUnsupportedOnObserveOnlyStack(t *testing.T) {
	store := state.New("")
	store.Apply(func(m map[int]*state.Session) {
		m[42] = &state.Session{PID: 42, Hyprland: &state.HyprlandInfo{Address: "0xabc"}}
	})
	s := New(store, "", terminal.NewNone(), wm.NewNone())

	if err := s.focus(context.Background(), "active"); !errors.Is(err, ErrNavigateUnsupported) {
		t.Errorf("focus on Observe-only stack err = %v, want ErrNavigateUnsupported", err)
	}
}

// §5.1 statusFromHookEvent — full mapping table including the ⚠ never-emit
// "unknown" contract.
func TestStatusFromHookEvent(t *testing.T) {
	tests := []struct {
		event string
		want  string
	}{
		{"UserPromptSubmit", "working"},
		{"PostToolUse", "working"},
		{"PermissionRequest", "permission"},
		{"Stop", "idle"},
		{"SessionStart", "idle"},
		{"Bogus", ""},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.event, func(t *testing.T) {
			got := statusFromHookEvent(tt.event)
			if got != tt.want {
				t.Errorf("statusFromHookEvent(%q) = %q, want %q", tt.event, got, tt.want)
			}
			if got == "unknown" {
				t.Errorf("statusFromHookEvent emitted \"unknown\", which must never happen")
			}
		})
	}
}

// §5.2 pickSession — seed cases over a pure slice. The bare-number form keeps
// the ⚠ PID-vs-index collision for back-compat (decisions.md #3: "2" resolves
// to PID 2 when present, else index 2); the pid:/idx: prefixes added in Phase
// 1.5 are the unambiguous forms.
func TestPickSession(t *testing.T) {
	sessions := []state.Session{
		{PID: 100},
		{PID: 2, Focused: true},
		{PID: 300},
	}

	if got := pickSession(sessions, "active"); got == nil || got.PID != 2 {
		t.Errorf(`pickSession "active" = %v, want focused PID 2`, got)
	}
	// "2" matches PID 2 (index 1), NOT index 2 (PID 300) — the back-compat collision.
	if got := pickSession(sessions, "2"); got == nil || got.PID != 2 {
		t.Errorf(`pickSession "2" = %v, want PID 2 (not index 2)`, got)
	}
	// "0" matches no PID, falls back to index 0.
	if got := pickSession(sessions, "0"); got == nil || got.PID != 100 {
		t.Errorf(`pickSession "0" = %v, want index 0 (PID 100)`, got)
	}
	if got := pickSession(sessions, "nope"); got != nil {
		t.Errorf(`pickSession "nope" = %v, want nil`, got)
	}
	if got := pickSession(sessions, "99"); got != nil {
		t.Errorf(`pickSession "99" = %v, want nil (no PID, out of range)`, got)
	}
	// Negative numeric selector parses but matches no PID and is below index 0.
	if got := pickSession(sessions, "-1"); got != nil {
		t.Errorf(`pickSession "-1" = %v, want nil`, got)
	}

	// Explicit pid: selector resolves only by PID, never by index.
	if got := pickSession(sessions, "pid:300"); got == nil || got.PID != 300 {
		t.Errorf(`pickSession "pid:300" = %v, want PID 300`, got)
	}
	// "pid:2" is unambiguous — PID 2, same as the bare heuristic here.
	if got := pickSession(sessions, "pid:2"); got == nil || got.PID != 2 {
		t.Errorf(`pickSession "pid:2" = %v, want PID 2`, got)
	}
	// A pid: with no matching session is nil, even though that number is a valid
	// index — the prefix disables the index fallback.
	if got := pickSession(sessions, "pid:0"); got != nil {
		t.Errorf(`pickSession "pid:0" = %v, want nil (no PID 0; index fallback disabled)`, got)
	}
	if got := pickSession(sessions, "pid:bad"); got != nil {
		t.Errorf(`pickSession "pid:bad" = %v, want nil`, got)
	}
	// Explicit idx: selector resolves only by position. "idx:2" is PID 300 —
	// the very session the bare-"2" heuristic shadows behind PID 2.
	if got := pickSession(sessions, "idx:2"); got == nil || got.PID != 300 {
		t.Errorf(`pickSession "idx:2" = %v, want index 2 (PID 300)`, got)
	}
	if got := pickSession(sessions, "idx:0"); got == nil || got.PID != 100 {
		t.Errorf(`pickSession "idx:0" = %v, want index 0 (PID 100)`, got)
	}
	if got := pickSession(sessions, "idx:99"); got != nil {
		t.Errorf(`pickSession "idx:99" = %v, want nil (out of range)`, got)
	}

	// "active"/"" with none focused falls back to sessions[0].
	noneFocused := []state.Session{{PID: 7}, {PID: 8}}
	if got := pickSession(noneFocused, ""); got == nil || got.PID != 7 {
		t.Errorf(`pickSession "" (none focused) = %v, want PID 7`, got)
	}
}

// chainReader builds a readProc that maps each pid to its ppid; an unknown pid
// returns ErrGone. It counts calls so the depth bound can be asserted.
func chainReader(chain map[int]int, calls *int) func(int) (proc.Info, error) {
	return func(pid int) (proc.Info, error) {
		*calls++
		ppid, ok := chain[pid]
		if !ok {
			return proc.Info{}, errors.New("gone")
		}
		return proc.Info{PID: pid, PPID: ppid}, nil
	}
}

func tracked(pids ...int) map[int]*state.Session {
	m := map[int]*state.Session{}
	for _, p := range pids {
		m[p] = &state.Session{PID: p}
	}
	return m
}

// §5.5 findTrackedAncestor — self-match returns immediately without reading.
func TestFindTrackedAncestorSelfMatch(t *testing.T) {
	read := func(int) (proc.Info, error) {
		t.Fatal("readProc must not be called on a self-match")
		return proc.Info{}, nil
	}
	if got := findTrackedAncestor(tracked(100), 100, read); got != 100 {
		t.Errorf("self-match = %d, want 100", got)
	}
}

// §5.5 findTrackedAncestor — walks the ppid chain to the first tracked ancestor.
func TestFindTrackedAncestorWalksChain(t *testing.T) {
	calls := 0
	chain := map[int]int{102: 101, 101: 100, 100: 1}
	if got := findTrackedAncestor(tracked(100), 102, chainReader(chain, &calls)); got != 100 {
		t.Errorf("walk = %d, want 100", got)
	}
}

// §5.5 ⚠ characterization: the walk inspects depths 0..19 only. A tracked
// ancestor reachable only at depth 20 is NOT found, and readProc is called
// exactly 20 times.
func TestFindTrackedAncestorDepthBound(t *testing.T) {
	chain := map[int]int{}
	for p := 100; p < 200; p++ {
		chain[p] = p + 1 // 100->101->...->199
	}
	calls := 0
	// Only pid 120 (reachable at depth 20 from 100) is tracked.
	if got := findTrackedAncestor(tracked(120), 100, chainReader(chain, &calls)); got != 0 {
		t.Errorf("depth-20 ancestor = %d, want 0 (out of bound)", got)
	}
	if calls != 20 {
		t.Errorf("readProc called %d times, want 20 (depths 0..19)", calls)
	}

	// The same ancestor one hop closer (depth 19) IS found.
	calls = 0
	if got := findTrackedAncestor(tracked(119), 100, chainReader(chain, &calls)); got != 119 {
		t.Errorf("depth-19 ancestor = %d, want 119", got)
	}
}

// §5.5 findTrackedAncestor — pid<=1, a read error, and ppid==0 all return 0.
func TestFindTrackedAncestorTerminators(t *testing.T) {
	calls := 0
	if got := findTrackedAncestor(tracked(100), 1, chainReader(nil, &calls)); got != 0 {
		t.Errorf("pid<=1 = %d, want 0", got)
	}
	if got := findTrackedAncestor(tracked(100), 50, chainReader(map[int]int{}, &calls)); got != 0 {
		t.Errorf("read error = %d, want 0", got)
	}
	if got := findTrackedAncestor(tracked(100), 50, chainReader(map[int]int{50: 0}, &calls)); got != 0 {
		t.Errorf("ppid==0 = %d, want 0", got)
	}
}
