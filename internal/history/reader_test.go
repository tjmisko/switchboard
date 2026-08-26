package history

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeDay(t *testing.T, dir, day string, lines ...string) {
	t.Helper()
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, day+".jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadDayToleratesTornLine(t *testing.T) {
	dir := t.TempDir()
	writeDay(t, dir, "2026-06-26",
		`{"ts":"2026-06-26T12:00:00Z","type":"transition","to":"working"}`,
		`{"ts":"2026-06-26T12:01:00Z","type":"transition","to":"idle"}`,
		`{"ts":"2026-06-26T12:02:00Z","type":"transi`, // torn final line (crash mid-append)
	)
	evs, err := ReadDay(dir, "2026-06-26")
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2 (torn line skipped)", len(evs))
	}
	if evs[1].To != "idle" {
		t.Errorf("event 1 = %+v, want to=idle", evs[1])
	}
}

// TestReadDaySkipsForeignJSONLine asserts a foreign-but-valid JSON line (e.g.
// {"foo":"bar"}) is dropped, not surfaced as a phantom zero-value Event. Unlike a
// torn line it parses cleanly, but `type` is guaranteed-present for a real event,
// so a type-less line is treated as foreign and skipped — matching the tolerance
// the transcript reader already applies to stray lines.
func TestReadDaySkipsForeignJSONLine(t *testing.T) {
	dir := t.TempDir()
	writeDay(t, dir, "2026-06-26",
		`{"ts":"2026-06-26T12:00:00Z","type":"transition","to":"working"}`,
		`{"foo":"bar"}`, // foreign-but-valid JSON — NOT a torn line
		`{"ts":"2026-06-26T12:02:00Z","type":"transition","to":"idle"}`,
	)
	evs, err := ReadDay(dir, "2026-06-26")
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2 (foreign line skipped): %+v", len(evs), evs)
	}
	if evs[0].To != "working" || evs[1].To != "idle" {
		t.Errorf("real events should pass through in order, got %+v", evs)
	}
}

func TestReadDayMissingFileIsEmpty(t *testing.T) {
	evs, err := ReadDay(t.TempDir(), "2026-01-01")
	if err != nil {
		t.Fatalf("missing day should not error: %v", err)
	}
	if len(evs) != 0 {
		t.Errorf("missing day should be empty, got %d", len(evs))
	}
}

func TestReadRangeSpansDaysAndFilters(t *testing.T) {
	dir := t.TempDir()
	writeDay(t, dir, "2026-06-25", `{"ts":"2026-06-25T23:30:00Z","type":"transition","to":"a"}`)
	writeDay(t, dir, "2026-06-26",
		`{"ts":"2026-06-26T08:00:00Z","type":"transition","to":"b"}`,
		`{"ts":"2026-06-26T20:00:00Z","type":"transition","to":"c"}`,
	)
	writeDay(t, dir, "2026-06-27", `{"ts":"2026-06-27T09:00:00Z","type":"transition","to":"d"}`)

	from := time.Date(2026, 6, 26, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 26, 18, 0, 0, 0, time.UTC)
	evs, err := ReadRange(dir, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].To != "b" {
		t.Fatalf("range [06-26 00:00, 18:00) = %+v, want only event b", evs)
	}
}

func TestDaysSortedOldestFirst(t *testing.T) {
	dir := t.TempDir()
	writeDay(t, dir, "2026-06-27", "{}")
	writeDay(t, dir, "2026-06-25", "{}")
	writeDay(t, dir, "2026-06-26", "{}")
	days, err := Days(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"2026-06-25", "2026-06-26", "2026-06-27"}
	if len(days) != 3 || days[0] != want[0] || days[2] != want[2] {
		t.Errorf("Days = %v, want %v", days, want)
	}
}
