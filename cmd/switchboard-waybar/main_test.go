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
	tip := sessionTooltip(projectname.Config{}, s, now)
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
	tip := sessionTooltip(projectname.Config{}, s, now)
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
	out := renderSlot(snap, 0, testAvail, testMetrics, &nameConfig{})
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
	out := renderSlot(snap, 0, testAvail, testMetrics, &nameConfig{})
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
	out := renderSlot(state.Snapshot{}, 0, testAvail, testMetrics, &nameConfig{})
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

	if full := renderSlot(snap, 0, 100000, unit, &nameConfig{}); strings.HasSuffix(full.Text, "…") {
		t.Errorf("a wide bar should not abbreviate: %q", full.Text)
	}

	narrow := renderSlot(snap, 0, 10, unit, &nameConfig{})
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
	out := renderSlot(snap, 0, testAvail, testMetrics, &nameConfig{})
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
	if got := renderSlot(snapshotIn(root), 0, testAvail, testMetrics, names); got.Text != "aaa-proj" {
		t.Fatalf("chip text = %q, want aaa-proj", got.Text)
	}

	// Rewrite as the rename hook would, forcing a later mtime so the test does
	// not depend on the filesystem's timestamp granularity.
	writeAbbrev(t, cfgPath, root, "zzz")
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(cfgPath, future, future); err != nil {
		t.Fatal(err)
	}

	got := renderSlot(snapshotIn(root), 0, testAvail, testMetrics, names)
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
	if got := renderSlot(snapshotIn(root), 0, testAvail, testMetrics, names); got.Text != "aaa-proj" {
		t.Fatalf("chip text = %q, want aaa-proj", got.Text)
	}

	writeAbbrev(t, cfgPath, root, "bbb") // same length, so size is unchanged too
	if err := os.Chtimes(cfgPath, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}

	if got := renderSlot(snapshotIn(root), 0, testAvail, testMetrics, names); got.Text != "aaa-proj" {
		t.Errorf("chip text = %q, want the cached aaa-proj — the config was re-read despite an unchanged stamp", got.Text)
	}
}

// A config file that does not exist yet is the common case (the user has never
// renamed a project). Its absence caches as the defaults, and the first rename
// still has to land.
func TestNameConfigShouldPickUpAConfigFileCreatedAfterFirstRender(t *testing.T) {
	cfgPath, root := newNameConfigFixture(t)

	names := &nameConfig{}
	if got := renderSlot(snapshotIn(root), 0, testAvail, testMetrics, names); got.Text != "proj" {
		t.Fatalf("chip text = %q, want the unprefixed proj (no user config)", got.Text)
	}

	writeAbbrev(t, cfgPath, root, "zzz")
	if got := renderSlot(snapshotIn(root), 0, testAvail, testMetrics, names); got.Text != "zzz-proj" {
		t.Errorf("chip text = %q, want zzz-proj — a newly created config was not picked up", got.Text)
	}
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
	snap := state.Snapshot{Sessions: []state.Session{
		{PID: 4821, CWD: "/home/u/proj", Claude: &state.ClaudeInfo{Status: "working"}},
	}}

	if !e.emit(renderAggregate(snap, names)) {
		t.Fatal("the first aggregate emission must be written")
	}
	if e.emit(renderAggregate(snap, names)) {
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
	if got := renderSlot(snapshotIn(root), 0, testAvail, testMetrics, names); got.Text != "aaa-proj" {
		t.Fatalf("chip text = %q, want aaa-proj", got.Text)
	}

	// The middle-click rename, as the bar performs it.
	if err := projectname.SetAbbrev(root, "zzz"); err != nil {
		t.Fatal(err)
	}
	got := renderSlot(snapshotIn(root), 0, testAvail, testMetrics, names)
	if got.Text != "zzz-proj" {
		t.Errorf("chip text = %q, want zzz-proj — the middle-click rename did not reach the chip", got.Text)
	}
	if !strings.Contains(got.Tooltip, "<b>zzz</b>") {
		t.Errorf("tooltip kept the stale abbreviation: %q", got.Tooltip)
	}
}
