package rpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/history"
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

// §5.1 statusFromHookEvent — full mapping table for both agents, including the
// ⚠ never-emit "unknown" contract. An empty agent defaults to the claude map.
func TestStatusFromHookEvent(t *testing.T) {
	tests := []struct {
		agent string
		event string
		want  string
	}{
		// claude
		{"claude", "UserPromptSubmit", "working"},
		{"claude", "PostToolUse", "working"},
		{"claude", "PermissionRequest", "permission"},
		{"claude", "Stop", "idle"},
		{"claude", "SessionStart", "idle"},
		{"claude", "PreToolUse", ""}, // claude does not wire PreToolUse here
		{"claude", "Bogus", ""},
		{"claude", "", ""},
		// empty agent → claude mapping
		{"", "PostToolUse", "working"},
		// codex (adds PreToolUse → working)
		{"codex", "UserPromptSubmit", "working"},
		{"codex", "PreToolUse", "working"},
		{"codex", "PostToolUse", "working"},
		{"codex", "PermissionRequest", "permission"},
		{"codex", "Stop", "idle"},
		{"codex", "SessionStart", "idle"},
		{"codex", "PreCompact", ""}, // unmapped → status unchanged
		{"codex", "Bogus", ""},
	}
	for _, tt := range tests {
		t.Run(tt.agent+"/"+tt.event, func(t *testing.T) {
			got := statusFromHookEvent(tt.agent, tt.event)
			if got != tt.want {
				t.Errorf("statusFromHookEvent(%q, %q) = %q, want %q", tt.agent, tt.event, got, tt.want)
			}
			if got == "unknown" {
				t.Errorf("statusFromHookEvent emitted \"unknown\", which must never happen")
			}
		})
	}
}

// sessionLabel prefers the request-supplied id (which a hook carries before it
// is copied onto the session), then the session's own id, then "?"; and it
// names the chip by window title when known, falling back to cwd.
func TestSessionLabel(t *testing.T) {
	tests := []struct {
		name     string
		sess     *state.Session
		preferID string
		want     string
	}{
		{
			name:     "prefers request id and window title",
			sess:     &state.Session{CWD: "/home/u/proj", Wezterm: &state.WeztermInfo{WindowTitle: "checklist-supernode-proposal"}},
			preferID: "ce13c0f2-320b-4c97",
			want:     `session=ce13c0f2 "checklist-supernode-proposal"`,
		},
		{
			name: "falls back to session id and cwd",
			sess: &state.Session{CWD: "/home/u/proj", Claude: &state.ClaudeInfo{SessionID: "abcdef12-9999"}},
			want: "session=abcdef12 cwd=/home/u/proj",
		},
		{
			name: "unknown id renders as ?",
			sess: &state.Session{CWD: "/home/u/proj"},
			want: "session=? cwd=/home/u/proj",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sessionLabel(tt.sess, tt.preferID); got != tt.want {
				t.Errorf("sessionLabel = %q, want %q", got, tt.want)
			}
		})
	}
}

// handleHook logs every real status transition (with the driving event) and is
// silent on a no-op repeat — the forensic trail that lets a drifted chip be
// traced back to the hook that set it, while same-status hooks stay quiet.
func TestHandleHookLogsTransitions(t *testing.T) {
	store := state.New("")
	store.Apply(func(m map[int]*state.Session) {
		// Seed the session at the hook PID so findTrackedAncestor self-matches
		// and never touches the real /proc.
		m[42] = &state.Session{PID: 42, CWD: "/home/u/proj"}
	})
	s := New(store, "", terminal.NewNone(), wm.NewNone())

	var buf bytes.Buffer
	defer log.SetOutput(log.Writer())
	log.SetOutput(&buf)

	s.handleHook(Request{Cmd: "hook", Event: "Stop", PID: 42, SessionID: "ce13c0f2-aaaa"})
	s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42})

	out := buf.String()
	if !strings.Contains(out, "status: pid=42 session=ce13c0f2 cwd=/home/u/proj ->idle (agent=claude event=Stop)") {
		t.Errorf("missing idle transition line in:\n%s", out)
	}
	if !strings.Contains(out, "idle->working (agent=claude event=PostToolUse)") {
		t.Errorf("missing working transition line in:\n%s", out)
	}

	// A repeat of the current status logs nothing.
	buf.Reset()
	s.handleHook(Request{Cmd: "hook", Event: "UserPromptSubmit", PID: 42})
	if got := store.Snapshot().Sessions[0].Claude.Status; got != "working" {
		t.Fatalf("status = %q, want working (precondition)", got)
	}
	if buf.Len() != 0 {
		t.Errorf("no-op repeat logged %q, want silence", buf.String())
	}
}

// With an enabled history sink, every hook-driven status edge is mirrored into
// the durable activity log (one transition event per edge, carrying the
// from/to/agent and the closed interval's length), while a no-op repeat records
// nothing. This is the daemon-side wiring of the activity log.
func TestHandleHookRecordsHistoryTransitions(t *testing.T) {
	dir := t.TempDir()
	store := state.New("")
	store.Apply(func(m map[int]*state.Session) {
		m[42] = &state.Session{PID: 42, CWD: "/home/u/proj", Agent: state.AgentKindClaude}
	})
	sink := history.NewSink(history.Config{Enabled: true, Detail: history.DetailFull, Dir: dir})
	s := New(store, "", terminal.NewNone(), wm.NewNone())
	s.SetHistory(sink)

	s.handleHook(Request{Cmd: "hook", Event: "Stop", PID: 42, SessionID: "ce13c0f2-aaaa"})
	s.handleHook(Request{Cmd: "hook", Event: "UserPromptSubmit", PID: 42, SessionID: "ce13c0f2-aaaa"})
	s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, SessionID: "ce13c0f2-aaaa"}) // no-op (already working)
	sink.Close()

	evs := readHistoryEvents(t, dir)
	if len(evs) != 2 {
		t.Fatalf("recorded %d events, want 2 (idle, working; the repeat is silent):\n%+v", len(evs), evs)
	}
	if evs[0].Type != history.EventTransition || evs[0].To != "idle" {
		t.Errorf("event 0 = %+v, want a transition to idle", evs[0])
	}
	if evs[1].From != "idle" || evs[1].To != "working" {
		t.Errorf("event 1 = %+v, want idle->working", evs[1])
	}
	if evs[1].SessionID != "ce13c0f2-aaaa" || evs[1].Agent != "claude" || evs[1].CWD != "/home/u/proj" {
		t.Errorf("event 1 missing identity fields: %+v", evs[1])
	}
}

// readHistoryEvents reads every event across all day-files in dir, in file then
// line order.
func readHistoryEvents(t *testing.T, dir string) []history.Event {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read history dir: %v", err)
	}
	var evs []history.Event
	for _, e := range entries {
		f, err := os.Open(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var ev history.Event
			if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
				t.Fatalf("decode %q: %v", sc.Text(), err)
			}
			evs = append(evs, ev)
		}
		f.Close()
	}
	return evs
}

// A hook reaches the daemon only after Claude Code has recorded the entry that
// triggered it, so a transition must be dated from that transcript entry — not
// from the later moment the hook is processed. Anchoring kills the skew behind
// the hookless-recovery races: with a wall-clock StatusSince, a fast follow-up
// signal (e.g. an immediate Ctrl+C) carries a transcript timestamp OLDER than
// StatusSince and the reconciler discards it as stale. See docs/timing-hazards.md.
func TestHandleHookAnchorsStatusSinceToTranscript(t *testing.T) {
	promptTime := mustRPCTime(t, "2026-06-22T10:50:00Z")
	tpath := filepath.Join(t.TempDir(), "transcript.jsonl")
	line := `{"type":"user","timestamp":"2026-06-22T10:50:00Z","message":{"role":"user","content":[{"type":"text","text":"do the thing"}]}}`
	if err := os.WriteFile(tpath, []byte(line+"\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	store := state.New("")
	store.Apply(func(m map[int]*state.Session) {
		m[42] = &state.Session{PID: 42, CWD: "/p", Claude: &state.ClaudeInfo{
			Status: "idle", Transcript: tpath,
		}}
	})
	s := New(store, "", terminal.NewNone(), wm.NewNone())

	s.handleHook(Request{Cmd: "hook", Event: "UserPromptSubmit", PID: 42})

	got := store.Snapshot().Sessions[0].Claude
	if got.Status != "working" {
		t.Fatalf("status = %q, want working", got.Status)
	}
	if !got.StatusSince.Equal(promptTime) {
		t.Errorf("StatusSince = %v, want anchored to transcript entry %v (not wall-clock now)", got.StatusSince, promptTime)
	}
}

// A PostToolUse fires for EVERY tool that completes — including a sibling tool in
// the same turn or a background subagent's Task that lands while an interactive
// prompt is still waiting on the user. handleHook must not let such a PostToolUse
// clear a "permission" latch; the chip stays red until the transcript shows the
// main thread advanced past the prompt. This is the regression behind a red chip
// flashing green ~1s after the question appeared, before it was answered.
func TestHandleHookHoldsPermissionWhilePromptPending(t *testing.T) {
	since := mustRPCTime(t, "2026-06-22T10:50:41Z")

	cases := []struct {
		name       string
		lines      []string
		wantStatus string
	}{
		{
			// The prompt's assistant turn predates `since`; the only thing newer is
			// a bare tool_result from a concurrent tool. Not a resolution → hold red.
			name: "sibling tool_result keeps permission red",
			lines: []string{
				`{"type":"assistant","timestamp":"2026-06-22T10:50:30Z","message":{"role":"assistant","content":[{"type":"text","text":"let me ask"}]}}`,
				`{"type":"user","timestamp":"2026-06-22T10:50:42Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_sibling"}]}}`,
			},
			wantStatus: "permission",
		},
		{
			// The main thread produced an assistant message after `since` — the turn
			// resumed, so the prompt was answered → clear to working.
			name: "assistant message after prompt clears to working",
			lines: []string{
				`{"type":"assistant","timestamp":"2026-06-22T10:50:30Z","message":{"role":"assistant","content":[{"type":"text","text":"let me ask"}]}}`,
				`{"type":"assistant","timestamp":"2026-06-22T10:50:55Z","message":{"role":"assistant","content":[{"type":"text","text":"thanks, continuing"}]}}`,
			},
			wantStatus: "working",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tpath := filepath.Join(t.TempDir(), "transcript.jsonl")
			if err := os.WriteFile(tpath, []byte(strings.Join(tc.lines, "\n")+"\n"), 0o644); err != nil {
				t.Fatalf("write fixture: %v", err)
			}

			store := state.New("")
			store.Apply(func(m map[int]*state.Session) {
				m[42] = &state.Session{PID: 42, CWD: "/home/u/proj", Claude: &state.ClaudeInfo{
					Status: "permission", StatusSince: since, Transcript: tpath,
				}}
			})
			s := New(store, "", terminal.NewNone(), wm.NewNone())

			s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42})

			if got := store.Snapshot().Sessions[0].Claude.Status; got != tc.wantStatus {
				t.Errorf("status after PostToolUse = %q, want %q", got, tc.wantStatus)
			}
		})
	}
}

// A2: the identity-correlated fast path. A PermissionRequest stashes the tool it
// was raised for; a later PostToolUse whose tool_name MATCHES clears red at hook
// speed (no transcript needed), while a sibling/Task PostToolUse whose tool_name
// differs holds red and falls back to the transcript check.
func TestHandleHookEarlyClearByToolName(t *testing.T) {
	t.Run("matching tool_name clears red at hook speed", func(t *testing.T) {
		store := state.New("")
		store.Apply(func(m map[int]*state.Session) {
			m[42] = &state.Session{PID: 42, CWD: "/p"}
		})
		s := New(store, "", terminal.NewNone(), wm.NewNone())

		// Red onset captures the pending tool.
		s.handleHook(Request{Cmd: "hook", Event: "PermissionRequest", PID: 42, ToolName: "AskUserQuestion"})
		if got := store.Snapshot().Sessions[0].Claude.PendingTool; got != "AskUserQuestion" {
			t.Fatalf("PendingTool = %q, want AskUserQuestion", got)
		}
		// The approved tool's own PostToolUse clears it — no transcript involved.
		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "AskUserQuestion"})
		sess := store.Snapshot().Sessions[0]
		if sess.Claude.Status != "working" {
			t.Errorf("status = %q, want working (tool-name match clears)", sess.Claude.Status)
		}
		if sess.Claude.PendingTool != "" {
			t.Errorf("PendingTool = %q, want cleared on leaving red", sess.Claude.PendingTool)
		}
	})

	t.Run("non-matching Task PostToolUse holds red", func(t *testing.T) {
		// No transcript on disk, so the fallback is StateUnknown→hold: a background
		// Task completing while an AskUserQuestion is pending must not clear red.
		store := state.New("")
		store.Apply(func(m map[int]*state.Session) {
			m[42] = &state.Session{PID: 42, CWD: "/p"}
		})
		s := New(store, "", terminal.NewNone(), wm.NewNone())

		s.handleHook(Request{Cmd: "hook", Event: "PermissionRequest", PID: 42, ToolName: "AskUserQuestion"})
		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "Task"})
		if got := store.Snapshot().Sessions[0].Claude.Status; got != "permission" {
			t.Errorf("status = %q, want permission (sibling Task must not clear)", got)
		}
	})

	t.Run("tool-name match is tunable off", func(t *testing.T) {
		store := state.New("")
		store.Apply(func(m map[int]*state.Session) {
			m[42] = &state.Session{PID: 42, CWD: "/p"}
		})
		s := New(store, "", terminal.NewNone(), wm.NewNone())
		tun := s.tun
		tun.EarlyClearApproveByToolName = false
		s.SetTuning(tun)

		s.handleHook(Request{Cmd: "hook", Event: "PermissionRequest", PID: 42, ToolName: "AskUserQuestion"})
		s.handleHook(Request{Cmd: "hook", Event: "PostToolUse", PID: 42, ToolName: "AskUserQuestion"})
		// With the fast path off and no transcript, it falls back to hold.
		if got := store.Snapshot().Sessions[0].Claude.Status; got != "permission" {
			t.Errorf("status = %q, want permission (fast path disabled, no transcript → hold)", got)
		}
	})
}

// A codex hook routes its enrichment to the session's Codex block (not Claude),
// stamps the agent kind, and uses the codex event→status map. This is the
// multi-agent routing contract: one session map, per-agent blocks.
func TestHandleHookRoutesCodex(t *testing.T) {
	store := state.New("")
	store.Apply(func(m map[int]*state.Session) {
		m[7] = &state.Session{PID: 7, CWD: "/home/u/proj"}
	})
	s := New(store, "", terminal.NewNone(), wm.NewNone())

	s.handleHook(Request{Cmd: "hook", Agent: "codex", Event: "PermissionRequest", PID: 7, SessionID: "0199736b-codex"})

	sess := store.Snapshot().Sessions[0]
	if sess.Agent != "codex" {
		t.Errorf("session agent = %q, want codex", sess.Agent)
	}
	if sess.Claude != nil {
		t.Errorf("claude block populated for a codex hook: %+v", sess.Claude)
	}
	if sess.Codex == nil || sess.Codex.Status != "permission" {
		t.Errorf("codex block = %+v, want status=permission", sess.Codex)
	}
	if sess.Codex.SessionID != "0199736b-codex" {
		t.Errorf("codex session_id = %q, want 0199736b-codex", sess.Codex.SessionID)
	}
	// Enrichment() must surface the codex block for renderers.
	if got := sess.Enrichment(); got == nil || got.Status != "permission" {
		t.Errorf("Enrichment() = %+v, want the codex block", got)
	}
}

func mustRPCTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse time %q: %v", s, err)
	}
	return ts
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
