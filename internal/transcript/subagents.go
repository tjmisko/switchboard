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
// <id> between prefix and suffix is the universal key. subagentsDirName is the dir
// those two files live in, under the session's own sibling directory.
const (
	subagentFilePrefix  = "agent-"
	subagentMetaSuffix  = ".meta.json"
	subagentJSONLSuffix = ".jsonl"
	subagentsDirName    = "subagents"
)

// subagentsDirForTranscript derives <dir>/<session-id>/subagents/ from a parent
// transcript path <dir>/<session-id>.jsonl. It is the SINGLE derivation in this
// package: SubagentsForTranscript and SubagentPath both route through it so the
// two can never drift apart.
//
// The dir is derived from the PASSED path and nothing else — never from cwd, a
// project slug, or any re-derivation of either. That is what keeps it correct for
// a session running in a git worktree (its records still live beside the
// transcript switchboard already stores), for a /name-renamed session, and under
// an XDG-relocated ~/.claude (subagent-fanout-detection-plan.md, G10).
//
// TrimSuffix is a no-op when the path lacks the .jsonl suffix, which simply leaves
// the derived dir absent rather than inventing one. An empty path yields "" rather
// than the bare relative "subagents" that filepath.Join would otherwise produce.
func subagentsDirForTranscript(mainTranscript string) string {
	if mainTranscript == "" {
		return ""
	}
	return filepath.Join(strings.TrimSuffix(mainTranscript, subagentJSONLSuffix), subagentsDirName)
}

// SubagentPath returns the transcript file a given writer's OWN entries land in,
// given the parent (main-thread) transcript path:
//
//	agentID == ""  → mainTranscript, unchanged (the main thread writes there)
//	agentID != ""  → <dir>/<session-id>/subagents/agent-<agentID>.jsonl
//
// A subagent's writes are NOT in the parent transcript, and a hook fired from
// inside a subagent still reports the PARENT's transcript_path
// (claude-code-hook-schema.md §3) — so while a teammate works, the parent file can
// be arbitrarily stale and this sibling file is the only evidence of that
// teammate's activity.
//
// The empty-agentID case returning mainTranscript unchanged is deliberate: it lets
// a caller resolve a pending writer with no branch, passing whatever agent_id the
// hook carried (empty ⇒ main thread) straight through.
//
// agentID must be BARE: the <id> between "agent-" and the suffix in the filename
// pair agent-<id>.meta.json / agent-<id>.jsonl — exactly what Subagent.AgentID
// holds. This function does NOT strip a leading "agent-", and no caller may strip
// one on its behalf. Normalization of the hook's agent_id happens ONCE, at the RPC
// boundary (normalizeAgentID), which removes at most one leading prefix; a second
// strip anywhere downstream would eat a legitimate prefix.
//
// That is not hypothetical. A named subagent's id is shaped a<name><hex>, and the
// name is user-supplied — a subagent named "gent-foo" gets id "agent-foo-<hex>",
// stored on disk as agent-agent-foo-<hex>.jsonl. Stripping here would resolve it to
// agent-foo-<hex>.jsonl, a DIFFERENT agent's transcript. A resolver would then read
// that other agent's activity and clear a prompt this agent is still blocked on: a
// missed RED, silent and unrecoverable.
//
// The two failure modes are deliberately asymmetric. Over-stripping is silent and
// wrong. Under-stripping is not: a caller that mistakenly passes a still-prefixed
// id derives agent-agent-<id>.jsonl, which does not exist, so the read fails and the
// caller keeps its pending entry. Fail-safe is the direction to err in, so the
// prefix is left alone.
//
// A trailing ".jsonl" IS tolerated — an id and its filename are interchangeable at
// call sites, and ".jsonl" is a suffix no bare id carries.
//
// Returns "" when there is nothing sane to derive: an empty mainTranscript, an
// agentID that is only the ".jsonl" suffix, or an agentID containing a path
// separator (a hook-supplied value must not be able to escape the subagents dir).
func SubagentPath(mainTranscript, agentID string) string {
	if agentID == "" {
		return mainTranscript // main thread: its writes are the parent transcript
	}
	dir := subagentsDirForTranscript(mainTranscript)
	if dir == "" {
		return ""
	}
	// No TrimPrefix: agentID is already bare (see the contract above). Stripping a
	// second "agent-" here would silently retarget another agent's transcript.
	id := strings.TrimSuffix(agentID, subagentJSONLSuffix)
	if id == "" || strings.ContainsRune(id, filepath.Separator) || strings.ContainsRune(id, '/') {
		return ""
	}
	return filepath.Join(dir, subagentFilePrefix+id+subagentJSONLSuffix)
}

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
	// <dir>/<session-id>.jsonl → <dir>/<session-id>/subagents/, via the one shared
	// derivation SubagentPath also uses (a path lacking .jsonl simply leaves the dir
	// absent → nil,nil).
	dir := subagentsDirForTranscript(transcriptPath)
	dirents, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil // no fanouts
		}
		return nil, err
	}

	// Union the metadata and transcript files by their agent-<id> stem, preserving
	// first-seen (= filename) order for a stable result.
	//
	// The TrimPrefix below parses a FILENAME — it is what MAKES Subagent.AgentID bare,
	// and is unrelated to normalizing a hook-supplied agent_id. Do not read it as
	// license to strip again in SubagentPath: an id that itself begins with "agent-"
	// lives in agent-agent-<id>.jsonl, and exactly one strip recovers it.
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
