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
	if len(results2) != 1 || results2[0] != "toolu_early" {
		t.Errorf("second read results = %v, want [toolu_early]", results2)
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
	if len(results2) != 1 || results2[0] != "toolu_a" {
		t.Errorf("second read results = %v, want [toolu_a]", results2)
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
