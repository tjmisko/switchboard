package history

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
)

// Seed scanning: the streaming, all-sessions-at-once replacement for the
// per-session full-store materialization that OOM-killed the daemon on
// 2026-08-26 (docs/seed-replay-memory-plan.md).
//
// What first-sight seeding actually needs is a per-session set-membership
// answer — "which subagents and workflow runs has the log already recorded?" —
// a few hundred short strings per session. The old path
// (PriorSubagentState/PriorWorkflowState) answered it by decoding EVERY line
// of EVERY day-file into one []Event, twice per session: measured on the
// incident store, ~5 GB of transient heap and ~8 s of CPU per pass, times two
// passes, times every live session on a daemon restart. This fold answers it
// in one pass, one line in memory at a time, for every session in the log at
// once — the result is the only thing that survives.

// SeedSets is one session's durable already-recorded reduction: the subagent
// keys with a spawn/stop event in the log, and the workflow run ids with a
// start/stop. Subagent keys follow eventAgentKey: AgentID when present, else
// ToolUseID (older events recorded before agent_id was captured).
type SeedSets struct {
	Spawned         map[string]bool
	Stopped         map[string]bool
	WorkflowStarted map[string]bool
	WorkflowStopped map[string]bool
}

func newSeedSets() *SeedSets {
	return &SeedSets{
		Spawned:         map[string]bool{},
		Stopped:         map[string]bool{},
		WorkflowStarted: map[string]bool{},
		WorkflowStopped: map[string]bool{},
	}
}

// SeedIndex holds the reduction for every session the log has ever recorded
// fanout/workflow events for, keyed by session id.
type SeedIndex map[string]*SeedSets

// Sets returns the sets for one session, never nil: a session the log has
// nothing for gets fresh empty sets, which is exactly the answer seeding
// wants for it.
func (ix SeedIndex) Sets(sessionID string) *SeedSets {
	if s := ix[sessionID]; s != nil {
		return s
	}
	return newSeedSets()
}

// SeedStats describes one scan, for the fanout-seed telemetry line.
type SeedStats struct {
	Files   int   // day-files opened
	Lines   int   // lines streamed past
	Matched int   // lines that decoded to one of the four seeded event types
	Bytes   int64 // bytes streamed
}

// seedEvent is the minimal decode a seeding fold needs — attributing one
// fanout/workflow event to its session and key. Decoding into this instead of
// the full 20-field Event is most of the CPU win.
type seedEvent struct {
	Type          string `json:"type"`
	SessionID     string `json:"session_id"`
	AgentID       string `json:"agent_id"`
	ToolUseID     string `json:"tool_use_id"`
	WorkflowRunID string `json:"workflow_run_id"`
}

// seedTypeMarkers admit a line to the decode. They match the event-type VALUE
// (`"subagent_spawn"`), never the compact `"type":"…"` key-value form, because
// day-files rewritten by the Python repair scripts carry `"type": "…"` with a
// space — the compact form would silently skip exactly the repaired span
// lines, and a missed spawn here re-emits as a duplicate span on the next
// restart. A false positive (the value quoted inside some other field) only
// costs one small decode: the decoded Type is the authoritative check.
var seedTypeMarkers = [][]byte{
	[]byte(`"` + EventSubagentSpawn + `"`),
	[]byte(`"` + EventSubagentStop + `"`),
	[]byte(`"` + EventWorkflowStart + `"`),
	[]byte(`"` + EventWorkflowStop + `"`),
}

// SeedScan folds the whole store into a SeedIndex in one streaming pass.
//
// Line tolerance mirrors ReadDay exactly — a torn or unparseable line is
// skipped, a foreign JSON line without the right `type` is skipped — so the
// sets this builds are the sets the old materializing readers built. A file
// that fails mid-scan aborts with what was folded so far and the error; the
// caller treats a failed scan as "seed from empty", which re-emits at most
// what a first-ever run would.
func SeedScan(dir string) (SeedIndex, SeedStats, error) {
	index, _, stats, err := seedScanAll(dir)
	return index, stats, err
}

// seedScanAll is SeedScan plus the per-day consumed offsets the seed cursor
// persists.
func seedScanAll(dir string) (SeedIndex, map[string]int64, SeedStats, error) {
	index := SeedIndex{}
	offsets := map[string]int64{}
	var stats SeedStats
	days, err := Days(dir)
	if err != nil {
		return index, offsets, stats, err
	}
	for _, day := range days {
		consumed, err := seedScanFileFrom(DayPath(dir, day), 0, index, &stats)
		if err != nil {
			return index, offsets, stats, err
		}
		offsets[day] = consumed
	}
	return index, offsets, stats, nil
}

// seedScanFileFrom streams path from byte offset start, folding each line into
// the index, and returns the offset consumed: start plus every line that ended
// in '\n'. A final line WITHOUT a newline — a crash mid-append — is folded
// best-effort (matching bufio.Scanner's old behavior: complete-JSON is kept,
// torn JSON is skipped by the fold) but never counted as consumed, so a
// cursor resuming at the returned offset re-reads it once the writer completes
// it. Re-folding is harmless: the sets are idempotent.
func seedScanFileFrom(path string, start int64, index SeedIndex, stats *SeedStats) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return start, nil // pruned between listing and open — nothing recorded there anymore
		}
		return start, err
	}
	defer f.Close()
	if start > 0 {
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			return start, err
		}
	}
	stats.Files++

	consumed := start
	r := bufio.NewReaderSize(f, 64*1024)
	for {
		line, err := r.ReadBytes('\n')
		switch {
		case err == nil:
			stats.Lines++
			stats.Bytes += int64(len(line))
			consumed += int64(len(line))
			seedFoldLine(line[:len(line)-1], index, stats)
		case err == io.EOF:
			if len(line) > 0 {
				stats.Lines++
				stats.Bytes += int64(len(line))
				seedFoldLine(line, index, stats) // folded, NOT consumed — see above
			}
			return consumed, nil
		default:
			return consumed, err
		}
	}
}

// seedFoldLine folds one raw line into the index. The full scan and the
// cursor's tail replay both reach it through seedScanFileFrom, so the
// admission rules can never diverge between them.
func seedFoldLine(line []byte, index SeedIndex, stats *SeedStats) {
	admitted := false
	for _, marker := range seedTypeMarkers {
		if bytes.Contains(line, marker) {
			admitted = true
			break
		}
	}
	if !admitted {
		return
	}
	var ev seedEvent
	if json.Unmarshal(line, &ev) != nil {
		return // torn line (crash mid-append) — same tolerance as ReadDay
	}
	if ev.SessionID == "" {
		return
	}
	// The subagent correlation key, mirroring eventAgentKey: the universal
	// AgentID when present, ToolUseID for events recorded before agent_id
	// was captured.
	key := ev.AgentID
	if key == "" {
		key = ev.ToolUseID
	}
	sets := index[ev.SessionID]
	ensure := func() *SeedSets {
		if sets == nil {
			sets = newSeedSets()
			index[ev.SessionID] = sets
		}
		return sets
	}
	switch ev.Type {
	case EventSubagentSpawn:
		if key != "" {
			ensure().Spawned[key] = true
			stats.Matched++
		}
	case EventSubagentStop:
		if key != "" {
			ensure().Stopped[key] = true
			stats.Matched++
		}
	case EventWorkflowStart:
		if ev.WorkflowRunID != "" {
			ensure().WorkflowStarted[ev.WorkflowRunID] = true
			stats.Matched++
		}
	case EventWorkflowStop:
		if ev.WorkflowRunID != "" {
			ensure().WorkflowStopped[ev.WorkflowRunID] = true
			stats.Matched++
		}
	}
}
