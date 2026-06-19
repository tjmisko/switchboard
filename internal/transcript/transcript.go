// Package transcript inspects the tail of a Claude Code session transcript
// (.jsonl) to recover status the hooks don't deliver.
//
// switchboard's chip status is edge-triggered by Claude Code hooks, and some
// edges never fire:
//
//   - An AskUserQuestion (or a tool/plan needing approval) fires a
//     PermissionRequest hook that latches the chip "permission" (red). Declining
//     the prompt — or interrupting the turn — fires no clearing hook (PostToolUse
//     only fires on success; Stop not on interrupt), so the red latch has nothing
//     to release it. ResolutionState recovers it.
//   - Interrupting a turn (Esc) fires no Stop hook, so a "working" (green) chip
//     never falls to idle; and resuming work after a Stop (e.g. an orchestrator
//     woken by a background teammate) fires no working hook, so an "idle"
//     (orange) chip never returns to green. NewestSignal recovers both.
//
// Detecting resolution from the transcript needs care: Claude Code does **not**
// flush an interactive tool_use to the .jsonl until it *resolves*. While a prompt
// is pending, its tool_use is absent and the tail shows the *previous*
// (already-resolved) tool — so a dangling-tool_use scan cannot tell "pending"
// from "just declined". The reliable signal is **time**: every conversational
// entry carries a timestamp, so a tool_result dated after the moment the chip
// went red means the prompt was answered or declined; if the newest tool_result
// predates that moment, nothing has resolved since the prompt appeared.
package transcript

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
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

// Signal classifies the newest conversational entry for the idle↔working
// self-heal (see NewestSignal).
type Signal int

const (
	// SignalNone means the tail held no classifiable conversational entry.
	SignalNone Signal = iota
	// SignalActivity means the session produced work — an assistant message, or
	// a user message that is not an interrupt notice.
	SignalActivity
	// SignalInterrupt means the user interrupted the turn ("[Request interrupted
	// by user]"), which fires no Stop hook.
	SignalInterrupt
)

func (s Signal) String() string {
	switch s {
	case SignalActivity:
		return "activity"
	case SignalInterrupt:
		return "interrupt"
	default:
		return "none"
	}
}

// interruptMarkerPrefix is the text Claude Code writes as a user entry when a
// turn is interrupted: "[Request interrupted by user]" and the "…for tool use]"
// variant both share this prefix. A completed tool merely records
// "interrupted":false inside its result, which is not a text block, so it does
// not match.
const interruptMarkerPrefix = "[Request interrupted by user"

// DefaultTailBytes bounds how much of the file end the readers consume. The
// signals we need (the newest tool_result, the newest conversational entry) live
// at the very end, so a small window keeps the check cheap even on multi-megabyte
// transcripts.
const DefaultTailBytes = 128 * 1024

// entry is the subset of a transcript line we care about: the top-level
// timestamp plus the message role and its raw content. Content is kept raw
// because Claude Code writes it either as an array of typed blocks or, for some
// user entries, as a bare string — blocks() reconciles both.
type entry struct {
	Timestamp string `json:"timestamp"`
	Message   struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type block struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// blocks parses message.content tolerantly: an array of typed blocks yields its
// elements; a bare string yields one synthetic text block; anything else (null,
// object, unparseable) yields nil. This keeps a string-content user entry from
// being dropped while still surfacing tool_result/text blocks from array content.
func (e entry) blocks() []block {
	raw := bytes.TrimSpace(e.Message.Content)
	if len(raw) == 0 {
		return nil
	}
	switch raw[0] {
	case '[':
		var bs []block
		if json.Unmarshal(raw, &bs) != nil {
			return nil
		}
		return bs
	case '"':
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return nil
		}
		return []block{{Type: "text", Text: s}}
	default:
		return nil
	}
}

// parsedTime returns the entry's timestamp, or false when it is absent or
// unparseable (the metadata entries Claude Code interleaves — mode, custom-title,
// last-prompt, … — carry no timestamp and are thereby ignored).
func (e entry) parsedTime() (time.Time, bool) {
	if e.Timestamp == "" {
		return time.Time{}, false
	}
	ts, err := time.Parse(time.RFC3339, e.Timestamp)
	if err != nil {
		return time.Time{}, false
	}
	return ts, true
}

// readTailEntries reads up to maxBytes from the end of the transcript at path and
// returns the parsed entries. It drops the partial first line when the read began
// mid-file and tolerates stray/foreign/unparseable lines. A missing/unreadable
// file (or empty path) returns a non-nil error so callers can apply a backstop.
func readTailEntries(path string, maxBytes int64) ([]entry, error) {
	if path == "" {
		return nil, errors.New("transcript: empty path")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	var start int64
	if maxBytes > 0 && fi.Size() > maxBytes {
		start = fi.Size() - maxBytes
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}

	lines := bytes.Split(data, []byte{'\n'})
	// Drop the partial first line when we seeked into the middle of the file.
	if start > 0 && len(lines) > 0 {
		lines = lines[1:]
	}
	entries := make([]entry, 0, len(lines))
	for _, raw := range lines {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var e entry
		if json.Unmarshal(raw, &e) != nil {
			continue // tolerate stray/foreign lines
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// ResolutionState reads up to maxBytes from the end of the transcript at path and
// reports whether a prompt that latched the chip red at `since` has been
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
	entries, err := readTailEntries(path, maxBytes)
	if err != nil {
		return StateUnknown, err
	}

	var newest time.Time
	var found bool
	for _, e := range entries {
		if !hasToolResult(e) {
			continue
		}
		ts, ok := e.parsedTime()
		if !ok {
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

func hasToolResult(e entry) bool {
	for _, b := range e.blocks() {
		if b.Type == "tool_result" {
			return true
		}
	}
	return false
}

// NewestSignal reads up to maxBytes from the end of the transcript at path and
// returns the kind and timestamp of the newest timestamped entry that is either
// conversational activity (an assistant message, or a user message that is not an
// interrupt notice) or a user interrupt notice. Timestamp-less metadata and
// ancillary system entries are ignored. It returns (SignalNone, zero, nil) when
// the tail holds no classifiable entry, and a non-nil error only on I/O failure.
//
// Callers compare the returned timestamp against the moment the chip last
// transitioned: a SignalActivity newer than that means an idle chip's session
// resumed work (→ working); a SignalInterrupt newer than that means a working
// chip's turn was interrupted with no Stop hook (→ idle).
func NewestSignal(path string, maxBytes int64) (Signal, time.Time, error) {
	entries, err := readTailEntries(path, maxBytes)
	if err != nil {
		return SignalNone, time.Time{}, err
	}

	var newest time.Time
	kind := SignalNone
	for _, e := range entries {
		k := classify(e)
		if k == SignalNone {
			continue
		}
		ts, ok := e.parsedTime()
		if !ok {
			continue
		}
		if ts.After(newest) {
			newest = ts
			kind = k
		}
	}
	return kind, newest, nil
}

// classify maps an entry to its status signal: an assistant message is activity;
// a user message is an interrupt notice when a text block carries the interrupt
// marker, otherwise activity. Everything else (system, metadata) is SignalNone.
func classify(e entry) Signal {
	switch e.Message.Role {
	case "assistant":
		return SignalActivity
	case "user":
		for _, b := range e.blocks() {
			if b.Type == "text" && strings.HasPrefix(strings.TrimSpace(b.Text), interruptMarkerPrefix) {
				return SignalInterrupt
			}
		}
		return SignalActivity
	default:
		return SignalNone
	}
}
