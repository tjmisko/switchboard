package history

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The cursor is a cache of the SeedScan reduction, and this file holds it to
// the cache's one obligation: agreeing with the full replay it stands in for,
// on every path — clean load, tail replay, invalidation, retention pruning,
// and a torn final line.

func seedCursorFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeDay(t, dir, "2026-06-25",
		`{"ts":"2026-06-25T10:00:00Z","type":"subagent_spawn","session_id":"s1","agent_id":"a1"}`,
		`{"ts":"2026-06-25T10:05:00Z","type":"subagent_stop","session_id":"s1","agent_id":"a1"}`,
		`{"ts":"2026-06-25T11:00:00Z","type":"workflow_start","session_id":"s2","workflow_run_id":"wf_a"}`,
	)
	writeDay(t, dir, "2026-06-26",
		`{"ts": "2026-06-26T10:00:00Z", "type": "subagent_spawn", "session_id": "s1", "agent_id": "a2"}`, // python-spaced repair shape
		`{"ts":"2026-06-26T10:01:00Z","type":"transition","session_id":"s1","from":"idle","to":"working"}`,
	)
	return dir
}

func assertIndexesEqual(t *testing.T, got, want SeedIndex, context string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: index diverged from full replay.\n got: %+v\nwant: %+v", context, got, want)
	}
}

func TestSeedLoadCursorRoundTripMatchesFullScan(t *testing.T) {
	dir := seedCursorFixture(t)

	first, err := SeedLoad(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first.Source != "scan" {
		t.Fatalf("first load source = %q, want scan (no cursor exists yet)", first.Source)
	}
	if err := WriteSeedCursor(dir, first); err != nil {
		t.Fatal(err)
	}

	second, err := SeedLoad(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second.Source != "cursor" {
		t.Fatalf("second load source = %q, want cursor", second.Source)
	}
	fresh, _, err := SeedScan(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertIndexesEqual(t, second.Index, fresh, "cursor round-trip")
	if !reflect.DeepEqual(second.Offsets, first.Offsets) {
		t.Errorf("offsets moved with no writes: %v vs %v", second.Offsets, first.Offsets)
	}
}

func TestSeedLoadReplaysOnlyTheAppendedTail(t *testing.T) {
	dir := seedCursorFixture(t)
	first, _ := SeedLoad(dir)
	if err := WriteSeedCursor(dir, first); err != nil {
		t.Fatal(err)
	}

	// The daemon's later life: appends to the last day, plus a whole new day.
	appendLine(t, dir, "2026-06-26",
		`{"ts":"2026-06-26T18:00:00Z","type":"subagent_spawn","session_id":"s1","agent_id":"a3"}`)
	writeDay(t, dir, "2026-06-27",
		`{"ts":"2026-06-27T09:00:00Z","type":"workflow_stop","session_id":"s2","workflow_run_id":"wf_a"}`)

	res, err := SeedLoad(dir)
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != "cursor" {
		t.Fatalf("source = %q, want cursor — appends must not invalidate", res.Source)
	}
	// Only the tail was read: two files touched (the appended day + the new
	// day), and only the appended lines streamed.
	if res.Stats.Files != 2 || res.Stats.Lines != 2 {
		t.Errorf("stats = %+v, want files=2 lines=2 (the tail, not the store)", res.Stats)
	}
	fresh, _, err := SeedScan(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertIndexesEqual(t, res.Index, fresh, "tail replay")
	if !res.Index.Sets("s1").Spawned["a3"] || !res.Index.Sets("s2").WorkflowStopped["wf_a"] {
		t.Errorf("tail events missing: %+v", res.Index)
	}
}

func TestSeedLoadRebuildsWhenADayFileShrank(t *testing.T) {
	dir := seedCursorFixture(t)
	first, _ := SeedLoad(dir)
	if err := WriteSeedCursor(dir, first); err != nil {
		t.Fatal(err)
	}

	// A scrub/repair rewrote a day-file shorter than its consumed offset.
	writeDayFile(t, dir, "2026-06-25",
		`{"ts":"2026-06-25T10:00:00Z","type":"subagent_spawn","session_id":"s1","agent_id":"a1"}`+"\n")

	res, err := SeedLoad(dir)
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != "scan" {
		t.Fatalf("source = %q, want scan — a shrunken file must invalidate the cursor", res.Source)
	}
	fresh, _, err := SeedScan(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertIndexesEqual(t, res.Index, fresh, "post-shrink rebuild")
	if res.Index.Sets("s1").Stopped["a1"] {
		t.Error("the rewritten file no longer records a1's stop; a cursor answer would have kept it")
	}
}

func TestSeedLoadKeepsAPrunedFilesContribution(t *testing.T) {
	dir := seedCursorFixture(t)
	first, _ := SeedLoad(dir)
	if err := WriteSeedCursor(dir, first); err != nil {
		t.Fatal(err)
	}

	// Retention deleted the oldest day. Its events are gone from disk but were
	// recorded, and "recorded ever" is the question seeding answers — the
	// cursor's sets must keep them.
	if err := os.Remove(DayPath(dir, "2026-06-25")); err != nil {
		t.Fatal(err)
	}

	res, err := SeedLoad(dir)
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != "cursor" {
		t.Fatalf("source = %q, want cursor — a pruned file is not an invalidation", res.Source)
	}
	s1 := res.Index.Sets("s1")
	if !s1.Spawned["a1"] || !s1.Stopped["a1"] || !res.Index.Sets("s2").WorkflowStarted["wf_a"] {
		t.Errorf("pruned day's contribution lost: %+v", res.Index)
	}
	if _, exists := res.Offsets["2026-06-25"]; exists {
		t.Error("a pruned file must drop its offset entry, not carry a ghost")
	}
}

func TestSeedLoadNeverConsumesATornTail(t *testing.T) {
	dir := t.TempDir()
	writeDay(t, dir, "2026-06-26",
		`{"ts":"2026-06-26T10:00:00Z","type":"subagent_spawn","session_id":"s1","agent_id":"a1"}`)
	// A crash mid-append: half a line, no newline.
	torn := `{"ts":"2026-06-26T10:01:00Z","type":"subagent_spawn","session_id":"s1","agent_`
	appendRaw(t, dir, "2026-06-26", torn)

	first, err := SeedLoad(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteSeedCursor(dir, first); err != nil {
		t.Fatal(err)
	}
	if first.Index.Sets("s1").Spawned["a2"] {
		t.Fatal("the torn line half-parsed; the fixture is wrong")
	}

	// The writer completes the line later; the un-consumed offset re-reads it.
	appendRaw(t, dir, "2026-06-26", `id":"a2"}`+"\n")
	res, err := SeedLoad(dir)
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != "cursor" {
		t.Fatalf("source = %q, want cursor", res.Source)
	}
	if !res.Index.Sets("s1").Spawned["a2"] {
		t.Errorf("the completed line was never folded — the torn tail was wrongly consumed: %+v", res.Index.Sets("s1"))
	}
}

func TestSeedLoadRebuildsOnMalformedCursor(t *testing.T) {
	dir := seedCursorFixture(t)
	if err := os.WriteFile(filepath.Join(dir, seedCursorName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := SeedLoad(dir)
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != "scan" {
		t.Fatalf("source = %q, want scan on a malformed cursor", res.Source)
	}
	fresh, _, err := SeedScan(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertIndexesEqual(t, res.Index, fresh, "malformed-cursor rebuild")
}

// --- fixture plumbing ---

func appendLine(t *testing.T, dir, day, line string) {
	t.Helper()
	appendRaw(t, dir, day, line+"\n")
}

func appendRaw(t *testing.T, dir, day, raw string) {
	t.Helper()
	f, err := os.OpenFile(DayPath(dir, day), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(raw); err != nil {
		t.Fatal(err)
	}
}

func writeDayFile(t *testing.T, dir, day, body string) {
	t.Helper()
	if err := os.WriteFile(DayPath(dir, day), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
