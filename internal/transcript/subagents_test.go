package transcript

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// bigTaskUse is an Agent tool_use spawn whose input.prompt is padded to padKB so a
// handful of them overflow the 128 KiB tail window — the scenario that makes the
// tail-bounded Tasks() drop an early spawn.
func bigTaskUse(ts, id string, padKB int) string {
	pad := strings.Repeat("x", padKB*1024)
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"role":"assistant","content":[{"type":"tool_use","id":%q,"name":"Agent","input":{"subagent_type":"Explore","description":"d","prompt":%q}}]}}`, ts, id, pad)
}

// bgTaskUse is an Agent tool_use launched in the background (run_in_background
// true), as Claude Code records a backgrounded fanout.
func bgTaskUse(ts, id string) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"role":"assistant","content":[{"type":"tool_use","id":%q,"name":"Agent","input":{"subagent_type":"general-purpose","description":"bg","run_in_background":true}}]}}`, ts, id)
}

// launchAckResult is the tool_result Claude Code returns to ACKNOWLEDGE a spawn
// rather than to report its outcome. Content is the block-array shape the real
// transcripts use, and the wording is passed in so both live variants can be
// exercised — see transcript.launchAckPrefixes.
func launchAckResult(ts, id, wording string) string {
	text := wording + ". (This tool result is internal metadata — never quote or paste any part of it.)\nagentId: a1b2c3\nThe agent is working in the background. You will be notified automatically when it completes."
	return fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":[{"type":"text","text":%q}]}]}}`, ts, id, text)
}

// stringResult is a tool_result whose content is a BARE STRING rather than a
// block array — the other shape Claude Code writes, which resultText must also
// flatten before the ack prefixes can be matched against it.
func stringResult(ts, id, text string) string {
	return fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":%q}]}}`, ts, id, text)
}

// resultByID indexes a TasksSince result set by the tool_use it answers.
func resultByID(results []TaskResult, id string) (TaskResult, bool) {
	for _, r := range results {
		if r.ToolUseID == id {
			return r, true
		}
	}
	return TaskResult{}, false
}

// subagentTerminalLine is the assistant entry that ends a finished subagent's own
// transcript: stop_reason end_turn (its turn ended naturally).
func subagentTerminalLine(ts string) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"done"}]}}`, ts)
}

// subagentWorkingLine is an assistant tool_use entry (stop_reason tool_use): the
// last line of an agent still mid-turn, which must NOT read as terminal.
func subagentWorkingLine(ts string) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"role":"assistant","stop_reason":"tool_use","content":[{"type":"tool_use","id":"toolu_z","name":"Read"}]}}`, ts)
}

func writeFile(t *testing.T, path string, lines []string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func appendFile(t *testing.T, path string, lines []string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open append %s: %v", path, err)
	}
	defer f.Close()
	if _, err := f.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		t.Fatalf("append %s: %v", path, err)
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Size()
}

func taskIDs(tasks []Task) map[string]bool {
	m := make(map[string]bool, len(tasks))
	for _, t := range tasks {
		m[t.ID] = true
	}
	return m
}

func taskByID(tasks []Task, id string) (Task, bool) {
	for _, t := range tasks {
		if t.ID == id {
			return t, true
		}
	}
	return Task{}, false
}

// TestTasksSinceCatchesSpawnsTailDrops is the core motivation: a turn large enough
// to overflow the 128 KiB tail window scrolls an early spawn — and the result of a
// spawn whose tool_result lands far away — out of Tasks()' reach, while the forward
// cursor, threaded across reads, catches every spawn and the straddling result.
func TestTasksSinceCatchesSpawnsTailDrops(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")

	// Part 1: an early spawn, then five large spawns whose ~200 KiB of padding
	// pushes the early one well outside a 128 KiB tail window.
	allIDs := []string{"toolu_early"}
	lines := []string{bigTaskUse("2026-06-01T21:39:00Z", "toolu_early", 4)}
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("toolu_b%d", i)
		allIDs = append(allIDs, id)
		lines = append(lines, bigTaskUse(fmt.Sprintf("2026-06-01T21:39:%02dZ", 10+i), id, 40))
	}
	writeFile(t, path, lines)

	// The tail-bounded reader drops the early spawn: its launching tool_use has
	// scrolled out of the 128 KiB window.
	tail, err := Tasks(path, 128*1024)
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if _, ok := taskByID(tail, "toolu_early"); ok {
		t.Fatal("Tasks(128 KiB) saw toolu_early; the test needs it scrolled out of the window")
	}

	// The forward cursor from 0 catches every spawn, including the early one; no
	// tool_result has landed yet.
	spawns1, results1, off1, err := TasksSince(path, 0)
	if err != nil {
		t.Fatalf("TasksSince: %v", err)
	}
	got := taskIDs(spawns1)
	for _, id := range allIDs {
		if !got[id] {
			t.Errorf("TasksSince(0) missed spawn %q", id)
		}
	}
	if len(results1) != 0 {
		t.Errorf("results = %v, want none yet", results1)
	}
	if want := fileSize(t, path); off1 != want {
		t.Errorf("newOffset = %d, want EOF %d", off1, want)
	}

	// Part 2: the early spawn's tool_result lands far after its spawn — it straddles
	// the tail window.
	appendFile(t, path, []string{taskResult("2026-06-01T21:45:00Z", "toolu_early")})

	// Even now the tail window cannot pair the straddling result: it sees the result
	// but not the spawn, so toolu_early stays absent from Tasks.
	tail2, err := Tasks(path, 128*1024)
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if _, ok := taskByID(tail2, "toolu_early"); ok {
		t.Fatal("Tasks(128 KiB) paired the straddling result; expected the spawn still out of window")
	}

	// Threaded from the first call's offset, the second read sees only the new
	// result — nothing from the first delta double-counted.
	spawns2, results2, _, err := TasksSince(path, off1)
	if err != nil {
		t.Fatalf("TasksSince: %v", err)
	}
	if len(spawns2) != 0 {
		t.Errorf("second read spawns = %v, want none", spawns2)
	}
	if len(results2) != 1 || results2[0].ToolUseID != "toolu_early" || results2[0].LaunchAck {
		t.Errorf("second read results = %v, want [{toolu_early false}]", results2)
	}
}

// TestTasksSinceOffsetThreading verifies two sequential calls — the second resuming
// at the first's newOffset — together see every spawn and result exactly once, and
// that a partial trailing line is excluded (the offset lands on a line boundary and
// the partial is re-read whole next call).
func TestTasksSinceOffsetThreading(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")

	// Two complete spawn lines, then a THIRD spawn written as a partial trailing
	// line (no terminating newline) — as if caught mid-flush.
	head := strings.Join([]string{
		taskUse("2026-06-01T21:39:00Z", "toolu_a"),
		taskUse("2026-06-01T21:39:01Z", "toolu_b"),
	}, "\n") + "\n"
	full3 := taskUse("2026-06-01T21:39:02Z", "toolu_c")
	split := len(full3) / 2
	if err := os.WriteFile(path, []byte(head+full3[:split]), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// First read: the two complete spawns; the partial third is excluded and the
	// offset sits at the boundary after toolu_b.
	spawns1, _, off1, err := TasksSince(path, 0)
	if err != nil {
		t.Fatalf("TasksSince: %v", err)
	}
	if g := taskIDs(spawns1); !g["toolu_a"] || !g["toolu_b"] || g["toolu_c"] {
		t.Errorf("first read = %v, want {toolu_a,toolu_b} (toolu_c partial, excluded)", g)
	}
	if off1 != int64(len(head)) {
		t.Errorf("newOffset = %d, want line boundary %d (after toolu_b)", off1, len(head))
	}

	// Complete the partial third line and append a tool_result for toolu_a.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	if _, err := f.WriteString(full3[split:] + "\n" + taskResult("2026-06-01T21:45:00Z", "toolu_a") + "\n"); err != nil {
		f.Close()
		t.Fatalf("append: %v", err)
	}
	f.Close()

	// Second read threaded from off1: the now-complete third spawn and the result,
	// each seen exactly once — nothing from the first delta re-read.
	spawns2, results2, _, err := TasksSince(path, off1)
	if err != nil {
		t.Fatalf("TasksSince: %v", err)
	}
	if g := taskIDs(spawns2); !g["toolu_c"] || g["toolu_a"] || g["toolu_b"] {
		t.Errorf("second read = %v, want {toolu_c} only", g)
	}
	if len(results2) != 1 || results2[0].ToolUseID != "toolu_a" || results2[0].LaunchAck {
		t.Errorf("second read results = %v, want [{toolu_a false}]", results2)
	}
}

// TestTasksSinceBackground checks the run_in_background tool_use input surfaces as
// Task.Background; a foreground spawn carries Background false.
func TestTasksSinceBackground(t *testing.T) {
	path := writeTranscript(t,
		bgTaskUse("2026-06-01T21:39:00Z", "toolu_bg"),
		taskUse("2026-06-01T21:39:01Z", "toolu_fg"),
	)
	spawns, _, _, err := TasksSince(path, 0)
	if err != nil {
		t.Fatalf("TasksSince: %v", err)
	}
	if bg, ok := taskByID(spawns, "toolu_bg"); !ok || !bg.Background {
		t.Errorf("toolu_bg: found=%v Background=%v, want found=true Background=true", ok, bg.Background)
	}
	if fg, ok := taskByID(spawns, "toolu_fg"); !ok || fg.Background {
		t.Errorf("toolu_fg: found=%v Background=%v, want found=true Background=false", ok, fg.Background)
	}
}

// TestSubagentsForTranscript reads a fixture subagents/ dir spanning every shape:
// a finished fanout (full meta + end_turn jsonl), a running one (minimal meta +
// non-terminal jsonl), an ORPHAN jsonl with no meta (HasMeta false), and a
// META-ONLY spawn with no jsonl yet (Done false, ModTime zero). It confirms the
// union enumeration by filename stem, the last-line Done rule, ModTime, HasMeta,
// and defensive field parsing.
func TestSubagentsForTranscript(t *testing.T) {
	dir := t.TempDir()
	transcriptPath := filepath.Join(dir, "sess.jsonl")
	subagentsDir := filepath.Join(dir, "sess", "subagents")
	if err := os.MkdirAll(subagentsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// done: full meta (toolUseId + spawnDepth present); jsonl's last line is end_turn.
	writeFile(t, filepath.Join(subagentsDir, "agent-done.meta.json"),
		[]string{`{"agentType":"Explore","description":"map the codebase","toolUseId":"toolu_done","spawnDepth":1}`})
	writeFile(t, filepath.Join(subagentsDir, "agent-done.jsonl"),
		[]string{subagentWorkingLine("2026-06-01T21:39:00Z"), subagentTerminalLine("2026-06-01T21:39:30Z")})

	// run: MINIMAL meta (only agentType — defensive parse); an earlier end_turn must
	// NOT mark it Done because the LAST line is a tool_use.
	writeFile(t, filepath.Join(subagentsDir, "agent-run.meta.json"),
		[]string{`{"agentType":"general-purpose"}`})
	writeFile(t, filepath.Join(subagentsDir, "agent-run.jsonl"),
		[]string{subagentTerminalLine("2026-06-01T21:39:00Z"), subagentWorkingLine("2026-06-01T21:40:00Z")})

	// orphan: a jsonl with NO sibling meta — still reported, HasMeta false,
	// AgentType empty, Done read from its (terminal) last line.
	writeFile(t, filepath.Join(subagentsDir, "agent-orphan.jsonl"),
		[]string{subagentTerminalLine("2026-06-01T21:41:00Z")})

	// metaonly: a meta with NO jsonl yet (just spawned) — Done false, ModTime zero,
	// TaskKind parsed.
	writeFile(t, filepath.Join(subagentsDir, "agent-metaonly.meta.json"),
		[]string{`{"agentType":"general-purpose","toolUseId":"toolu_mo","taskKind":"in_process_teammate"}`})

	subs, err := SubagentsForTranscript(transcriptPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(subs) != 4 {
		t.Fatalf("got %d subagents, want 4 (union of meta + jsonl): %+v", len(subs), subs)
	}
	// Key on AgentID — the filename stem — which is the universal join.
	byID := map[string]Subagent{}
	for _, s := range subs {
		byID[s.AgentID] = s
	}

	done, ok := byID["done"]
	if !ok {
		t.Fatalf("missing AgentID \"done\"; got %+v", subs)
	}
	if !done.Done {
		t.Error("agent \"done\": Done = false, want true (last line is end_turn)")
	}
	if !done.HasMeta || done.ModTime.IsZero() {
		t.Errorf("agent \"done\": HasMeta=%v ModTime.zero=%v, want true/false", done.HasMeta, done.ModTime.IsZero())
	}
	if done.AgentType != "Explore" || done.ToolUseID != "toolu_done" || done.Description != "map the codebase" || done.SpawnDepth != 1 {
		t.Errorf("agent \"done\" meta mis-parsed: %+v", done)
	}

	run, ok := byID["run"]
	if !ok {
		t.Fatalf("missing AgentID \"run\"; got %+v", subs)
	}
	if run.Done {
		t.Error("agent \"run\": Done = true, want false (last line is a tool_use)")
	}
	// Heterogeneous/defensive: absent fields → zero values, no error.
	if !run.HasMeta || run.AgentType != "general-purpose" || run.ToolUseID != "" || run.Description != "" || run.SpawnDepth != 0 || run.TaskKind != "" {
		t.Errorf("agent \"run\" minimal meta mis-parsed (absent fields → zero): %+v", run)
	}

	orphan, ok := byID["orphan"]
	if !ok {
		t.Fatalf("missing AgentID \"orphan\" (a jsonl with no meta must still be reported); got %+v", subs)
	}
	if orphan.HasMeta || orphan.AgentType != "" {
		t.Errorf("agent \"orphan\": HasMeta=%v AgentType=%q, want false/empty", orphan.HasMeta, orphan.AgentType)
	}
	if !orphan.Done || orphan.ModTime.IsZero() {
		t.Errorf("agent \"orphan\": Done=%v ModTime.zero=%v, want true/false (terminal jsonl)", orphan.Done, orphan.ModTime.IsZero())
	}

	metaonly, ok := byID["metaonly"]
	if !ok {
		t.Fatalf("missing AgentID \"metaonly\" (a meta with no jsonl must still be reported); got %+v", subs)
	}
	if !metaonly.HasMeta || metaonly.TaskKind != "in_process_teammate" || metaonly.ToolUseID != "toolu_mo" {
		t.Errorf("agent \"metaonly\" meta mis-parsed: %+v", metaonly)
	}
	// No jsonl yet → not Done, but ModTime falls back to the meta.json's mtime so
	// the just-spawned fanout is still datable.
	if metaonly.Done || metaonly.ModTime.IsZero() {
		t.Errorf("agent \"metaonly\": Done=%v ModTime.zero=%v, want false/false (ModTime from meta.json)", metaonly.Done, metaonly.ModTime.IsZero())
	}
}

func TestSubagentsForTranscriptAbsentDir(t *testing.T) {
	// No sibling <session>/subagents dir → the session had no fanouts → (nil, nil).
	subs, err := SubagentsForTranscript(filepath.Join(t.TempDir(), "sess.jsonl"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if subs != nil {
		t.Errorf("got %+v, want nil (absent dir)", subs)
	}
}

func TestSubagentsForTranscriptEmptyPath(t *testing.T) {
	if _, err := SubagentsForTranscript(""); err == nil {
		t.Error("empty path: err = nil, want error")
	}
}

// TestSubagentsModTimePrefersJSONL locks the ModTime precedence: when both files
// exist, ModTime is the jsonl's mtime (the activity timestamp), not the meta's.
func TestSubagentsModTimePrefersJSONL(t *testing.T) {
	dir := t.TempDir()
	transcriptPath := filepath.Join(dir, "sess.jsonl")
	subagentsDir := filepath.Join(dir, "sess", "subagents")
	if err := os.MkdirAll(subagentsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	metaPath := filepath.Join(subagentsDir, "agent-x.meta.json")
	jsonlPath := filepath.Join(subagentsDir, "agent-x.jsonl")
	writeFile(t, metaPath, []string{`{"agentType":"Explore"}`})
	writeFile(t, jsonlPath, []string{subagentTerminalLine("2026-06-01T21:39:00Z")})

	// Distinct mtimes an hour apart: the meta older, the jsonl newer.
	metaTime := time.Date(2026, 6, 1, 20, 0, 0, 0, time.UTC)
	jsonlTime := time.Date(2026, 6, 1, 21, 0, 0, 0, time.UTC)
	if err := os.Chtimes(metaPath, metaTime, metaTime); err != nil {
		t.Fatalf("chtimes meta: %v", err)
	}
	if err := os.Chtimes(jsonlPath, jsonlTime, jsonlTime); err != nil {
		t.Fatalf("chtimes jsonl: %v", err)
	}

	subs, err := SubagentsForTranscript(transcriptPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("got %d subagents, want 1: %+v", len(subs), subs)
	}
	if !subs[0].ModTime.Equal(jsonlTime) {
		t.Errorf("ModTime = %v, want the jsonl mtime %v (not the meta's %v)", subs[0].ModTime, jsonlTime, metaTime)
	}
}

// TestSubagentPath locks the writer→transcript resolution the permission path
// depends on: a subagent's own entries live in the sibling
// <session>/subagents/agent-<id>.jsonl and never in the parent transcript, while
// an empty agent id (the hook's main-thread marker) must resolve to the parent
// transcript itself so callers stay branch-free.
func TestSubagentPath(t *testing.T) {
	t.Run("should return the main transcript unchanged when the agent id is empty", func(t *testing.T) {
		// The hook omits agent_id on the main thread, so "" means "the writer is the
		// main thread" — whose entries ARE the parent transcript.
		main := "/home/u/.claude/projects/-home-u-Projects-switchboard/sess-uuid.jsonl"
		if got := SubagentPath(main, ""); got != main {
			t.Errorf("SubagentPath(main, \"\") = %q, want the main transcript %q", got, main)
		}
	})

	t.Run("should resolve the sibling subagents jsonl when the agent id names a teammate", func(t *testing.T) {
		// The real on-disk shape: <projects>/<slug>/<session-uuid>.jsonl beside
		// <projects>/<slug>/<session-uuid>/subagents/agent-<id>.jsonl.
		main := "/home/u/.claude/projects/-home-u-Tools-DigestDownloads/5318eb5b-79df-4dee-a9f8-c80df4eca79e.jsonl"
		want := "/home/u/.claude/projects/-home-u-Tools-DigestDownloads/5318eb5b-79df-4dee-a9f8-c80df4eca79e/subagents/agent-af5bd126402ac16c7.jsonl"
		if got := SubagentPath(main, "af5bd126402ac16c7"); got != want {
			t.Errorf("SubagentPath = %q, want %q", got, want)
		}
	})

	t.Run("should resolve from the stored transcript alone when the session runs in a git worktree", func(t *testing.T) {
		// G10: derive from the passed path, never from cwd or a project slug. A
		// worktree session's cwd is <repo>/.claude/worktrees/agent-<id>, unrelated to
		// where its records live — so chdir somewhere else entirely and the answer
		// must not move.
		root := t.TempDir()
		slug := "-home-u-Projects-switchboard--claude-worktrees-agent-ad471f23b56b4c180"
		main := filepath.Join(root, "projects", slug, "d053f268-983c-4dfe-9e61-c9cf3ad55ce1.jsonl")
		subagentsDir := filepath.Join(root, "projects", slug, "d053f268-983c-4dfe-9e61-c9cf3ad55ce1", "subagents")
		if err := os.MkdirAll(subagentsDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		want := filepath.Join(subagentsDir, "agent-ad471f23b56b4c180.jsonl")
		writeFile(t, want, []string{subagentWorkingLine("2026-08-05T12:38:21Z")})

		t.Chdir(t.TempDir()) // an unrelated cwd: the derivation must ignore it
		got := SubagentPath(main, "ad471f23b56b4c180")
		if got != want {
			t.Errorf("SubagentPath = %q, want %q", got, want)
		}
		if _, err := os.Stat(got); err != nil {
			t.Errorf("resolved path is not the file on disk: %v", err)
		}
	})

	t.Run("should tolerate a trailing .jsonl when the agent id is spelled as its filename", func(t *testing.T) {
		// An id and its transcript filename are interchangeable at call sites; ".jsonl"
		// is a suffix no bare id carries, so trimming it cannot destroy information.
		main := "/p/sess.jsonl"
		want := "/p/sess/subagents/agent-a1b2.jsonl"
		if got := SubagentPath(main, "a1b2"); got != want {
			t.Errorf("bare id: SubagentPath = %q, want %q", got, want)
		}
		if got := SubagentPath(main, "a1b2.jsonl"); got != want {
			t.Errorf("suffixed id: SubagentPath = %q, want %q (suffix must not be doubled)", got, want)
		}
	})

	t.Run("should derive a distinct non-existent path when the agent id still carries the agent- prefix", func(t *testing.T) {
		// The inverse of the old contract, and deliberately so. agentID is BARE by
		// contract; normalization happens once, at the RPC boundary. A second strip
		// here would be indistinguishable from a legitimate id that begins with
		// "agent-", so a still-prefixed id is treated as the literal id it claims to
		// be. It derives a path that does not exist, the read fails, and the caller
		// keeps its pending entry — fail-safe, and never another agent's transcript.
		main := "/p/sess.jsonl"
		bare := SubagentPath(main, "a1b2")
		prefixed := SubagentPath(main, "agent-a1b2")
		if want := "/p/sess/subagents/agent-agent-a1b2.jsonl"; prefixed != want {
			t.Errorf("prefixed id: SubagentPath = %q, want %q (the prefix is part of the id, not decoration)", prefixed, want)
		}
		if prefixed == bare {
			t.Errorf("a prefixed id collapsed onto the bare id's path %q; over-stripping would let one agent resolve to another's transcript", bare)
		}
	})

	t.Run("should resolve two agents whose ids differ only by an agent- prefix to two different files", func(t *testing.T) {
		// The concrete missed-RED hazard. Named subagents get ids shaped a<name><hex>
		// and the name is user-supplied, so a subagent named "gent-foo" yields the id
		// "agent-foo-7152e6a858d30551" — which can coexist with an unrelated agent
		// whose id is "foo-7152e6a858d30551". Stripping in SubagentPath would alias the
		// former onto the latter, and a resolver reading the wrong agent's activity
		// would clear a prompt that is still pending.
		dir := t.TempDir()
		main := filepath.Join(dir, "sess.jsonl")
		subagentsDir := filepath.Join(dir, "sess", "subagents")
		if err := os.MkdirAll(subagentsDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		victim := "foo-7152e6a858d30551"      // an unrelated agent, sitting in the strip's shadow
		named := "agent-foo-7152e6a858d30551" // the "gent-foo" teammate's real, bare id
		for _, id := range []string{victim, named} {
			writeFile(t, filepath.Join(subagentsDir, "agent-"+id+".jsonl"), []string{subagentWorkingLine("2026-08-05T12:38:21Z")})
		}

		gotVictim := SubagentPath(main, victim)
		gotNamed := SubagentPath(main, named)
		if want := filepath.Join(subagentsDir, "agent-"+victim+".jsonl"); gotVictim != want {
			t.Errorf("unrelated agent: SubagentPath = %q, want %q", gotVictim, want)
		}
		if want := filepath.Join(subagentsDir, "agent-"+named+".jsonl"); gotNamed != want {
			t.Errorf("named teammate: SubagentPath = %q, want %q (its id legitimately begins with agent-)", gotNamed, want)
		}
		if gotNamed == gotVictim {
			t.Fatal("both ids resolved to one file: the named teammate would read another agent's activity and clear a still-pending prompt")
		}
		for _, p := range []string{gotVictim, gotNamed} {
			if _, err := os.Stat(p); err != nil {
				t.Errorf("resolved path is not the file on disk: %v", err)
			}
		}
	})

	t.Run("should treat the whole path as the session stem when the transcript lacks the .jsonl suffix", func(t *testing.T) {
		// TrimSuffix is a no-op; the result stays inside a plausible sibling dir
		// rather than becoming a path outside the session tree.
		if got, want := SubagentPath("/p/sess", "a1b2"), "/p/sess/subagents/agent-a1b2.jsonl"; got != want {
			t.Errorf("SubagentPath = %q, want %q", got, want)
		}
	})

	t.Run("should return empty when there is nothing sane to derive", func(t *testing.T) {
		// "agent-" is deliberately NOT here any more: with no TrimPrefix it is an id
		// like any other, deriving the non-existent agent-agent-.jsonl rather than "".
		// Only an id that trims to nothing, or one that could escape the dir, is empty.
		cases := []struct{ name, main, agent string }{
			{"empty transcript with a teammate id", "", "a1b2"},
			{"id that is only the .jsonl suffix", "/p/sess.jsonl", ".jsonl"},
			{"id that would escape the subagents dir", "/p/sess.jsonl", "../../../etc/passwd"},
		}
		for _, c := range cases {
			if got := SubagentPath(c.main, c.agent); got != "" {
				t.Errorf("%s: SubagentPath = %q, want \"\"", c.name, got)
			}
		}
	})
}

// TestSubagentPathAgreesWithSubagentsForTranscript is the anti-drift test: both
// callers share one derivation, so every agent the dir scan reports must be
// resolvable by SubagentPath from the same transcript path — including a
// named-teammate id and an orphan jsonl with no meta.
//
// It is also what pins the two remaining "agent-" strips as NON-symmetric. The scan
// strips the prefix off a FILENAME to recover the bare id; SubagentPath puts it back
// and strips nothing. Round-tripping an id that itself begins with "agent-" only
// closes if exactly one of the two strips exists — a second strip in SubagentPath
// breaks this test.
func TestSubagentPathAgreesWithSubagentsForTranscript(t *testing.T) {
	dir := t.TempDir()
	transcriptPath := filepath.Join(dir, "d053f268-983c-4dfe-9e61-c9cf3ad55ce1.jsonl")
	subagentsDir := filepath.Join(dir, "d053f268-983c-4dfe-9e61-c9cf3ad55ce1", "subagents")
	if err := os.MkdirAll(subagentsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Four id shapes: a hex id with meta+jsonl, a named in-process teammate, an
	// orphan jsonl with no meta, and a named teammate whose own bare id begins with
	// "agent-" (the "gent-foo" case) — stored as agent-agent-foo-<hex>.jsonl, and the
	// one that only round-trips because SubagentPath no longer strips.
	ids := []string{"ad471f23b56b4c180", "aauth-tests-7152e6a858d30551", "aorphan9c0", "agent-foo-7152e6a858d30551"}
	writeFile(t, filepath.Join(subagentsDir, "agent-"+ids[0]+".meta.json"), []string{`{"agentType":"general-purpose","spawnDepth":1}`})
	for _, id := range ids {
		writeFile(t, filepath.Join(subagentsDir, "agent-"+id+".jsonl"), []string{subagentTerminalLine("2026-08-05T12:38:21Z")})
	}

	subs, err := SubagentsForTranscript(transcriptPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(subs) != len(ids) {
		t.Fatalf("got %d subagents, want %d: %+v", len(subs), len(ids), subs)
	}
	for _, s := range subs {
		got := SubagentPath(transcriptPath, s.AgentID)
		want := filepath.Join(subagentsDir, "agent-"+s.AgentID+".jsonl")
		if got != want {
			t.Errorf("agent %q: SubagentPath = %q, want %q", s.AgentID, got, want)
		}
		if _, err := os.Stat(got); err != nil {
			t.Errorf("agent %q: SubagentPath does not point at the scanned file: %v", s.AgentID, err)
		}
	}
}

// --- naming a blocked writer ---------------------------------------------
//
// A red chip that names the blocked teammate has to get that name from
// somewhere, and the only place it exists is the spawn's meta.json. These pin
// the single-file path derivation and the fallback chain a renderer resolves
// through, without paying SubagentsForTranscript's whole-directory scan.

func TestSubagentMetaPath(t *testing.T) {
	const parent = "/home/u/.claude/projects/-home-u-proj/sess.jsonl"
	const dir = "/home/u/.claude/projects/-home-u-proj/sess/subagents"

	t.Run("should resolve the sibling meta file when the agent id names a teammate", func(t *testing.T) {
		want := dir + "/agent-af5bd126402ac16c7.meta.json"
		if got := SubagentMetaPath(parent, "af5bd126402ac16c7"); got != want {
			t.Errorf("SubagentMetaPath = %q, want %q", got, want)
		}
	})

	t.Run("should return nothing when the writer is the main thread", func(t *testing.T) {
		// Unlike SubagentPath, which hands the main thread its parent transcript,
		// there is no meta for a thread that was never spawned. "" is the caller's
		// discriminator, not an error.
		if got := SubagentMetaPath(parent, ""); got != "" {
			t.Errorf("SubagentMetaPath(main) = %q, want \"\"", got)
		}
	})

	t.Run("should tolerate a trailing .jsonl so an id and its filename are interchangeable", func(t *testing.T) {
		want := dir + "/agent-a1b2.meta.json"
		if got := SubagentMetaPath(parent, "a1b2.jsonl"); got != want {
			t.Errorf("SubagentMetaPath = %q, want %q", got, want)
		}
	})

	t.Run("should not strip a second agent- prefix when the id legitimately carries one", func(t *testing.T) {
		// The missed-RED hazard SubagentPath documents, in the meta namespace: an
		// agent named "gent-foo" has id "agent-foo-<hex>" and lives in
		// agent-agent-foo-<hex>.meta.json. Stripping would name a DIFFERENT agent.
		want := dir + "/agent-agent-foo-9f.meta.json"
		if got := SubagentMetaPath(parent, "agent-foo-9f"); got != want {
			t.Errorf("SubagentMetaPath = %q, want %q", got, want)
		}
	})

	t.Run("should return nothing when the id could escape the subagents dir", func(t *testing.T) {
		for _, id := range []string{"../../etc/passwd", "a/b", ".jsonl"} {
			if got := SubagentMetaPath(parent, id); got != "" {
				t.Errorf("SubagentMetaPath(%q) = %q, want \"\" (must not escape the dir)", id, got)
			}
		}
	})

	t.Run("should return nothing when there is no transcript to derive from", func(t *testing.T) {
		if got := SubagentMetaPath("", "a1b2"); got != "" {
			t.Errorf("SubagentMetaPath = %q, want \"\"", got)
		}
	})

	t.Run("should point at the same spawn as SubagentPath", func(t *testing.T) {
		// The two exported spellings share one derivation; this is the tripwire if
		// they ever stop agreeing on which spawn they mean.
		jsonl := SubagentPath(parent, "a1b2")
		meta := SubagentMetaPath(parent, "a1b2")
		if strings.TrimSuffix(jsonl, ".jsonl") != strings.TrimSuffix(meta, ".meta.json") {
			t.Errorf("SubagentPath %q and SubagentMetaPath %q name different spawns", jsonl, meta)
		}
	})
}

func TestSubagentDisplayName(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		writeFile(t, path, []string{body})
		return path
	}

	t.Run("should prefer the teammate name when the meta carries one", func(t *testing.T) {
		p := write("full.meta.json", `{"agentType":"general-purpose","name":"escalate-cleanup","description":"Clean up escalation duplication","taskKind":"in_process_teammate"}`)
		if got := SubagentDisplayName(p); got != "escalate-cleanup" {
			t.Errorf("SubagentDisplayName = %q, want escalate-cleanup", got)
		}
	})

	t.Run("should fall back to the agent type when the meta is the minimal shape", func(t *testing.T) {
		// Only agentType is present in EVERY meta; a Task-spawned agent has no name.
		p := write("minimal.meta.json", `{"agentType":"Explore"}`)
		if got := SubagentDisplayName(p); got != "Explore" {
			t.Errorf("SubagentDisplayName = %q, want Explore", got)
		}
	})

	t.Run("should trim surrounding whitespace from the name it reports", func(t *testing.T) {
		p := write("padded.meta.json", `{"agentType":"Explore","name":"  spaced-out  "}`)
		if got := SubagentDisplayName(p); got != "spaced-out" {
			t.Errorf("SubagentDisplayName = %q, want spaced-out", got)
		}
	})

	t.Run("should report nothing rather than invent a name when the meta is unusable", func(t *testing.T) {
		cases := map[string]string{
			"missing":  filepath.Join(dir, "nope.meta.json"),
			"empty id": "",
			"non-JSON": write("junk.meta.json", `not json at all`),
			"nameless": write("bare.meta.json", `{"toolUseId":"toolu_x"}`),
			"blank":    write("blank.meta.json", `{"agentType":"","name":"   "}`),
		}
		for what, p := range cases {
			if got := SubagentDisplayName(p); got != "" {
				t.Errorf("%s: SubagentDisplayName = %q, want \"\"", what, got)
			}
		}
	})
}

func TestSubagentsForTranscriptShouldReportTheTeammateName(t *testing.T) {
	dir := t.TempDir()
	transcriptPath := filepath.Join(dir, "sess.jsonl")
	subagentsDir := filepath.Join(dir, "sess", "subagents")
	if err := os.MkdirAll(subagentsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(subagentsDir, "agent-named.meta.json"),
		[]string{`{"agentType":"general-purpose","name":"escalate-cleanup","description":"Clean up escalation duplication"}`})
	writeFile(t, filepath.Join(subagentsDir, "agent-anon.meta.json"),
		[]string{`{"agentType":"Explore"}`})

	subs, err := SubagentsForTranscript(transcriptPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	byID := map[string]Subagent{}
	for _, s := range subs {
		byID[s.AgentID] = s
	}
	if got := byID["named"].Name; got != "escalate-cleanup" {
		t.Errorf("named agent: Name = %q, want escalate-cleanup", got)
	}
	if got := byID["anon"].Name; got != "" {
		t.Errorf("minimal meta should leave Name empty, got %q", got)
	}
}

// TestTasksSinceShouldFlagLaunchAcksWhenTheResultOnlyAcknowledgesTheSpawn pins the
// classification the fanout Observer depends on. Both ack wordings are live in the
// corpus, and either one being read as a completion force-closes a subagent
// seconds after it starts.
func TestTasksSinceShouldFlagLaunchAcksWhenTheResultOnlyAcknowledgesTheSpawn(t *testing.T) {
	path := writeTranscript(t,
		taskUse("2026-06-01T21:39:00Z", "toolu_new"),
		taskUse("2026-06-01T21:39:01Z", "toolu_old"),
		taskUse("2026-06-01T21:39:02Z", "toolu_real"),
		taskUse("2026-06-01T21:39:03Z", "toolu_str"),
		launchAckResult("2026-06-01T21:39:04Z", "toolu_new", "Async agent launched successfully"),
		launchAckResult("2026-06-01T21:39:05Z", "toolu_old", "Spawned successfully"),
		taskResult("2026-06-01T21:39:06Z", "toolu_real"),
		stringResult("2026-06-01T21:39:07Z", "toolu_str", "Spawned successfully. (This tool result is internal metadata.)"),
	)

	_, results, _, err := TasksSince(path, 0)
	if err != nil {
		t.Fatalf("TasksSince: %v", err)
	}
	for _, tc := range []struct {
		id      string
		wantAck bool
		what    string
	}{
		{"toolu_new", true, "the current async-launch wording"},
		{"toolu_old", true, "the older spawned-successfully wording"},
		{"toolu_str", true, "an ack whose content is a bare string, not a block array"},
		{"toolu_real", false, "a genuine result carrying no ack text"},
	} {
		got, ok := resultByID(results, tc.id)
		if !ok {
			t.Errorf("%s: %s missing from results", tc.id, tc.what)
			continue
		}
		if got.LaunchAck != tc.wantAck {
			t.Errorf("%s (%s): LaunchAck = %v, want %v", tc.id, tc.what, got.LaunchAck, tc.wantAck)
		}
	}
}

// TestTasksShouldNotMarkDoneWhenTheResultIsOnlyALaunchAck covers the legacy
// tail-bounded reader by the same rule, so a caller that reaches for it does not
// inherit the bug the Observer was fixed for.
func TestTasksShouldNotMarkDoneWhenTheResultIsOnlyALaunchAck(t *testing.T) {
	path := writeTranscript(t,
		taskUse("2026-06-01T21:39:00Z", "toolu_ack"),
		taskUse("2026-06-01T21:39:01Z", "toolu_done"),
		launchAckResult("2026-06-01T21:39:02Z", "toolu_ack", "Async agent launched successfully"),
		taskResult("2026-06-01T21:39:03Z", "toolu_done"),
	)

	tasks, err := Tasks(path, 128*1024)
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	acked, ok := taskByID(tasks, "toolu_ack")
	if !ok {
		t.Fatal("acked spawn missing from Tasks")
	}
	if acked.Done {
		t.Error("a spawn answered only by a launch ack must not read as Done")
	}
	finished, ok := taskByID(tasks, "toolu_done")
	if !ok {
		t.Fatal("finished spawn missing from Tasks")
	}
	if !finished.Done {
		t.Error("a spawn answered by a real tool_result must read as Done")
	}

	n, err := InFlightTasks(path, 128*1024)
	if err != nil {
		t.Fatalf("InFlightTasks: %v", err)
	}
	if n != 1 {
		t.Errorf("InFlightTasks = %d, want 1 (the acked spawn still running)", n)
	}
}
