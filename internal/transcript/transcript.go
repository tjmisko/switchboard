// Package transcript inspects the tail of a Claude Code session transcript
// (.jsonl) to decide whether a pending interactive prompt has been resolved.
//
// switchboard's chip status is edge-triggered by Claude Code hooks: an
// AskUserQuestion (or a tool/plan needing approval) fires a PermissionRequest
// hook that latches the chip to "permission" (red). But when the user *declines*
// the prompt — or interrupts the turn — Claude Code fires no clearing hook
// (PostToolUse only fires on success; Stop not on interrupt), so the red latch
// has nothing to release it.
//
// Detecting resolution from the transcript needs care: Claude Code does **not**
// flush an interactive tool_use to the .jsonl until it *resolves*. While a prompt
// is pending, its tool_use is absent and the tail shows the *previous*
// (already-resolved) tool — so a dangling-tool_use scan cannot tell "pending"
// from "just declined". The reliable signal is **time**: every entry carries a
// timestamp, so a tool_result dated after the moment the chip went red means the
// prompt was answered or declined; if the newest tool_result predates that
// moment, nothing has resolved since the prompt appeared and it is still pending.
package transcript

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"
)

// PromptState reports whether a "permission" prompt has resolved.
type PromptState int

const (
	// StateUnknown means the transcript could not be read or parsed. Callers
	// should fall back to their own backstop (e.g. a TTL) rather than guess.
	StateUnknown PromptState = iota
	// StatePending means nothing has resolved since the chip went red — either
	// no tool_result exists yet, or the newest one predates the prompt. The
	// prompt is still waiting on the user; keep nagging.
	StatePending
	// StateResolved means a tool_result is dated after the prompt appeared — the
	// user answered or declined (or Claude otherwise moved on).
	StateResolved
)

func (s PromptState) String() string {
	switch s {
	case StatePending:
		return "pending"
	case StateResolved:
		return "resolved"
	default:
		return "unknown"
	}
}

// DefaultTailBytes bounds how much of the file end ResolutionState reads. The
// signal we need (the newest tool_result) lives at the very end, so a small
// window keeps the check cheap even on multi-megabyte transcripts.
const DefaultTailBytes = 128 * 1024

// entry is the subset of a transcript line we care about: the top-level
// timestamp plus enough of the message to spot a tool_result.
type entry struct {
	Timestamp string `json:"timestamp"`
	Message   struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
		} `json:"content"`
	} `json:"message"`
}

// ResolutionState reads up to maxBytes from the end of the transcript at path
// and reports whether a prompt that latched the chip red at `since` has been
// resolved.
//
//   - StateResolved — the newest tool_result is dated strictly after `since`.
//   - StatePending  — no tool_result is newer than `since` (incl. the case of no
//     tool_result at all, which is a fresh/unflushed prompt — keep nagging).
//   - StateUnknown  — the file is missing/unreadable (returned with a non-nil
//     error); the caller should apply its TTL backstop.
//
// A read that succeeds but finds no usable timestamped tool_result returns
// StatePending (nil error): "can't see a resolution" defaults to keep-red, so a
// genuinely pending prompt is never demoted. Only an actual I/O failure yields
// StateUnknown, so the TTL backstop fires only when the check truly fails.
func ResolutionState(path string, since time.Time, maxBytes int64) (PromptState, error) {
	if path == "" {
		return StateUnknown, errors.New("transcript: empty path")
	}
	f, err := os.Open(path)
	if err != nil {
		return StateUnknown, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return StateUnknown, err
	}
	var start int64
	if maxBytes > 0 && fi.Size() > maxBytes {
		start = fi.Size() - maxBytes
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return StateUnknown, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return StateUnknown, err
	}

	lines := bytes.Split(data, []byte{'\n'})
	// Dropped the partial first line when we seeked into the middle of the file.
	if start > 0 && len(lines) > 0 {
		lines = lines[1:]
	}

	var newest time.Time
	var found bool
	for _, raw := range lines {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var e entry
		if json.Unmarshal(raw, &e) != nil || e.Timestamp == "" {
			continue // tolerate stray/foreign/timestamp-less lines
		}
		hasResult := false
		for _, c := range e.Message.Content {
			if c.Type == "tool_result" {
				hasResult = true
				break
			}
		}
		if !hasResult {
			continue
		}
		ts, err := time.Parse(time.RFC3339, e.Timestamp)
		if err != nil {
			continue
		}
		if ts.After(newest) {
			newest = ts
			found = true
		}
	}

	if found && newest.After(since) {
		return StateResolved, nil
	}
	return StatePending, nil
}
