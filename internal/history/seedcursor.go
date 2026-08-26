package history

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

// The seed cursor makes a daemon restart's seeding O(appended tail) instead of
// O(store): a derived snapshot of the SeedScan reduction plus, per day-file,
// how many bytes that reduction has consumed. The next start loads it, streams
// only the bytes past each recorded offset (which is exactly what the previous
// daemon wrote during its life), and rewrites it.
//
// Two properties are load-bearing (docs/seed-replay-memory-plan.md):
//
//   - The log stays the sole source of truth. The cursor is a cache of a
//     reduction, never authoritative: any validation failure — missing file
//     where bytes were consumed is fine (retention pruned it; its contribution
//     is already IN the sets), but a file SHORTER than its offset means a
//     rewrite (a scrub, a repair) — discards the cursor and rebuilds from a
//     full SeedScan, silently. Degradation is always "replay more", never
//     "wrong answer".
//   - Offsets are line-exact. A consumed offset always lands on a line
//     boundary because it only advances past lines that ended in '\n'; a
//     torn final line (crash mid-append) is folded best-effort but NOT
//     consumed, so the next start re-reads it once completed. Re-folding a
//     line is harmless — the sets are idempotent.
//
// The file lives beside the day-files as .seed-cursor-v1.json (dot-named, so
// listDayFiles never mistakes it for a day) and is written once per daemon
// start, immediately after seeding — no runtime flushing, no coupling to the
// sink's writer goroutine.

const seedCursorName = ".seed-cursor-v1.json"

// SeedResult is a seeding pass's complete outcome: the reduction, the
// per-day-file consumed offsets backing it, the scan stats for telemetry, and
// which path produced it ("scan" for a full rebuild, "cursor" for a load plus
// tail replay).
type SeedResult struct {
	Index   SeedIndex
	Offsets map[string]int64 // day key ("2026-08-26") -> bytes consumed
	Stats   SeedStats
	Source  string
}

// seedCursorFile is the on-disk form. Sets are sorted slices so the file is
// deterministic and diffable.
type seedCursorFile struct {
	Version  int                       `json:"version"`
	Files    map[string]int64          `json:"files"`
	Sessions map[string]seedCursorSets `json:"sessions"`
}

type seedCursorSets struct {
	Spawned   []string `json:"spawned,omitempty"`
	Stopped   []string `json:"stopped,omitempty"`
	WfStarted []string `json:"wf_started,omitempty"`
	WfStopped []string `json:"wf_stopped,omitempty"`
}

// SeedLoad produces the seeding reduction the cheapest valid way: from the
// cursor plus a tail replay when the cursor validates, from a full SeedScan
// when it is absent, unreadable, versioned wrong, or contradicted by a
// day-file shorter than its recorded offset. The rebuild is silent by design —
// the caller learns which path ran from Source, and the telemetry line
// records it, but nothing treats it as an error.
func SeedLoad(dir string) (SeedResult, error) {
	if res, ok := seedLoadFromCursor(dir); ok {
		return res, nil
	}
	index, offsets, stats, err := seedScanAll(dir)
	return SeedResult{Index: index, Offsets: offsets, Stats: stats, Source: "scan"}, err
}

func seedLoadFromCursor(dir string) (SeedResult, bool) {
	data, err := os.ReadFile(filepath.Join(dir, seedCursorName))
	if err != nil {
		return SeedResult{}, false
	}
	var cur seedCursorFile
	if json.Unmarshal(data, &cur) != nil || cur.Version != 1 {
		return SeedResult{}, false
	}

	files, err := listDayFiles(dir)
	if err != nil {
		return SeedResult{}, false
	}
	sizes := make(map[string]int64, len(files))
	for _, f := range files {
		sizes[dayKey(f.date)] = f.size
	}
	// A file shorter than its consumed offset was rewritten under the cursor
	// (a scrub, a repair): every recorded offset into it is meaningless, and
	// so, transitively, is the reduction — rebuild. A file that is GONE is the
	// opposite case: retention pruned it, and its contribution already lives
	// in the sets; only its offset entry is dropped.
	for day, off := range cur.Files {
		if size, exists := sizes[day]; exists && size < off {
			return SeedResult{}, false
		}
	}

	res := SeedResult{
		Index:   SeedIndex{},
		Offsets: map[string]int64{},
		Source:  "cursor",
	}
	for sid, sets := range cur.Sessions {
		res.Index[sid] = &SeedSets{
			Spawned:         setFromSlice(sets.Spawned),
			Stopped:         setFromSlice(sets.Stopped),
			WorkflowStarted: setFromSlice(sets.WfStarted),
			WorkflowStopped: setFromSlice(sets.WfStopped),
		}
	}
	// Replay only the tails: bytes past each recorded offset, and new files
	// from 0. Order is oldest-first via the listing, though order cannot
	// matter — the fold is commutative set insertion.
	for _, f := range files {
		day := dayKey(f.date)
		start := cur.Files[day]
		if f.size <= start {
			res.Offsets[day] = start
			continue // untouched since the cursor was written — no open, no read
		}
		consumed, err := seedScanFileFrom(f.path, start, res.Index, &res.Stats)
		if err != nil {
			return SeedResult{}, false // unreadable tail — rebuild from scratch
		}
		res.Offsets[day] = consumed
	}
	return res, true
}

// WriteSeedCursor persists the result atomically (temp + rename). Failure is
// returned for the caller to log; the cursor is a cache, so a failed write
// costs the next start one full scan and nothing else.
func WriteSeedCursor(dir string, res SeedResult) error {
	cur := seedCursorFile{
		Version:  1,
		Files:    res.Offsets,
		Sessions: make(map[string]seedCursorSets, len(res.Index)),
	}
	for sid, sets := range res.Index {
		cur.Sessions[sid] = seedCursorSets{
			Spawned:   sliceFromSet(sets.Spawned),
			Stopped:   sliceFromSet(sets.Stopped),
			WfStarted: sliceFromSet(sets.WorkflowStarted),
			WfStopped: sliceFromSet(sets.WorkflowStopped),
		}
	}
	data, err := json.Marshal(cur)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, seedCursorName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func setFromSlice(vals []string) map[string]bool {
	set := make(map[string]bool, len(vals))
	for _, v := range vals {
		set[v] = true
	}
	return set
}

func sliceFromSet(set map[string]bool) []string {
	if len(set) == 0 {
		return nil
	}
	vals := make([]string, 0, len(set))
	for v := range set {
		vals = append(vals, v)
	}
	sort.Strings(vals)
	return vals
}
