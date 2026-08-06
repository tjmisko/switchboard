package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/barlayout"
	sblabel "github.com/tjmisko/switchboard/internal/label"
	"github.com/tjmisko/switchboard/internal/projectname"
	"github.com/tjmisko/switchboard/internal/state"
)

// Test chips render on a bar wide enough that no abbreviation kicks in.
var (
	testAvail   = 100000.0
	testMetrics = barlayout.DefaultMetrics()
)

func TestSessionTooltipShowsStatusDuration(t *testing.T) {
	now := time.Date(2026, 6, 26, 14, 30, 0, 0, time.UTC)
	since := now.Add(-45 * time.Second)
	s := state.Session{
		PID: 4821, CWD: "/home/u/proj",
		Claude: &state.ClaudeInfo{Status: "permission", StatusSinceWire: &since},
	}
	tip := sessionTooltip(projectname.Config{}, nil, nil, s, now)
	if !strings.Contains(tip, "permission · 45s") {
		t.Errorf("tooltip should show the permission-wait duration:\n%s", tip)
	}
}

func TestSessionTooltipSuspendedShowsNoDuration(t *testing.T) {
	now := time.Date(2026, 6, 26, 14, 30, 0, 0, time.UTC)
	since := now.Add(-5 * time.Minute)
	s := state.Session{
		PID: 4821, CWD: "/home/u/proj", Suspended: true,
		Claude: &state.ClaudeInfo{Status: "working", StatusSinceWire: &since},
	}
	tip := sessionTooltip(projectname.Config{}, nil, nil, s, now)
	// Suspended status (and its clock) is stale; show "suspended", not a counter.
	if strings.Contains(tip, "5m") {
		t.Errorf("suspended session should not show a stale duration:\n%s", tip)
	}
	if !strings.Contains(tip, "suspended") {
		t.Errorf("suspended session should be labeled suspended:\n%s", tip)
	}
}

func TestRenderSlotStatusAndFlags(t *testing.T) {
	snap := state.Snapshot{
		Sessions: []state.Session{
			{PID: 4821, CWD: "/home/u/proj", Focused: true, Claude: &state.ClaudeInfo{Status: "working"}},
		},
	}
	out := renderSlot(snap, 0, testAvail, testMetrics, &nameConfig{}, &sblabel.NameCache{})
	if !slices.Contains(out.Class, "working") {
		t.Errorf("class missing status 'working': %v", out.Class)
	}
	if !slices.Contains(out.Class, "focused") {
		t.Errorf("class missing 'focused': %v", out.Class)
	}
	if slices.Contains(out.Class, "suspended") {
		t.Errorf("non-suspended session should not carry 'suspended': %v", out.Class)
	}
}

func TestRenderSlotSuspended(t *testing.T) {
	snap := state.Snapshot{
		Sessions: []state.Session{
			{PID: 4821, CWD: "/home/u/proj", Suspended: true, Claude: &state.ClaudeInfo{Status: "working"}},
		},
	}
	out := renderSlot(snap, 0, testAvail, testMetrics, &nameConfig{}, &sblabel.NameCache{})
	if !slices.Contains(out.Class, "suspended") {
		t.Errorf("suspended session missing 'suspended' class: %v", out.Class)
	}
	// The underlying status class is still present so CSS can layer the two.
	if !slices.Contains(out.Class, "working") {
		t.Errorf("suspended chip dropped its status class: %v", out.Class)
	}
	if !strings.Contains(out.Tooltip, "suspended") {
		t.Errorf("tooltip should note suspension: %q", out.Tooltip)
	}
}

func TestRenderSlotEmpty(t *testing.T) {
	out := renderSlot(state.Snapshot{}, 0, testAvail, testMetrics, &nameConfig{}, &sblabel.NameCache{})
	if !slices.Contains(out.Class, "empty") {
		t.Errorf("out-of-range slot should be 'empty': %v", out.Class)
	}
}

// When the bar is crowded the chip text is abbreviated with an ellipsis, but
// the tooltip still carries the full, untruncated name.
func TestRenderSlotAbbreviatesWhenCrowded(t *testing.T) {
	// Hermetic: no real projectname config. XDG_CONFIG_HOME has to move too —
	// ConfigPath prefers it over $HOME, so setting HOME alone would still read
	// the developer's own projects.json when XDG_CONFIG_HOME is set.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	snap := state.Snapshot{Sessions: []state.Session{
		{PID: 1, CWD: "/home/u/aaaaaaaaaaaaaaaaaaaa", Claude: &state.ClaudeInfo{Status: "working"}},
		{PID: 2, CWD: "/home/u/bbbbbbbbbbbbbbbbbbbb", Claude: &state.ClaudeInfo{Status: "working"}},
	}}
	unit := barlayout.Metrics{CharPx: 1, ChipFixedPx: 0}

	if full := renderSlot(snap, 0, 100000, unit, &nameConfig{}, &sblabel.NameCache{}); strings.HasSuffix(full.Text, "…") {
		t.Errorf("a wide bar should not abbreviate: %q", full.Text)
	}

	narrow := renderSlot(snap, 0, 10, unit, &nameConfig{}, &sblabel.NameCache{})
	if !strings.HasSuffix(narrow.Text, "…") {
		t.Errorf("a crowded bar should abbreviate with an ellipsis: %q", narrow.Text)
	}
	if !strings.Contains(narrow.Tooltip, "aaaaaaaa") {
		t.Errorf("tooltip should keep the full name, got %q", narrow.Tooltip)
	}
}

// A delegating chip (idle main thread, subagents in flight) renders GREEN: its
// primary class is "working" so existing CSS paints it green with no change, and
// a secondary "delegating" class rides along for an optional badge. The tooltip
// explains the green with the agent count.
func TestRenderSlotDelegating(t *testing.T) {
	snap := state.Snapshot{
		Sessions: []state.Session{
			{PID: 4821, CWD: "/home/u/proj", Claude: &state.ClaudeInfo{
				Status: state.StatusDelegating, InFlightSubagents: 2,
			}},
		},
	}
	out := renderSlot(snap, 0, testAvail, testMetrics, &nameConfig{}, &sblabel.NameCache{})
	if !slices.Contains(out.Class, "working") {
		t.Errorf("delegating chip must carry the green 'working' class: %v", out.Class)
	}
	if !slices.Contains(out.Class, "delegating") {
		t.Errorf("delegating chip missing its 'delegating' marker class: %v", out.Class)
	}
	if out.Alt != "working" {
		t.Errorf("Alt = %q, want working (green)", out.Alt)
	}
	if !strings.Contains(out.Tooltip, "delegating · 2 agents") {
		t.Errorf("tooltip should explain the green with the agent count: %q", out.Tooltip)
	}
}

// A delegating chip whose session is running an ultracode Workflow names the
// workflow and its progress instead of the bare agent count — the numbers the
// CLI's own "N/M agents done" line shows, so the bar and the pane agree.
func TestRenderSlotDelegatingShouldNameWorkflowAndProgressWhenRunActive(t *testing.T) {
	snap := state.Snapshot{
		Sessions: []state.Session{
			{PID: 4821, CWD: "/home/u/proj", Claude: &state.ClaudeInfo{
				Status: state.StatusDelegating, InFlightSubagents: 10,
				Workflows: []state.WorkflowStatus{{
					RunID: "wf_5e3cb808-2ac", Name: "simplification-audit",
					AgentsStarted: 17, AgentsDone: 7, InFlight: 10,
				}},
			}},
		},
	}
	out := renderSlot(snap, 0, testAvail, testMetrics, &nameConfig{}, &sblabel.NameCache{})
	if !strings.Contains(out.Tooltip, "workflow simplification-audit · 7/17 agents") {
		t.Errorf("tooltip should name the workflow and its progress: %q", out.Tooltip)
	}
}

// A run whose persisted script (and so its name) is missing still annotates
// with its opaque run id rather than falling back to the bare count.
func TestRenderSlotDelegatingShouldFallBackToRunIDWhenWorkflowNameUnknown(t *testing.T) {
	snap := state.Snapshot{
		Sessions: []state.Session{
			{PID: 4821, CWD: "/home/u/proj", Claude: &state.ClaudeInfo{
				Status: state.StatusDelegating, InFlightSubagents: 3,
				Workflows: []state.WorkflowStatus{
					{RunID: "wf_9f-1", AgentsStarted: 4, AgentsDone: 1, InFlight: 3},
					{RunID: "wf_9f-2", Name: "second", AgentsStarted: 2, AgentsDone: 2},
				},
			}},
		},
	}
	out := renderSlot(snap, 0, testAvail, testMetrics, &nameConfig{}, &sblabel.NameCache{})
	if !strings.Contains(out.Tooltip, "workflow wf_9f-1 · 1/4 agents (+1 more)") {
		t.Errorf("tooltip should show the run id and fold extra runs: %q", out.Tooltip)
	}
}

// --- naming the writer behind a red chip ----------------------------------

// blockedNow anchors the blocked-writer tooltip tests, so the "· 45s" the hover
// prints is pinned rather than racing the wall clock.
var blockedNow = time.Date(2026, 6, 26, 14, 30, 0, 0, time.UTC)

// blockedSession builds a red session with `writers` blocked (wire spelling;
// "main" = the main thread) alongside a real subagents/ dir carrying a teammate
// meta per entry of `names` (bare agent id -> teammate name).
func blockedSession(t *testing.T, writers []string, inflight int, names map[string]string) state.Session {
	t.Helper()
	dir := t.TempDir()
	transcriptPath := filepath.Join(dir, "sess.jsonl")
	subagentsDir := filepath.Join(dir, "sess", "subagents")
	if err := os.MkdirAll(subagentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for id, name := range names {
		body := fmt.Sprintf(`{"agentType":"general-purpose","name":%q,"taskKind":"in_process_teammate"}`, name)
		if err := os.WriteFile(filepath.Join(subagentsDir, "agent-"+id+".meta.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	since := blockedNow.Add(-45 * time.Second)
	return state.Session{
		PID: 4821, CWD: "/home/u/proj",
		Claude: &state.ClaudeInfo{
			Status:            state.StatusPermission,
			StatusSinceWire:   &since,
			Transcript:        transcriptPath,
			PendingWriters:    writers,
			InFlightSubagents: inflight,
		},
	}
}

// The incident, end to end: the chip read the SESSION's name while the actual
// state was "the escalate-cleanup teammate is waiting on approval". The hover now
// names the writer, so the red is a decision the user can route without switching
// to the pane first.
func TestSessionTooltipShouldNameTheBlockedTeammate(t *testing.T) {
	s := blockedSession(t, []string{"af5bd126402ac16c7"}, 4,
		map[string]string{"af5bd126402ac16c7": "escalate-cleanup"})
	tip := sessionTooltip(projectname.Config{}, &projectname.DirCache{}, &sblabel.NameCache{}, s, blockedNow)
	if !strings.Contains(tip, "permission · escalate-cleanup · 45s") {
		t.Errorf("tooltip should name the blocked teammate:\n%s", tip)
	}
}

func TestSessionTooltipShouldNameEveryWriterWhenTwoAreBlockedAtOnce(t *testing.T) {
	s := blockedSession(t, []string{"af5bd126402ac16c7", "main"}, 2,
		map[string]string{"af5bd126402ac16c7": "escalate-cleanup"})
	tip := sessionTooltip(projectname.Config{}, &projectname.DirCache{}, &sblabel.NameCache{}, s, blockedNow)
	if !strings.Contains(tip, "permission · escalate-cleanup, main · 45s") {
		t.Errorf("tooltip should name both blocked writers:\n%s", tip)
	}
}

// The solo case: one session, no teammates, main thread blocked. "main" restates
// what the red already means, so the hover stays exactly as it was.
func TestSessionTooltipShouldLeaveASoloPermissionUnannotated(t *testing.T) {
	s := blockedSession(t, []string{"main"}, 0, nil)
	tip := sessionTooltip(projectname.Config{}, &projectname.DirCache{}, &sblabel.NameCache{}, s, blockedNow)
	if !strings.Contains(tip, "permission · 45s") {
		t.Errorf("solo permission tooltip should be status + duration only:\n%s", tip)
	}
	if strings.Contains(tip, "main") {
		t.Errorf("solo permission tooltip should not name the main thread:\n%s", tip)
	}
}

// Bar real estate is fitted as a SET (barlayout.Fit): a chip label that grew when
// a prompt appeared would re-abbreviate every other chip on the row, twice per
// prompt, and would break the stable chip identity the user navigates by. The
// writer's name belongs in the hover only.
func TestRenderSlotShouldNotPutTheBlockedWriterInTheChipLabel(t *testing.T) {
	blocked := blockedSession(t, []string{"af5bd126402ac16c7"}, 4,
		map[string]string{"af5bd126402ac16c7": "escalate-cleanup"})
	quiet := state.Session{
		PID: 4821, CWD: "/home/u/proj",
		Claude: &state.ClaudeInfo{Status: state.StatusWorking},
	}

	red := renderSlot(state.Snapshot{Sessions: []state.Session{blocked}}, 0, testAvail, testMetrics, &nameConfig{}, &sblabel.NameCache{})
	green := renderSlot(state.Snapshot{Sessions: []state.Session{quiet}}, 0, testAvail, testMetrics, &nameConfig{}, &sblabel.NameCache{})
	if red.Text != green.Text {
		t.Errorf("chip text changed when a prompt appeared: %q -> %q", green.Text, red.Text)
	}
	if strings.Contains(red.Text, "escalate-cleanup") {
		t.Errorf("chip text must not carry the blocked writer: %q", red.Text)
	}
	if !strings.Contains(red.Tooltip, "escalate-cleanup") {
		t.Errorf("the writer's name should still reach the hover: %q", red.Tooltip)
	}
}

// --- project-name config cache -------------------------------------------

// newNameConfigFixture points ConfigPath at a temp dir and returns (configPath,
// projectRoot). The project root carries a .git so ProjectRoot resolves to it
// exactly, instead of walking up into whatever repo /tmp happens to sit under.
func newNameConfigFixture(t *testing.T) (string, string) {
	t.Helper()
	cfgHome := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", cfgHome)

	root := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(cfgHome, "switchboard", "projects.json")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatal(err)
	}
	return cfgPath, root
}

func writeAbbrev(t *testing.T, cfgPath, root, abbrev string) {
	t.Helper()
	body := fmt.Sprintf(`{"projects":{%q:{"canonical":%q,"aliases":[%q]}}}`, root, abbrev, abbrev)
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func snapshotIn(root string) state.Snapshot {
	return state.Snapshot{Sessions: []state.Session{
		{PID: 4821, CWD: root, Claude: &state.ClaudeInfo{Status: "working"}},
	}}
}

// The bar's middle-click rename (~/.config/scripts/claude-abbrev-edit) rewrites
// projects.json out from under a running chip and expects the next snapshot to
// show the new abbreviation. A load-once cache would silently break that flow;
// the mtime check is what keeps it working.
func TestNameConfigShouldReloadWhenConfigFileChanges(t *testing.T) {
	cfgPath, root := newNameConfigFixture(t)
	writeAbbrev(t, cfgPath, root, "aaa")

	names := &nameConfig{}
	labels := &sblabel.NameCache{}
	if got := renderSlot(snapshotIn(root), 0, testAvail, testMetrics, names, labels); got.Text != "aaa-proj" {
		t.Fatalf("chip text = %q, want aaa-proj", got.Text)
	}

	// Rewrite as the rename hook would, forcing a later mtime so the test does
	// not depend on the filesystem's timestamp granularity.
	writeAbbrev(t, cfgPath, root, "zzz")
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(cfgPath, future, future); err != nil {
		t.Fatal(err)
	}

	got := renderSlot(snapshotIn(root), 0, testAvail, testMetrics, names, labels)
	if got.Text != "zzz-proj" {
		t.Errorf("chip text = %q, want zzz-proj — the rewritten config was not picked up", got.Text)
	}
	if !strings.Contains(got.Tooltip, "<b>zzz</b>") {
		t.Errorf("tooltip kept the stale abbreviation: %q", got.Tooltip)
	}
}

// The cache must actually cache: with the file's stamp pinned, a changed body is
// deliberately NOT observed. Rewriting the content while restoring the original
// mtime and keeping the size identical is the only way to prove from the outside
// that the second render did not re-read the file.
func TestNameConfigShouldServeCachedConfigWhenFileStampUnchanged(t *testing.T) {
	cfgPath, root := newNameConfigFixture(t)
	writeAbbrev(t, cfgPath, root, "aaa")
	fi, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	names := &nameConfig{}
	labels := &sblabel.NameCache{}
	if got := renderSlot(snapshotIn(root), 0, testAvail, testMetrics, names, labels); got.Text != "aaa-proj" {
		t.Fatalf("chip text = %q, want aaa-proj", got.Text)
	}

	writeAbbrev(t, cfgPath, root, "bbb") // same length, so size is unchanged too
	if err := os.Chtimes(cfgPath, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}

	if got := renderSlot(snapshotIn(root), 0, testAvail, testMetrics, names, labels); got.Text != "aaa-proj" {
		t.Errorf("chip text = %q, want the cached aaa-proj — the config was re-read despite an unchanged stamp", got.Text)
	}
}

// A config file that does not exist yet is the common case (the user has never
// renamed a project). Its absence caches as the defaults, and the first rename
// still has to land.
func TestNameConfigShouldPickUpAConfigFileCreatedAfterFirstRender(t *testing.T) {
	cfgPath, root := newNameConfigFixture(t)

	names := &nameConfig{}
	labels := &sblabel.NameCache{}
	if got := renderSlot(snapshotIn(root), 0, testAvail, testMetrics, names, labels); got.Text != "proj" {
		t.Fatalf("chip text = %q, want the unprefixed proj (no user config)", got.Text)
	}

	writeAbbrev(t, cfgPath, root, "zzz")
	if got := renderSlot(snapshotIn(root), 0, testAvail, testMetrics, names, labels); got.Text != "zzz-proj" {
		t.Errorf("chip text = %q, want zzz-proj — a newly created config was not picked up", got.Text)
	}
}

// --- session-name cache ----------------------------------------------------

// writeSessionName drops ~/.claude/sessions/<pid>.json carrying name under the
// test's HOME, which is what `/name` writes and what label.RawName prefers over
// the window title. Returns the path so a test can rewrite it.
func writeSessionName(t *testing.T, pid int, name string) string {
	t.Helper()
	dir := filepath.Join(os.Getenv("HOME"), ".claude", "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, fmt.Sprintf("%d.json", pid))
	body := fmt.Sprintf(`{"pid":%d,"name":%q,"status":"busy"}`, pid, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// `/name bbb` rewrites the session file out from under a running chip, and the
// bar is expected to show the new name on the next snapshot. Caching the lookup
// must not cost that.
func TestRenderSlotShouldShowARenamedSessionOnTheNextSnapshot(t *testing.T) {
	_, root := newNameConfigFixture(t)
	path := writeSessionName(t, 4821, "aaa")

	names := &nameConfig{}
	labels := &sblabel.NameCache{}
	if got := renderSlot(snapshotIn(root), 0, testAvail, testMetrics, names, labels); got.Text != "proj-aaa" {
		t.Fatalf("chip text = %q, want proj-aaa", got.Text)
	}

	// The rename, with a forced later mtime so the test does not depend on the
	// filesystem's timestamp granularity. The new name is the same length as the
	// old, so the size is unchanged and mtime alone carries the invalidation.
	writeSessionName(t, 4821, "bbb")
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	got := renderSlot(snapshotIn(root), 0, testAvail, testMetrics, names, labels)
	if got.Text != "proj-bbb" {
		t.Errorf("chip text = %q, want proj-bbb — the rename did not reach the chip", got.Text)
	}
	if !strings.Contains(got.Tooltip, "bbb") {
		t.Errorf("tooltip kept the stale name: %q", got.Tooltip)
	}
}

// A session that exits and a new one that starts must not share a name just
// because renderSlot names them through one cache.
func TestRenderSlotShouldNameEachSessionFromItsOwnFile(t *testing.T) {
	_, root := newNameConfigFixture(t)
	writeSessionName(t, 4821, "first")
	writeSessionName(t, 4822, "second")
	snap := state.Snapshot{Sessions: []state.Session{
		{PID: 4821, CWD: root, Claude: &state.ClaudeInfo{Status: "working"}},
		{PID: 4822, CWD: root, Claude: &state.ClaudeInfo{Status: "working"}},
	}}

	names := &nameConfig{}
	labels := &sblabel.NameCache{}
	for pass := range 2 {
		if got := renderSlot(snap, 0, testAvail, testMetrics, names, labels); got.Text != "proj-first" {
			t.Errorf("pass %d: slot 0 text = %q, want proj-first", pass, got.Text)
		}
		if got := renderSlot(snap, 1, testAvail, testMetrics, names, labels); got.Text != "proj-second" {
			t.Errorf("pass %d: slot 1 text = %q, want proj-second", pass, got.Text)
		}
	}
}

// BenchmarkRenderSlotUncachedNames is the pre-change cost of one emission: every
// slot names EVERY session in the snapshot so the abbreviation agrees across
// chips, and each of those names was a read plus an unmarshal. Passing a nil
// cache is exactly the old behavior. BenchmarkRenderSlotCachedNames is what
// replaces it. Multiply either by the bar's ten slot processes for the real
// per-snapshot cost.
func BenchmarkRenderSlotUncachedNames(b *testing.B) {
	snap := benchSnapshot(b)
	names := &nameConfig{}
	names.config()
	b.ResetTimer()
	for b.Loop() {
		_ = renderSlot(snap, 0, testAvail, testMetrics, names, nil)
	}
}

func BenchmarkRenderSlotCachedNames(b *testing.B) {
	snap := benchSnapshot(b)
	names := &nameConfig{}
	names.config()
	labels := &sblabel.NameCache{}
	renderSlot(snap, 0, testAvail, testMetrics, names, labels) // prime
	b.ResetTimer()
	for b.Loop() {
		_ = renderSlot(snap, 0, testAvail, testMetrics, names, labels)
	}
}

// BenchmarkRenderSlotRealisticCwds is the emission cost with cwds that EXIST and
// carry a .git, which is what every live session on this machine looks like.
//
// It exists because benchSnapshot's cwd (/home/u/Projects/Arachne) does not
// exist, so ProjectRoot climbs to "/" — five stats — before giving up. A real
// session sits AT its repo root and resolves in one. Sizing the projectname cost
// off the missing-path benchmark overstates it by roughly 3x, and that number is
// the whole argument for caching the resolution.
func BenchmarkRenderSlotRealisticCwds(b *testing.B) {
	snap := benchSnapshotRealistic(b)
	names := &nameConfig{}
	names.config()
	labels := &sblabel.NameCache{}
	renderSlot(snap, 0, testAvail, testMetrics, names, labels) // prime
	b.ResetTimer()
	for b.Loop() {
		_ = renderSlot(snap, 0, testAvail, testMetrics, names, labels)
	}
}

// benchSnapshotRealistic mirrors benchSnapshot but gives each session a real
// repo root as its cwd, spread over several projects the way the live bar is.
func benchSnapshotRealistic(b *testing.B) state.Snapshot {
	b.Helper()
	home := b.TempDir()
	b.Setenv("HOME", home)
	b.Setenv("XDG_CONFIG_HOME", b.TempDir())
	dir := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		b.Fatal(err)
	}
	projects := b.TempDir()
	sessions := make([]state.Session, 13)
	for i := range sessions {
		pid := 7000 + i
		// Six distinct projects over thirteen sessions, the live shape: several
		// sessions share a repo, so a per-dir cache would see repeats.
		cwd := filepath.Join(projects, fmt.Sprintf("proj%d", i%6))
		if err := os.MkdirAll(filepath.Join(cwd, ".git"), 0o755); err != nil {
			b.Fatal(err)
		}
		body := fmt.Sprintf(`{"pid":%d,"sessionId":"b5c7fd65-5733-4ce2-a0fa-932b91d2c02%d","cwd":%q,"startedAt":1785950796170,"kind":"interactive","name":"assess-npm-vulnerabilities","nameSource":"derived","status":"busy","updatedAt":1785957054072}`, pid, i%10, cwd)
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.json", pid)), []byte(body), 0o644); err != nil {
			b.Fatal(err)
		}
		sessions[i] = state.Session{PID: pid, CWD: cwd, Claude: &state.ClaudeInfo{Status: "working"}}
	}
	return state.Snapshot{Sessions: sessions}
}

// benchSnapshot is a snapshot at this machine's usual live session count, each
// session named by a file on disk the way a real one is.
func benchSnapshot(b *testing.B) state.Snapshot {
	b.Helper()
	home := b.TempDir()
	b.Setenv("HOME", home)
	b.Setenv("XDG_CONFIG_HOME", b.TempDir())
	dir := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		b.Fatal(err)
	}
	sessions := make([]state.Session, 13)
	for i := range sessions {
		pid := 7000 + i
		body := fmt.Sprintf(`{"pid":%d,"sessionId":"b5c7fd65-5733-4ce2-a0fa-932b91d2c02%d","cwd":"/home/u/Projects/Arachne","startedAt":1785950796170,"kind":"interactive","name":"assess-npm-vulnerabilities","nameSource":"derived","status":"busy","updatedAt":1785957054072}`, pid, i%10)
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.json", pid)), []byte(body), 0o644); err != nil {
			b.Fatal(err)
		}
		sessions[i] = state.Session{PID: pid, CWD: "/home/u/Projects/Arachne", Claude: &state.ClaudeInfo{Status: "working"}}
	}
	return state.Snapshot{Sessions: sessions}
}

// --- emission dedupe -------------------------------------------------------

func TestEmitterShouldSuppressALineIdenticalToThePrevious(t *testing.T) {
	var buf bytes.Buffer
	e := &emitter{w: &buf}
	o := waybarOutput{Text: "sb-foo", Tooltip: "tip", Class: []string{"working"}}

	if !e.emit(o) {
		t.Fatal("the first emission must always be written")
	}
	if e.emit(o) {
		t.Error("an identical emission should be suppressed")
	}
	if got := strings.Count(buf.String(), "\n"); got != 1 {
		t.Errorf("wrote %d lines, want 1", got)
	}
}

func TestEmitterShouldWriteWhenAnyFieldChanges(t *testing.T) {
	var buf bytes.Buffer
	e := &emitter{w: &buf}
	e.emit(waybarOutput{Text: "sb-foo", Tooltip: "idle · 3m", Class: []string{"idle"}})

	// The tooltip's live duration counter alone is a real change: the bar shows
	// it, so it must reach the pipe.
	if !e.emit(waybarOutput{Text: "sb-foo", Tooltip: "idle · 4m", Class: []string{"idle"}}) {
		t.Error("a changed tooltip duration should be written")
	}
	if !e.emit(waybarOutput{Text: "sb-foo", Tooltip: "idle · 4m", Class: []string{"working"}}) {
		t.Error("a changed class should be written")
	}
	if got := strings.Count(buf.String(), "\n"); got != 3 {
		t.Errorf("wrote %d lines, want 3", got)
	}
}

// Entering the degraded state changes the bytes, so the dedupe lets it through
// on its own; staying down must not re-print it every retry.
func TestEmitterShouldWriteTheDegradedChipOnceWhenTheDaemonGoesDown(t *testing.T) {
	var buf bytes.Buffer
	e := &emitter{w: &buf}
	degraded := waybarOutput{Text: "✕", Tooltip: "switchboard not running", Class: []string{"tracker-down"}}

	e.emit(waybarOutput{Text: "sb-foo", Class: []string{"working"}})
	if !e.emit(degraded) {
		t.Error("the transition into degraded must be written")
	}
	if e.emit(degraded) || e.emit(degraded) {
		t.Error("a daemon that stays down should not re-print the degraded chip every retry")
	}
	if got := strings.Count(buf.String(), "\n"); got != 2 {
		t.Errorf("wrote %d lines, want 2", got)
	}
}

// After a reconnect our record of what the bar last read is untrustworthy —
// waybar may have reloaded the module. forget makes the next line unconditional
// so a chip cannot stick showing stale content across a daemon restart.
func TestEmitterShouldWriteAfterForgetEvenWhenTheLineRepeats(t *testing.T) {
	var buf bytes.Buffer
	e := &emitter{w: &buf}
	chip := waybarOutput{Text: "sb-foo", Class: []string{"working"}}

	e.emit(chip)
	if e.emit(chip) {
		t.Fatal("precondition: the repeat should be suppressed before forget")
	}
	e.forget()
	if !e.emit(chip) {
		t.Error("the first line after a reconnect must be written even if it repeats")
	}
	if got := strings.Count(buf.String(), "\n"); got != 2 {
		t.Errorf("wrote %d lines, want 2", got)
	}
}

// Aggregate mode shares the emitter, so it dedupes too.
func TestEmitterShouldSuppressRepeatsInAggregateMode(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var buf bytes.Buffer
	e := &emitter{w: &buf}
	names := &nameConfig{}
	labels := &sblabel.NameCache{}
	snap := state.Snapshot{Sessions: []state.Session{
		{PID: 4821, CWD: "/home/u/proj", Claude: &state.ClaudeInfo{Status: "working"}},
	}}

	if !e.emit(renderAggregate(snap, names, labels)) {
		t.Fatal("the first aggregate emission must be written")
	}
	if e.emit(renderAggregate(snap, names, labels)) {
		t.Error("an unchanged aggregate snapshot should be suppressed")
	}
}

// BenchmarkNameConfigUncached is the pre-change cost: one os.ReadFile plus a
// json.Unmarshal of projects.json per emission, paid by every slot process on
// every snapshot. BenchmarkNameConfigCached is what replaces it — one os.Stat.
func BenchmarkNameConfigUncached(b *testing.B) {
	cfgPath, root := benchFixture(b)
	writeAbbrevB(b, cfgPath, root)
	for b.Loop() {
		_ = projectname.Load()
	}
}

func BenchmarkNameConfigCached(b *testing.B) {
	cfgPath, root := benchFixture(b)
	writeAbbrevB(b, cfgPath, root)
	names := &nameConfig{}
	names.config() // prime
	for b.Loop() {
		_ = names.config()
	}
}

func benchFixture(b *testing.B) (string, string) {
	b.Helper()
	cfgHome := b.TempDir()
	b.Setenv("HOME", b.TempDir())
	b.Setenv("XDG_CONFIG_HOME", cfgHome)
	root := filepath.Join(b.TempDir(), "proj")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		b.Fatal(err)
	}
	cfgPath := filepath.Join(cfgHome, "switchboard", "projects.json")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		b.Fatal(err)
	}
	return cfgPath, root
}

// A realistic config: the user has renamed a handful of projects.
func writeAbbrevB(b *testing.B, cfgPath, root string) {
	b.Helper()
	entries := ""
	for i := range 8 {
		if i > 0 {
			entries += ","
		}
		entries += fmt.Sprintf(`%q:{"canonical":"p%d","aliases":["p%d"]}`, fmt.Sprintf("%s%d", root, i), i, i)
	}
	body := fmt.Sprintf(`{"projects":{%s}}`, entries)
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		b.Fatal(err)
	}
}

// --- width override --------------------------------------------------------

func TestResolveAvailPxShouldUseTheOverrideWhenTheBarSuppliesOne(t *testing.T) {
	if got := resolveAvailPx(1234.5); got != 1234.5 {
		t.Errorf("resolveAvailPx(1234.5) = %v, want 1234.5 (no hyprctl fork)", got)
	}
}

// Zero and negative both mean "auto-detect", so an unset flag keeps the bar
// working with no config change.
func TestResolveAvailPxShouldAutoDetectWhenTheOverrideIsUnset(t *testing.T) {
	want := barlayout.ScreenWidthPx()
	for _, in := range []float64{0, -1} {
		if got := resolveAvailPx(in); got != want {
			t.Errorf("resolveAvailPx(%v) = %v, want the auto-detected %v", in, got, want)
		}
	}
}

// The end-to-end rename path, exercised through the REAL writer rather than a
// hand-rolled os.WriteFile: ~/.config/scripts/claude-abbrev-edit shells out to
// `switchboard-ctl name set`, which is projectname.SetAbbrev -> upsertEntry ->
// temp file + os.Rename.
//
// Deliberately no os.Chtimes fudging here. SetAbbrev writes MarshalIndent output,
// so swapping one three-letter abbrev for another leaves the file size
// IDENTICAL — mtime is the only thing that can catch the change, and this test
// fails on any filesystem whose timestamp granularity is too coarse to separate
// the two writes. That is exactly the property worth knowing about.
func TestNameConfigShouldPickUpARenameWrittenBySetAbbrev(t *testing.T) {
	_, root := newNameConfigFixture(t)
	if err := projectname.SetAbbrev(root, "aaa"); err != nil {
		t.Fatal(err)
	}

	names := &nameConfig{}
	labels := &sblabel.NameCache{}
	if got := renderSlot(snapshotIn(root), 0, testAvail, testMetrics, names, labels); got.Text != "aaa-proj" {
		t.Fatalf("chip text = %q, want aaa-proj", got.Text)
	}

	// The middle-click rename, as the bar performs it.
	if err := projectname.SetAbbrev(root, "zzz"); err != nil {
		t.Fatal(err)
	}
	got := renderSlot(snapshotIn(root), 0, testAvail, testMetrics, names, labels)
	if got.Text != "zzz-proj" {
		t.Errorf("chip text = %q, want zzz-proj — the middle-click rename did not reach the chip", got.Text)
	}
	if !strings.Contains(got.Tooltip, "<b>zzz</b>") {
		t.Errorf("tooltip kept the stale abbreviation: %q", got.Tooltip)
	}
}
