package history

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// readDay reads and decodes every event in a day-file.
func readDay(t *testing.T, dir, day string) []Event {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, day+".jsonl"))
	if err != nil {
		t.Fatalf("open day %s: %v", day, err)
	}
	defer f.Close()
	var evs []Event
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("decode %q: %v", sc.Text(), err)
		}
		evs = append(evs, e)
	}
	return evs
}

func TestDisabledSinkWritesNothing(t *testing.T) {
	dir := t.TempDir()
	s := NewSink(Config{Enabled: false, Dir: dir})
	s.Record(Event{Ts: time.Now(), Type: EventTransition, To: "working"})
	s.Close()
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("disabled sink wrote %d files, want 0", len(entries))
	}
}

func TestSinkWritesEventsPartitionedByDay(t *testing.T) {
	dir := t.TempDir()
	s := NewSink(Config{Enabled: true, Detail: DetailFull, Dir: dir})

	day1 := time.Date(2026, 6, 26, 23, 59, 0, 0, time.UTC)
	day2 := day1.Add(2 * time.Minute) // crosses into 2026-06-27 UTC
	s.Record(Event{Ts: day1, Type: EventSessionStart, PID: 1, Agent: "claude"})
	s.Record(Event{Ts: day1, Type: EventTransition, PID: 1, From: "idle", To: "working"})
	s.Record(Event{Ts: day2, Type: EventTransition, PID: 1, From: "working", To: "idle"})
	s.Close()

	d1 := readDay(t, dir, "2026-06-26")
	if len(d1) != 2 {
		t.Fatalf("day1 events = %d, want 2", len(d1))
	}
	if d1[0].Type != EventSessionStart || d1[1].To != "working" {
		t.Errorf("day1 events out of order/shape: %+v", d1)
	}
	d2 := readDay(t, dir, "2026-06-27")
	if len(d2) != 1 || d2[0].To != "idle" {
		t.Errorf("day2 events = %+v, want one working->idle", d2)
	}
}

func TestMinimalTierScrubsSensitiveFields(t *testing.T) {
	dir := t.TempDir()
	s := NewSink(Config{
		Enabled: true, Detail: DetailMinimal, Dir: dir,
		ResolveProject: func(cwd string) string { return "sb" },
	})
	ts := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	s.Record(Event{
		Ts: ts, Type: EventTransition, PID: 1, From: "permission", To: "working",
		CWD: "/home/u/Projects/secret", Pending: "AskUserQuestion", Reason: "tool-name match",
	})
	s.Close()

	ev := readDay(t, dir, "2026-06-26")[0]
	if ev.CWD != "" || ev.Pending != "" || ev.Reason != "" {
		t.Errorf("minimal tier leaked sensitive fields: %+v", ev)
	}
	if ev.Project != "sb" {
		t.Errorf("project should survive scrub (resolved before cwd dropped): %q", ev.Project)
	}
	if ev.From != "permission" || ev.To != "working" {
		t.Errorf("minimal tier dropped non-sensitive transition fields: %+v", ev)
	}
}

func TestMinimalTierScrubsDescriptionButKeepsAgentTypeAndTokens(t *testing.T) {
	dir := t.TempDir()
	s := NewSink(Config{Enabled: true, Detail: DetailMinimal, Dir: dir})
	ts := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	s.Record(Event{
		Ts: ts, Type: EventSubagentSpawn, PID: 1,
		AgentType: "Explore", Description: "refactor the auth module",
		CWD: "/home/u/secret", Pending: "Bash", Reason: "tool-name match",
		TokIn: 120, TokOut: 34, TokCacheRead: 1000, TokCacheCreate: 5,
	})
	s.Close()

	ev := readDay(t, dir, "2026-06-26")[0]
	// The content-revealing fields are scrubbed at the minimal tier.
	if ev.Description != "" || ev.CWD != "" || ev.Pending != "" || ev.Reason != "" {
		t.Errorf("minimal tier leaked content fields: %+v", ev)
	}
	// The agent kind and token accounting are not content — they survive minimal.
	if ev.AgentType != "Explore" {
		t.Errorf("agent_type should survive minimal scrub, got %q", ev.AgentType)
	}
	if ev.TokIn != 120 || ev.TokOut != 34 || ev.TokCacheRead != 1000 || ev.TokCacheCreate != 5 {
		t.Errorf("token counts should survive minimal scrub, got %+v", ev)
	}
}

func TestFullTierKeepsSensitiveFields(t *testing.T) {
	dir := t.TempDir()
	s := NewSink(Config{Enabled: true, Detail: DetailFull, Dir: dir})
	ts := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	s.Record(Event{Ts: ts, Type: EventTransition, CWD: "/home/u/proj", Pending: "Bash"})
	s.Close()

	ev := readDay(t, dir, "2026-06-26")[0]
	if ev.CWD != "/home/u/proj" || ev.Pending != "Bash" {
		t.Errorf("full tier should keep cwd/pending: %+v", ev)
	}
}

func TestPruneByRetainDays(t *testing.T) {
	dir := t.TempDir()
	// Three day-files: 10 days old, 2 days old, today.
	for _, day := range []string{"2026-06-16", "2026-06-24", "2026-06-26"} {
		if err := os.WriteFile(filepath.Join(dir, day+".jsonl"), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	pruneDir(dir, 5, 0, now) // keep 5 days

	if _, err := os.Stat(filepath.Join(dir, "2026-06-16.jsonl")); !os.IsNotExist(err) {
		t.Errorf("10-day-old file should be pruned")
	}
	for _, keep := range []string{"2026-06-24", "2026-06-26"} {
		if _, err := os.Stat(filepath.Join(dir, keep+".jsonl")); err != nil {
			t.Errorf("file within retention pruned: %s", keep)
		}
	}
}

func TestPruneByMaxBytesKeepsNewestAndToday(t *testing.T) {
	dir := t.TempDir()
	big := make([]byte, 1000)
	for _, day := range []string{"2026-06-20", "2026-06-25", "2026-06-26"} {
		if err := os.WriteFile(filepath.Join(dir, day+".jsonl"), big, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	pruneDir(dir, 0, 1500, now) // only ~1.5 files fit

	if _, err := os.Stat(filepath.Join(dir, "2026-06-20.jsonl")); !os.IsNotExist(err) {
		t.Errorf("oldest file should be trimmed under the byte cap")
	}
	if _, err := os.Stat(filepath.Join(dir, "2026-06-26.jsonl")); err != nil {
		t.Errorf("today's file must never be trimmed: %v", err)
	}
}

func TestPruneByMaxBytesRemovesOldestUntilUnderCap(t *testing.T) {
	dir := t.TempDir()
	payload := make([]byte, 1000)
	// Three NON-today files of ~1000B each; total 3000B.
	for _, day := range []string{"2026-06-20", "2026-06-25", "2026-06-28"} {
		if err := os.WriteFile(filepath.Join(dir, day+".jsonl"), payload, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC) // none of the files is "today"
	pruneDir(dir, 0, 2500, now)

	// 3000 > 2500 → drop the oldest (06-20) → 2000 ≤ 2500 → stop. The middle file
	// (06-25) is the boundary: it is KEPT even though it is old, because the cap is
	// already satisfied once the single oldest file is gone.
	if _, err := os.Stat(filepath.Join(dir, "2026-06-20.jsonl")); !os.IsNotExist(err) {
		t.Errorf("oldest (06-20) should be removed to get under the cap")
	}
	for _, keep := range []string{"2026-06-25", "2026-06-28"} {
		if _, err := os.Stat(filepath.Join(dir, keep+".jsonl")); err != nil {
			t.Errorf("%s should be kept (cap satisfied after removing the oldest): %v", keep, err)
		}
	}
}

func TestPurgeBefore(t *testing.T) {
	dir := t.TempDir()
	for _, day := range []string{"2026-06-20", "2026-06-25", "2026-06-26"} {
		if err := os.WriteFile(filepath.Join(dir, day+".jsonl"), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := Purge(dir, time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1 (only the 06-20 file is strictly older)", removed)
	}
	if _, err := os.Stat(filepath.Join(dir, "2026-06-25.jsonl")); err != nil {
		t.Errorf("06-25 should remain (not strictly before 06-25)")
	}
}

func TestPurgeAll(t *testing.T) {
	dir := t.TempDir()
	for _, day := range []string{"2026-06-20", "2026-06-26"} {
		os.WriteFile(filepath.Join(dir, day+".jsonl"), []byte("{}\n"), 0o644)
	}
	removed, err := Purge(dir, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Errorf("purge-all removed = %d, want 2", removed)
	}
}

func TestLoadConfigDefaultsDisabled(t *testing.T) {
	cfg := loadConfigFrom(filepath.Join(t.TempDir(), "absent.json"))
	if cfg.Enabled {
		t.Error("absent config should default to disabled")
	}
	if cfg.Detail != DetailMinimal {
		t.Errorf("default detail = %q, want minimal", cfg.Detail)
	}
}

func TestLoadConfigOmittedRetentionKeepsDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	os.WriteFile(path, []byte(`{"enabled":true}`), 0o644)
	cfg := loadConfigFrom(path)
	if !cfg.Enabled {
		t.Error("enabled should parse true")
	}
	// Omitted retention fields keep their defaults (omit means default, NOT 0).
	if cfg.RetainDays != 90 {
		t.Errorf("omitted retain_days = %d, want default 90", cfg.RetainDays)
	}
	if cfg.MaxBytes != 104857600 {
		t.Errorf("omitted max_bytes = %d, want default 104857600", cfg.MaxBytes)
	}
}

func TestLoadConfigMalformedJSONFallsBackToDisabledDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	os.WriteFile(path, []byte(`{this is not valid json`), 0o644)
	cfg := loadConfigFrom(path)
	if cfg.Enabled {
		t.Error("malformed config should stay disabled")
	}
	if cfg.Detail != DetailMinimal || cfg.RetainDays != 90 || cfg.MaxBytes != 104857600 {
		t.Errorf("malformed config should keep defaults, got %+v", cfg)
	}
}

func TestLoadConfigPreservesFullDetail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	os.WriteFile(path, []byte(`{"enabled":true,"detail":"full"}`), 0o644)
	if cfg := loadConfigFrom(path); cfg.Detail != DetailFull {
		t.Errorf("detail=full should be preserved, got %q", cfg.Detail)
	}
}

func TestLoadConfigNormalizesUnknownDetail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	os.WriteFile(path, []byte(`{"detail":"weird"}`), 0o644)
	if cfg := loadConfigFrom(path); cfg.Detail != DetailMinimal {
		t.Errorf("unknown detail should normalize to minimal, got %q", cfg.Detail)
	}
}

func TestLoadConfigParsesAndNormalizes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	os.WriteFile(path, []byte(`{"enabled":true,"detail":"bogus","retain_days":0}`), 0o644)
	cfg := loadConfigFrom(path)
	if !cfg.Enabled {
		t.Error("enabled should parse true")
	}
	if cfg.Detail != DetailMinimal {
		t.Errorf("unknown detail should normalize to minimal, got %q", cfg.Detail)
	}
	if cfg.RetainDays != 0 {
		t.Errorf("explicit retain_days=0 (unlimited) should be honored, got %d", cfg.RetainDays)
	}
}
