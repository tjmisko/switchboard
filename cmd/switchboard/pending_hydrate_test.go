package main

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/transcript"
)

// T12 (docs/subagent-permission-plan.md §9): prompt OWNERSHIP must survive a
// daemon restart, because PermissionRequest is edge-triggered, no hook re-raises a
// live prompt, and a blocked writer runs no tools — so a dropped entry is a
// permanent missed RED, not a transient one the next tick repairs.
//
// The transcripts may only SUBTRACT from what was persisted. An unmatched tool_use
// covers "awaiting approval" and "executing right now" alike, so deriving
// ownership from it would raise a false RED on every session that was mid-tool at
// restart; its absence, though, does prove the writer is unblocked.

const (
	// hydrateBlocked is a writer still waiting: an assistant tool_use whose
	// tool_result never landed.
	hydrateBlocked = `{"type":"assistant","timestamp":"2026-06-22T10:50:31Z","message":{"role":"assistant","stop_reason":"tool_use","content":[{"type":"tool_use","id":"toolu_gated","name":"Bash"}]}}
`
	// hydrateAnswered is the same writer after the prompt was answered while the
	// daemon was down — the window the Since := startup re-stamp would otherwise
	// hide, and the reason the falsifier exists at all.
	hydrateAnswered = hydrateBlocked + `{"type":"user","timestamp":"2026-06-22T10:55:04Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_gated"}]}}
`
	// hydrateParallel is trap 3a: a gated tool_use plus an auto-approved sibling
	// from one assistant message, with only the sibling's result back. The
	// TRAILING tool_use is matched while the prompt still waits.
	hydrateParallel = `{"type":"assistant","timestamp":"2026-06-22T10:50:31Z","message":{"role":"assistant","stop_reason":null,"content":[{"type":"tool_use","id":"toolu_gated","name":"Bash"}]}}
{"type":"assistant","timestamp":"2026-06-22T10:50:31Z","message":{"role":"assistant","stop_reason":"tool_use","content":[{"type":"tool_use","id":"toolu_sibling","name":"Read"}]}}
{"type":"user","timestamp":"2026-06-22T10:50:33Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_sibling"}]}}
`
)

// hydrateFixture lays out one session's transcript tree — <sid>.jsonl beside
// <sid>/subagents/agent-<id>.jsonl — and returns the main transcript path. A
// writer whose body is empty gets no file at all.
func hydrateFixture(t *testing.T, sid string, bodies map[string]string) string {
	t.Helper()
	base := t.TempDir()
	main := filepath.Join(base, sid+".jsonl")
	subdir := filepath.Join(base, sid, "subagents")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	for writer, body := range bodies {
		if body == "" {
			continue
		}
		path := main
		if writer != "" {
			path = filepath.Join(subdir, "agent-"+writer+".jsonl")
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return main
}

// hydrateFromMirror writes a state.json holding one claude session with the given
// status and persisted writers, then runs the real startup path — Load followed by
// dropStaleSessions — against a live pid, and returns the resulting block.
func hydrateFromMirror(t *testing.T, pid int, sid, mainTranscript, status string, writers []string) *state.AgentInfo {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.json")

	seed := state.New(path)
	seed.Apply(func(m map[int]*state.Session) {
		info := &state.AgentInfo{SessionID: sid, Transcript: mainTranscript, Status: status}
		for _, w := range writers {
			// Correlators are set here and must NOT survive: only the key set is
			// persisted (§9.5).
			info.SetPending(w, state.PendingPrompt{Tool: "Bash", InputHash: "cafe1234"})
		}
		m[pid] = &state.Session{PID: pid, Agent: state.AgentKindClaude, CWD: "/home/u/proj", Claude: info}
	})

	return hydrateMirrorAt(t, path, pid)
}

// hydrateMirrorAt runs Load + dropStaleSessions over an existing mirror file.
func hydrateMirrorAt(t *testing.T, path string, pid int) *state.AgentInfo {
	t.Helper()
	store := state.New(path)
	if err := store.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	sink := history.NewSink(history.Config{})
	dropStaleSessions(store, fakeProcSource{st: map[int]procState{pid: procAlive}}, sink, nil, transcript.DefaultTailBytes)

	var got *state.AgentInfo
	store.Apply(func(m map[int]*state.Session) {
		if m[pid] == nil {
			t.Fatalf("pid %d dropped by the stale sweep", pid)
		}
		got = m[pid].Claude
	})
	return got
}

func pendingWriterSet(info *state.AgentInfo) []string {
	writers := info.PendingWriterKeys()
	for i, w := range writers {
		if w == "" {
			writers[i] = state.PendingWriterMain
		}
	}
	sort.Strings(writers)
	return writers
}

func TestHydratePendingKeepsOwnershipAcrossARestart(t *testing.T) {
	const sid = "5318eb5b-79df-4dee-a9f8-c80df4eca79e"
	const teammate = "af5bd126402ac16c7"

	t.Run("should keep a subagent-owned red when that subagent's jsonl still holds an unmatched tool_use", func(t *testing.T) {
		main := hydrateFixture(t, sid, map[string]string{teammate: hydrateBlocked})
		got := hydrateFromMirror(t, 42, sid, main, state.StatusPermission, []string{teammate})

		if got.Status != state.StatusPermission {
			t.Errorf("status = %q, want permission", got.Status)
		}
		if want := []string{teammate}; !equalStrings(pendingWriterSet(got), want) {
			t.Errorf("pending writers = %v, want %v — losing the owner turns this into a main-thread red that the first main assistant message clears", pendingWriterSet(got), want)
		}
	})

	t.Run("should drop a hydrated entry when the subagent's jsonl shows the matching tool_result", func(t *testing.T) {
		// Answered while the daemon was down. Since := startup makes the
		// pre-restart resolution invisible to ResolveKind, so without the falsifier
		// this red latches until the cap.
		main := hydrateFixture(t, sid, map[string]string{teammate: hydrateAnswered})
		got := hydrateFromMirror(t, 42, sid, main, state.StatusPermission, []string{teammate})

		if len(got.Pending) != 0 {
			t.Errorf("pending writers = %v, want none (the tool returned, so its gate opened)", pendingWriterSet(got))
		}
	})

	t.Run("should keep a main-owned red when the main transcript holds an unmatched tool_use", func(t *testing.T) {
		// V4 (§9.7): the main jsonl DOES carry the pending tool_use while the prompt
		// waits, so the falsifier applies to main-thread entries too — the a != main
		// gate trap 3 once demanded must not be built.
		main := hydrateFixture(t, sid, map[string]string{"": hydrateBlocked})
		got := hydrateFromMirror(t, 42, sid, main, state.StatusPermission, []string{""})

		if want := []string{state.PendingWriterMain}; !equalStrings(pendingWriterSet(got), want) {
			t.Errorf("pending writers = %v, want %v", pendingWriterSet(got), want)
		}
	})

	t.Run("should keep a main-owned red when the trailing tool_use is matched but an earlier parallel sibling is not", func(t *testing.T) {
		// Trap 3a. A trailing-only falsifier drops this red while the user is still
		// looking at the prompt.
		main := hydrateFixture(t, sid, map[string]string{"": hydrateParallel})
		got := hydrateFromMirror(t, 42, sid, main, state.StatusPermission, []string{""})

		if want := []string{state.PendingWriterMain}; !equalStrings(pendingWriterSet(got), want) {
			t.Errorf("pending writers = %v, want %v — the gated tool_use is unmatched even though the LAST one is not", pendingWriterSet(got), want)
		}
	})

	t.Run("should keep a main-owned red when only a subagent's jsonl shows a matching tool_result", func(t *testing.T) {
		// Trap 3b: each writer is falsified against its OWN file. Cross them and a
		// busy teammate's completed tool clears the main thread's prompt.
		main := hydrateFixture(t, sid, map[string]string{"": hydrateBlocked, teammate: hydrateAnswered})
		got := hydrateFromMirror(t, 42, sid, main, state.StatusPermission, []string{""})

		if want := []string{state.PendingWriterMain}; !equalStrings(pendingWriterSet(got), want) {
			t.Errorf("pending writers = %v, want %v", pendingWriterSet(got), want)
		}
	})

	t.Run("should keep a subagent-owned red when only the main transcript is fully matched", func(t *testing.T) {
		// Trap 3b read the other way, and the measured shape of a subagent-raised
		// prompt: the main tail is matched throughout while the raising agent-*.jsonl
		// carries the unmatched tool_use.
		main := hydrateFixture(t, sid, map[string]string{"": hydrateAnswered, teammate: hydrateBlocked})
		got := hydrateFromMirror(t, 42, sid, main, state.StatusPermission, []string{teammate})

		if want := []string{teammate}; !equalStrings(pendingWriterSet(got), want) {
			t.Errorf("pending writers = %v, want %v", pendingWriterSet(got), want)
		}
	})

	t.Run("should keep a hydrated entry when its writer's transcript is unreadable", func(t *testing.T) {
		// Unreadable / truncated / tail-window-missed all mean KEEP, matching
		// permissionExit's unreadable handling.
		main := hydrateFixture(t, sid, map[string]string{"": hydrateBlocked})
		got := hydrateFromMirror(t, 42, sid, main, state.StatusPermission, []string{"a-writer-with-no-file"})

		if want := []string{"a-writer-with-no-file"}; !equalStrings(pendingWriterSet(got), want) {
			t.Errorf("pending writers = %v, want %v (no evidence is not evidence of resolution)", pendingWriterSet(got), want)
		}
	})

	t.Run("should keep only the writers still blocked when one of two prompts was answered while down", func(t *testing.T) {
		main := hydrateFixture(t, sid, map[string]string{"": hydrateBlocked, teammate: hydrateAnswered})
		got := hydrateFromMirror(t, 42, sid, main, state.StatusPermission, []string{"", teammate})

		if want := []string{state.PendingWriterMain}; !equalStrings(pendingWriterSet(got), want) {
			t.Errorf("pending writers = %v, want %v", pendingWriterSet(got), want)
		}
		if got.Status != state.StatusPermission {
			t.Errorf("status = %q, want permission — one writer is still blocked", got.Status)
		}
	})

	t.Run("should re-stamp every surviving prompt to startup time rather than restore its onset", func(t *testing.T) {
		main := hydrateFixture(t, sid, map[string]string{teammate: hydrateBlocked})
		got := hydrateFromMirror(t, 42, sid, main, state.StatusPermission, []string{teammate})

		p := got.Pending[teammate]
		if p.Since.IsZero() {
			t.Error("hydrated prompt has a zero Since; every pre-restart transcript entry would read as resolved-after")
		}
		if p.Tool != "" || p.InputHash != "" {
			t.Errorf("hydrated prompt = %+v, want the correlators dropped (they are re-earned from the next hook)", p)
		}
		if got.PendingTool != "" {
			t.Errorf("PendingTool = %q, want empty so the hook-path fast match cannot fire on a hydrated entry (trap 2)", got.PendingTool)
		}
	})
}

// Trap 4: the three hydrate combinations, handled explicitly rather than by
// accident. The version boundary is the interesting one — a mirror written before
// the field existed carries a red with no owner at all.
func TestHydratePendingHandlesEveryMirrorCombination(t *testing.T) {
	const sid = "5318eb5b-79df-4dee-a9f8-c80df4eca79e"

	t.Run("should seed the main thread when a permission chip has no persisted writers", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state.json")
		legacy := `{"sessions":[{"pid":42,"cwd":"/p","tty":"/dev/pts/1","started_at":"2026-05-28T09:00:00Z","agent":"claude",` +
			`"claude":{"session_id":"` + sid + `","status":"permission"}}],"updated_at":"2026-05-28T09:05:00Z"}`
		if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
			t.Fatal(err)
		}

		got := hydrateMirrorAt(t, path, 42)

		if got.Status != state.StatusPermission {
			t.Errorf("status = %q, want permission (status was always on the wire; only its owner was lost)", got.Status)
		}
		if want := []string{state.PendingWriterMain}; !equalStrings(pendingWriterSet(got), want) {
			t.Errorf("pending writers = %v, want %v — an ownerless red must become a main-thread red, which reproduces the pre-T12 behavior exactly", pendingWriterSet(got), want)
		}
	})

	t.Run("should not re-seed the main thread when the falsifier emptied a persisted writer set", func(t *testing.T) {
		// The seed keys off whether writers were PERSISTED, not off whether the map
		// is empty now — re-seeding would manufacture the very red the falsifier
		// just proved resolved.
		main := hydrateFixture(t, sid, map[string]string{"af5bd126402ac16c7": hydrateAnswered})
		got := hydrateFromMirror(t, 42, sid, main, state.StatusPermission, []string{"af5bd126402ac16c7"})

		if len(got.Pending) != 0 {
			t.Errorf("pending writers = %v, want none", pendingWriterSet(got))
		}
	})

	t.Run("should re-fold to permission when a non-red chip carries persisted writers", func(t *testing.T) {
		// Unreachable by construction, which is exactly why it is handled: Pending is
		// the authority post-T5, and a silent disagreement is how a missed RED hides.
		main := hydrateFixture(t, sid, map[string]string{"": hydrateBlocked})
		got := hydrateFromMirror(t, 42, sid, main, state.StatusWorking, []string{""})

		if got.Status != state.StatusPermission {
			t.Errorf("status = %q, want permission — the map outranks the persisted status", got.Status)
		}
		if want := []string{state.PendingWriterMain}; !equalStrings(pendingWriterSet(got), want) {
			t.Errorf("pending writers = %v, want %v", pendingWriterSet(got), want)
		}
	})

	t.Run("should leave a non-red chip alone when it carries no persisted writers", func(t *testing.T) {
		main := hydrateFixture(t, sid, map[string]string{"": hydrateBlocked})
		got := hydrateFromMirror(t, 42, sid, main, state.StatusWorking, nil)

		if got.Status != state.StatusWorking {
			t.Errorf("status = %q, want working — an unmatched tool_use is a running tool, not a prompt", got.Status)
		}
		if len(got.Pending) != 0 {
			t.Errorf("pending writers = %v, want none: the transcripts may falsify ownership, never manufacture it", pendingWriterSet(got))
		}
	})
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
