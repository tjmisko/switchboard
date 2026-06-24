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
// flush an interactive tool_use to the .jsonl until it *resolves*, and while the
// prompt waits the session keeps writing — a background teammate/subagent and any
// sibling auto-approved tool in the same turn flush tool_results dated *after* the
// chip went red. So "a tool_result newer than the prompt" cannot tell a resolved
// prompt from one still pending amid concurrent work; counting it demotes the red
// chip the instant any background work lands. The reliable signal is the *main
// conversation thread* advancing past the prompt: an assistant message dated after
// the prompt (the blocked turn resumed → the awaited tool was approved) or a user
// interrupt notice (declined / Esc). Tool_results — which subagents and parallel
// tools emit while the prompt still waits — are deliberately ignored.
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
	// StatePending means nothing has resolved since the chip went red — no
	// assistant message or interrupt notice is newer than the prompt (background
	// tool_results from subagents/parallel tools do not count). The prompt is
	// still waiting on the user; keep nagging.
	StatePending
	// StateResolved means the main conversation thread advanced past the prompt —
	// an assistant message or a user interrupt notice dated after it appeared (the
	// user answered, declined, or interrupted).
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

// localCommandPrefixes are the tags Claude Code wraps around the synthetic user
// entries it writes for local side-channel commands — a `!` bash command
// (<bash-input>/<bash-stdout>/<bash-stderr>, plus a <local-command-caveat>), and a
// `/` slash command (<command-name>/<command-message>/<command-args> and its
// <local-command-stdout|stderr> output). These run with NO agent turn: they fire
// neither UserPromptSubmit nor Stop, so they must not count as conversational
// activity. Treating them as activity made the idle→working self-heal misfire — a
// user who runs `!git status` in an idle (orange) session flushed a <bash-stdout>
// entry dated after the Stop, which NewestSignal read as "the session resumed" and
// promoted the chip back to green, where it latched forever (no Stop hook ever
// follows a local command to bring it down).
//
// Slash commands warrant care because some DO start an agent — but that path is
// already covered without this signal: a command that kicks off a turn fires
// UserPromptSubmit, which sets the chip working via the hook, so by reconcile time
// the status is no longer "idle" and the idle→working branch is never consulted.
// The classification only matters when the chip is *still* idle — i.e. no
// UserPromptSubmit fired — i.e. a purely-local command (/clear, /rename) that
// started no agent, exactly the case that must not flip green. A genuine prompt,
// likewise, fires UserPromptSubmit, so excluding all of these costs no real resume
// signal (at worst the first assistant message lands a beat later and flips it).
var localCommandPrefixes = []string{"<bash-", "<command-", "<local-command-"}

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
	// Tool-call fields, populated only for the relevant block types. Name/ID
	// identify a tool_use (the tool invoked and its id); ToolUseID back-links a
	// tool_result to the tool_use it answers. They let InFlightTasks pair launched
	// subagent Tasks against their completions over the tail.
	Name      string `json:"name"`
	ID        string `json:"id"`
	ToolUseID string `json:"tool_use_id"`
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
// reports whether a prompt that latched the chip red at `since` has been resolved
// — distinguishing the user moving past the prompt from unrelated activity (a
// background teammate/subagent, or a sibling auto-approved tool in the same turn)
// that keeps writing while the prompt still waits.
//
// Resolution is signalled only by the *main conversation thread* advancing past
// the prompt, which takes one of two forms in the tail (see resolutionKindOf):
//
//   - an assistant message dated after `since` — the blocked turn produced new
//     output, so the awaited tool was approved and ran (Claude Code withholds the
//     pending tool_use's assistant message until it resolves, so any assistant
//     entry newer than `since` postdates the approval);
//   - a user interrupt notice ("[Request interrupted by user…") dated after
//     `since` — the prompt was declined or the turn was Esc-interrupted (neither
//     fires a clearing hook).
//
// A plain user tool_result is deliberately NOT a resolution signal: subagent
// reports and parallel auto-approved tools land as tool_results dated after the
// prompt while it is still genuinely pending, and counting them would flash a red
// chip green the moment any background work completed.
//
//   - StateResolved — the newest resolution entry is dated strictly after `since`.
//   - StatePending  — no resolution entry is newer than `since` (incl. none at
//     all, a fresh/unflushed prompt — keep nagging).
//   - StateUnknown  — the file is missing/unreadable (returned with a non-nil
//     error); the caller should apply its TTL backstop.
//
// A read that succeeds but finds no usable resolution entry returns StatePending
// (nil error): "can't see a resolution" defaults to keep-red, so a genuinely
// pending prompt is never demoted. Only an actual I/O failure yields StateUnknown,
// so the TTL backstop fires only when the check truly fails.
func ResolutionState(path string, since time.Time, maxBytes int64) (PromptState, error) {
	kind, err := ResolveKind(path, since, maxBytes)
	if err != nil {
		return StateUnknown, err
	}
	if kind == ResolutionNone {
		return StatePending, nil
	}
	return StateResolved, nil
}

// ResolutionKind classifies *how* a permission prompt resolved, which selects
// the chip's exit color (see the reconciler's selfHealStaleAttention). The plain
// PromptState answers "is it resolved?"; this answers "resolved which way?", so
// an approved prompt whose turn resumed can go straight to green (working)
// instead of bouncing through orange (idle) on the way (see §2.1 / P3 in
// docs/status-color-state-model.md).
type ResolutionKind int

const (
	// ResolutionNone — nothing dated after `since` advanced the main thread past
	// the prompt; it is still pending (keep nagging). Bare tool_results from
	// concurrent subagent/parallel work do not count.
	ResolutionNone ResolutionKind = iota
	// ResolutionResumed — the newest post-`since` resolution entry is an assistant
	// message: the blocked turn produced new output, so the awaited tool was
	// approved and work resumed. The chip should exit to working (green).
	ResolutionResumed
	// ResolutionInterrupted — the newest post-`since` resolution entry is a user
	// interrupt notice ("[Request interrupted by user…"): the turn was Esc'd or
	// the prompt declined with no continuation, returning control to the user. The
	// chip should exit to idle (orange).
	ResolutionInterrupted
)

func (k ResolutionKind) String() string {
	switch k {
	case ResolutionResumed:
		return "resumed"
	case ResolutionInterrupted:
		return "interrupted"
	default:
		return "none"
	}
}

// ResolveKind reports how a prompt that latched the chip red at `since` resolved.
// It is the kind-aware core of ResolutionState: it scans the tail for the newest
// entry that advances the main conversation thread past the prompt and returns
// what kind it was — an assistant message (ResolutionResumed) or a user interrupt
// notice (ResolutionInterrupted) — newest wins, so a decline the model continued
// past (an assistant message after the rejection) reads as Resumed. A bare
// tool_result is deliberately NOT a resolution: concurrent subagent/parallel work
// flushes tool_results dated after the prompt while it is still pending, so
// counting them would clear the red chip the instant any background work landed.
//
//   - (kind, nil) where kind != None — the newest resolution entry is dated
//     strictly after `since`.
//   - (ResolutionNone, nil) — nothing newer than `since` resolved it (incl. none
//     at all, a fresh/unflushed prompt — keep nagging).
//   - (ResolutionNone, err) — the file is missing/unreadable; the caller should
//     apply its TTL backstop.
func ResolveKind(path string, since time.Time, maxBytes int64) (ResolutionKind, error) {
	entries, err := readTailEntries(path, maxBytes)
	if err != nil {
		return ResolutionNone, err
	}

	var newest time.Time
	kind := ResolutionNone
	for _, e := range entries {
		k := resolutionKindOf(e)
		if k == ResolutionNone {
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

	if kind != ResolutionNone && newest.After(since) {
		return kind, nil
	}
	return ResolutionNone, nil
}

// resolutionKindOf maps an entry to the resolution it represents: an assistant
// message means the blocked turn resumed (approved → Resumed); a user interrupt
// notice means it was declined/interrupted (Interrupted). Everything else —
// including a bare user tool_result from concurrent subagent/parallel work — is
// ResolutionNone.
func resolutionKindOf(e entry) ResolutionKind {
	if e.Message.Role == "assistant" {
		return ResolutionResumed
	}
	if classify(e) == SignalInterrupt {
		return ResolutionInterrupted
	}
	return ResolutionNone
}

// taskToolNames are the tool_use names whose invocation spawns a subagent. Work
// done inside such a subagent is "work happening" for the delegating-green rule
// (docs/status-color-state-model.md §5 cases 5/14): a main thread that has ended
// its turn but still has an in-flight Task is delegating, not idle.
var taskToolNames = map[string]bool{"Task": true, "Agent": true}

// InFlightTasks counts the subagent Tasks the main thread has launched but not
// yet collected: tool_use blocks named Task/Agent in the tail whose id has no
// matching tool_result.tool_use_id. The daemon reads this each reconcile tick to
// decide whether an idle main thread is actually delegating (→ render green).
//
// The read is tail-bounded (maxBytes), so a Task whose launching tool_use has
// scrolled out of the window but whose result is still in-window cannot be
// paired; the count is clamped at 0 rather than going negative. With the default
// window (DefaultTailBytes) this only bites pathologically long turns. Returns a
// non-nil error only on I/O failure (an empty/missing transcript), letting the
// caller leave the last-known count rather than guess.
func InFlightTasks(path string, maxBytes int64) (int, error) {
	entries, err := readTailEntries(path, maxBytes)
	if err != nil {
		return 0, err
	}
	launched := make(map[string]struct{})
	done := make(map[string]struct{})
	for _, e := range entries {
		for _, b := range e.blocks() {
			switch b.Type {
			case "tool_use":
				if taskToolNames[b.Name] && b.ID != "" {
					launched[b.ID] = struct{}{}
				}
			case "tool_result":
				if b.ToolUseID != "" {
					done[b.ToolUseID] = struct{}{}
				}
			}
		}
	}
	n := 0
	for id := range launched {
		if _, ok := done[id]; !ok {
			n++
		}
	}
	return n, nil
}

// NewestSignal reads up to maxBytes from the end of the transcript at path and
// returns the kind and timestamp of the newest timestamped entry that is either
// conversational activity (an assistant message, or a user message that is
// neither an interrupt notice nor a local-command side-channel record) or a user
// interrupt notice. Timestamp-less metadata, ancillary system entries, and the
// synthetic `!` bash / `/` slash-command entries (see localCommandPrefixes) are
// ignored — none represent an agent turn. It returns (SignalNone, zero, nil)
// when the tail holds no classifiable entry, and a non-nil error only on I/O
// failure.
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

// AnchorTime returns the timestamp a hook-driven status transition should be
// dated from: the newest turn entry in the transcript tail. It exists to kill a
// clock-skew class of bug (see docs/timing-hazards.md).
//
// A hook fires only AFTER Claude Code has written the entry that triggered it,
// but the daemon stamps the transition when it PROCESSES the hook — later, by the
// hook subprocess spawn + socket round-trip (tens to hundreds of ms). Dating the
// transition from that late wall-clock moment puts StatusSince AHEAD of the
// transcript event it represents. The reconciler then asks "did anything happen
// after the chip transitioned?" by comparing transcript timestamps against
// StatusSince — and a fast follow-up action that lands inside the hook gap (a
// Ctrl+C right after a prompt) carries a transcript timestamp EARLIER than that
// inflated StatusSince, so the real signal is wrongly filtered as stale and the
// hookless recovery never fires.
//
// Anchoring to the newest turn entry puts StatusSince on the same event stream,
// sampled at the same causal point, as the signals later compared against it, so
// a genuinely-later signal always reads as later regardless of hook latency.
// ok is false when the tail holds no timestamped turn entry (empty or unreadable
// transcript), so the caller falls back to wall-clock now — the pre-fix behavior,
// now confined to that degenerate case.
func AnchorTime(path string, maxBytes int64) (ts time.Time, ok bool) {
	_, newest, err := NewestSignal(path, maxBytes)
	if err != nil || newest.IsZero() {
		return time.Time{}, false
	}
	return newest, true
}

// classify maps an entry to its status signal: an assistant message is activity;
// a user message is an interrupt notice when a text block carries the interrupt
// marker, a local-command side-channel entry (no agent turn — see
// localCommandPrefixes) when it carries one of those tags, otherwise activity.
// Everything else (system, metadata) is SignalNone. A user tool_result keeps
// counting as activity: its blocks are tool_result, not text, so neither special
// case matches — that is the real "agent is mid-turn" signal the resume self-heal
// relies on.
func classify(e entry) Signal {
	switch e.Message.Role {
	case "assistant":
		return SignalActivity
	case "user":
		for _, b := range e.blocks() {
			if b.Type != "text" {
				continue
			}
			text := strings.TrimSpace(b.Text)
			if strings.HasPrefix(text, interruptMarkerPrefix) {
				return SignalInterrupt
			}
			if isLocalCommand(text) {
				return SignalNone
			}
		}
		return SignalActivity
	default:
		return SignalNone
	}
}

// isLocalCommand reports whether trimmed user-entry text is one of Claude Code's
// synthetic local-command records (a `!` bash command or `/` slash command),
// which fire no UserPromptSubmit/Stop hook pair and so are not agent activity.
func isLocalCommand(text string) bool {
	for _, p := range localCommandPrefixes {
		if strings.HasPrefix(text, p) {
			return true
		}
	}
	return false
}
