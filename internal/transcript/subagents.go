package transcript

// This file adds two non-tail-bounded ways to recover Claude Code subagent
// fanouts, so detection no longer hinges on the fragile 128 KiB tail window that
// Tasks()/InFlightTasks() read:
//
//   - TasksSince scans the parent transcript forward from a threaded byte offset,
//     so a spawn or its tool_result is never lost to window scroll-out the way it
//     is when a large turn pushes an early Agent tool_use out of the tail.
//   - SubagentsForTranscript reads the SIBLING metadata directory Claude Code
//     writes per session — <dir>/<session-id>/subagents/agent-*.meta.json — which
//     records every fanout out-of-band from the parent transcript entirely.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Subagent is one fanout recorded in a session's subagents/ metadata dir
// (<dir>/<session-id>/subagents/), reconstructed from the two files Claude Code
// writes per spawn: agent-<id>.meta.json (the fanout's metadata) and
// agent-<id>.jsonl (the subagent's own transcript). It is the out-of-band twin of
// a Task — the same fanout the parent transcript records as an Agent/Task tool_use
// — read from this sibling dir, immune to the parent tail's scroll-out.
//
// This struct reports RAW facts only; policy (inflight counting, depth filtering,
// quiescence/clock-skew, dedup) belongs to the Observer that consumes it.
//
// The universal identity is AgentID — the <id> in the agent-<id>.{meta.json,jsonl}
// filename — NOT toolUseId, which is absent for a meaningful fraction of metas
// (agent-teams teammates, and minimal {agentType,spawnDepth}/{agentType}-only
// variants). AgentID also matches the SubagentStart/Stop hook's agent_id, so it is
// the field to join on across the transcript, the metadata, and the hooks.
type Subagent struct {
	AgentID     string    // <id> from the agent-<id>.{meta.json,jsonl} FILENAME — the universal key/join (matches the SubagentStart/Stop hook agent_id)
	AgentType   string    // agentType from the meta; "" for an orphan jsonl that has no sibling meta
	Description string    // description — best-effort (absent in minimal metas)
	ToolUseID   string    // toolUseId — best-effort (absent ~36% of metas); for parent-transcript cross-check only
	SpawnDepth  int       // spawnDepth — best-effort (absent → 0; 0 = launched by the main thread)
	TaskKind    string    // taskKind — best-effort, e.g. "in_process_teammate"; "" if absent
	HasMeta     bool      // false for an orphan agent-<id>.jsonl with no sibling meta
	Done        bool      // the jsonl's last complete line has message.stop_reason == "end_turn"
	ModTime     time.Time // the agent-<id>.jsonl's mtime, or the meta.json's mtime when no jsonl exists yet; zero only when neither is stat-able
}

// subagentMeta is the subset of an agent-<id>.meta.json we parse. The real files
// are HETEROGENEOUS: only agentType is present in every one. The fuller in-process
// teammate metas add name/teamName/color/model/permissionMode/…, while others are
// as small as {"agentType":"…"}; toolUseId is absent in ~36% and spawnDepth in
// ~65%. Every field here is therefore best-effort — a missing one parses to its
// zero value, never an error (the filename AgentID, not anything in here, is the
// key).
type subagentMeta struct {
	AgentType   string `json:"agentType"`
	Description string `json:"description"`
	ToolUseID   string `json:"toolUseId"`
	SpawnDepth  int    `json:"spawnDepth"`
	TaskKind    string `json:"taskKind"`
}

// subagentFilePrefix/subagentMetaSuffix/subagentJSONLSuffix bracket the AgentID in
// the two filenames a spawn writes: agent-<id>.meta.json and agent-<id>.jsonl. The
// <id> between prefix and suffix is the universal key.
const (
	subagentFilePrefix  = "agent-"
	subagentMetaSuffix  = ".meta.json"
	subagentJSONLSuffix = ".jsonl"
)

// subagentTerminalReason is the assistant stop_reason that marks a subagent's own
// transcript as finished: its final turn ended naturally. A still-running agent's
// last complete line is instead an assistant tool_use (stop_reason "tool_use"), a
// streaming chunk (no stop_reason), or a user tool_result — none of which match,
// so an agent mid-flight reads as not Done.
const subagentTerminalReason = "end_turn"

// subagentTailBytes bounds the tail read used to find a subagent transcript's last
// line for Done detection: large enough to contain a final assistant message,
// small enough to never read a multi-MB subagent transcript whole.
const subagentTailBytes = 128 * 1024

// SubagentsForTranscript derives <dir>/<session-id>/subagents/ from the parent
// transcript path (<dir>/<session-id>.jsonl) and returns one Subagent per fanout.
// The spawn set is the UNION of the agent-*.meta.json and agent-*.jsonl files
// keyed by their agent-<id> filename stem: most ids have both, a just-spawned id
// may have a meta but no jsonl yet (Done=false, ModTime zero), and an orphan jsonl
// may exist with no meta (HasMeta=false, AgentType=""). For each id with a jsonl,
// Done is read from that jsonl's last complete line (a bounded tail read — never
// the whole file) and ModTime is its mtime; a meta-only id takes ModTime from the
// meta.json's mtime so even a just-spawned fanout is dated to real time.
//
// Entries are returned in agent-id order (os.ReadDir sorts by filename; the jsonl
// of an id sorts before its meta, so first-seen order tracks the id). Returns
// (nil, nil) when the dir is absent (the session had no fanouts). Meta fields are
// parsed defensively — a missing field is a zero value, never an error; a
// non-JSON meta is tolerated (the id is still reported, HasMeta=true, fields zero).
// A non-nil error is returned only on an unexpected I/O failure (a file listed in
// the dir but unreadable, or a ReadDir failure that is not "not exist").
func SubagentsForTranscript(transcriptPath string) ([]Subagent, error) {
	if transcriptPath == "" {
		return nil, errors.New("transcript: empty path")
	}
	// <dir>/<session-id>.jsonl → <dir>/<session-id>/subagents/. TrimSuffix is a
	// no-op (leaving the dir absent → nil,nil) if the path lacks the .jsonl suffix.
	dir := filepath.Join(strings.TrimSuffix(transcriptPath, ".jsonl"), "subagents")
	dirents, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil // no fanouts
		}
		return nil, err
	}

	// Union the metadata and transcript files by their agent-<id> stem, preserving
	// first-seen (= filename) order for a stable result.
	byID := map[string]*Subagent{}
	var order []string
	upsert := func(id string) *Subagent {
		s := byID[id]
		if s == nil {
			s = &Subagent{AgentID: id}
			byID[id] = s
			order = append(order, id)
		}
		return s
	}

	for _, de := range dirents {
		name := de.Name()
		if de.IsDir() || !strings.HasPrefix(name, subagentFilePrefix) {
			continue
		}
		switch {
		case strings.HasSuffix(name, subagentMetaSuffix):
			id := strings.TrimSuffix(strings.TrimPrefix(name, subagentFilePrefix), subagentMetaSuffix)
			if id == "" {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				return nil, err // listed but unreadable: a genuine I/O failure
			}
			s := upsert(id)
			s.HasMeta = true
			var m subagentMeta
			if json.Unmarshal(raw, &m) == nil { // tolerate a non-JSON meta: id stays reported, fields zero
				s.AgentType = m.AgentType
				s.Description = m.Description
				s.ToolUseID = m.ToolUseID
				s.SpawnDepth = m.SpawnDepth
				s.TaskKind = m.TaskKind
			}
			// ModTime falls back to the meta's mtime only when no jsonl has supplied
			// one (the jsonl, when present, is the authoritative activity timestamp).
			if s.ModTime.IsZero() {
				if info, err := de.Info(); err == nil {
					s.ModTime = info.ModTime()
				}
			}
		case strings.HasSuffix(name, subagentJSONLSuffix):
			id := strings.TrimSuffix(strings.TrimPrefix(name, subagentFilePrefix), subagentJSONLSuffix)
			if id == "" {
				continue
			}
			s := upsert(id)
			s.Done, s.ModTime = subagentJSONLState(filepath.Join(dir, name))
		}
	}

	subs := make([]Subagent, 0, len(order))
	for _, id := range order {
		subs = append(subs, *byID[id])
	}
	return subs, nil
}

// subagentJSONLState reads the subagent's own transcript at path and reports
// whether it has finished — its last complete (newline-terminated) line has
// message.stop_reason == "end_turn" — along with the file's mtime. A missing jsonl
// (a meta-only spawn with no transcript yet) yields (false, zero time). Only a
// bounded tail (subagentTailBytes) is read, never the whole file: the last line is
// the bytes after the final interior newline of that tail, which is complete
// because the read runs to EOF. Conservative — any read/parse failure, an empty
// file, or a last line whose tail window began mid-line yields Done=false.
func subagentJSONLState(path string) (done bool, mod time.Time) {
	f, err := os.Open(path)
	if err != nil {
		return false, time.Time{} // no jsonl yet (meta-only spawn)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return false, time.Time{}
	}
	mod = fi.ModTime()
	size := fi.Size()
	if size == 0 {
		return false, mod
	}
	var start int64
	if size > subagentTailBytes {
		start = size - subagentTailBytes
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return false, mod
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return false, mod
	}
	// Take the last complete line: drop trailing newline(s), then the bytes after
	// the final interior newline. With no interior newline the segment is the whole
	// read — complete only if we started at byte 0 (else it is a mid-line fragment).
	data = bytes.TrimRight(data, "\r\n")
	if i := bytes.LastIndexByte(data, '\n'); i >= 0 {
		data = data[i+1:]
	} else if start > 0 {
		return false, mod
	}
	var e entry
	if json.Unmarshal(data, &e) != nil {
		return false, mod
	}
	return e.Message.StopReason == subagentTerminalReason, mod
}

// TasksSince reads new transcript bytes from offset to EOF (NOT tail-bounded) and
// returns the Task/Agent tool_use spawns and the tool_use_ids whose tool_result
// landed in this delta, plus the new offset (at a line boundary, like
// UsageSinceByModel — a line caught mid-write is excluded and re-read next call,
// never double-counted). Threading the offset across calls means no spawn or
// result is ever missed to window scroll-out, unlike the tail-bounded Tasks().
//
// spawns and resultIDs are reported separately rather than paired: a spawn's
// tool_result commonly lands in a LATER delta than its spawn, so spawns always
// carry Done=false and the caller correlates resultIDs against the spawn ids it
// has seen across calls. A file shorter than offset (a /clear or session
// replacement truncated it) restarts from 0. Returns a non-nil error only on I/O
// failure.
func TasksSince(path string, offset int64) (spawns []Task, resultIDs []string, newOffset int64, err error) {
	complete, newOffset, err := readNewLines(path, offset)
	if err != nil || len(complete) == 0 {
		return nil, nil, newOffset, err
	}
	for _, raw := range bytes.Split(complete, []byte{'\n'}) {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var e entry
		if json.Unmarshal(raw, &e) != nil {
			continue // tolerate stray/foreign lines
		}
		for _, b := range e.blocks() {
			switch b.Type {
			case "tool_use":
				if taskToolNames[b.Name] && b.ID != "" {
					spawns = append(spawns, Task{
						ID:          b.ID,
						AgentType:   b.Input.SubagentType,
						Description: b.Input.Description,
						Background:  b.Input.RunInBackground,
					})
				}
			case "tool_result":
				if b.ToolUseID != "" {
					resultIDs = append(resultIDs, b.ToolUseID)
				}
			}
		}
	}
	return spawns, resultIDs, newOffset, nil
}
