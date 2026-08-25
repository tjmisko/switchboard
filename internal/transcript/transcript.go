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
// Detecting resolution from the transcript needs care, and NOT for the reason
// this comment used to give. Claude Code *does* flush an interactive tool_use to
// the .jsonl while the prompt waits — measured at ~5 s after the PermissionRequest
// hook and minutes-to-hours before the user answers, in the main transcript as
// well as a subagent's own file (docs/subagent-permission-plan.md §9.7, V4). What
// it does not do is flush it *before* the hook: the pending tool_use and its
// pre-prompt thinking/text turn-mates land a beat late, dated at generation time,
// which is the H7/H8 flush race (docs/timing-hazards.md) that AnchorSince defuses
// by anchoring a permission edge to wall-clock now.
//
// The real difficulty is concurrency. While the prompt waits the session keeps
// writing — a background teammate/subagent and any sibling auto-approved tool in
// the same turn flush tool_results dated *after* the chip went red. So "a
// tool_result newer than the prompt" cannot tell a resolved prompt from one still
// pending amid concurrent work; counting it demotes the red chip the instant any
// background work lands. The reliable signal is the *main conversation thread*
// advancing past the prompt: an assistant message dated after the prompt (the
// blocked turn resumed → the awaited tool was approved) or a user interrupt notice
// (declined / Esc). Tool_results — which subagents and parallel tools emit while
// the prompt still waits — are deliberately ignored.
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
	// UUID identifies the transcript row. Current assistant rows also carry the
	// provider message id below; UUID is a content-free fallback for older rows.
	UUID    string `json:"uuid"`
	Message struct {
		ID      string          `json:"id"`
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
		// Model names the model that produced an assistant message (e.g.
		// "claude-opus-4-8"); UsageSinceByModel buckets token usage by it so each
		// model's tokens can be priced at its own rate. Absent on user/system entries.
		Model string `json:"model"`
		// Usage is the per-assistant-message token accounting Claude Code records;
		// UsageSince sums it to track plan consumption. Absent on user/system entries.
		Usage *Usage `json:"usage"`
		// StopReason is the assistant message's terminal reason ("end_turn",
		// "tool_use", "max_tokens", …); subagentDone reads it to tell a finished
		// subagent transcript (end_turn) from one still mid-turn. Absent on
		// user/system entries.
		StopReason string `json:"stop_reason"`
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
	// Content is a tool_result's payload, kept raw because Claude Code writes it
	// either as a bare string or as an array of typed blocks. resultText()
	// reconciles both; it is what isLaunchAck reads to tell a subagent's spawn
	// acknowledgement from a real completion.
	Content json.RawMessage `json:"content"`
	// Input is the tool_use's arguments. For a Task/Agent tool_use it carries the
	// subagent's type and human description, which Tasks surfaces for the rich
	// subagent_spawn history events.
	Input struct {
		Description  string `json:"description"`
		SubagentType string `json:"subagent_type"`
		// RunInBackground is the Agent/Task input run_in_background flag.
		//
		// DO NOT make completion logic depend on this. Measured over 120
		// transcripts on this machine (2026-08-14): 69 Agent spawns, of which 68
		// carried no run_in_background at all and one carried an explicit false —
		// yet ALL 69 were answered with a spawn acknowledgement rather than a
		// result (see launchAckPrefixes). The flag has never been true for an
		// Agent spawn here, so any guard keyed on it silently never fires. It is
		// retained only as a best-effort tag; isLaunchAck is the load-bearing
		// signal.
		RunInBackground bool `json:"run_in_background"`
	} `json:"input"`
}

// launchAckPrefixes are the tool_result texts Claude Code returns to ACKNOWLEDGE
// a subagent spawn rather than to report its outcome. Both wordings are live in
// the corpus (49 "Spawned successfully" and 19 "Async agent launched
// successfully" over the 120 transcripts surveyed on 2026-08-14), so both must be
// recognized; the older one is not historical.
//
// An ack lands ~2s after the tool_use and says, in its own words, "The agent is
// working in the background. You will be notified automatically when it
// completes." The completion itself never arrives as a tool_result — it arrives
// as a <task-notification> user entry — so treating an ack as completion
// force-closes a subagent seconds after it starts. That is exactly the bug that
// made a session with four live background agents render idle.
//
// Matching is on the text prefix rather than on any structural field because no
// structural field distinguishes the two: the ack and a hypothetical real result
// are both plain tool_result blocks answering the same tool_use id.
var launchAckPrefixes = []string{
	"Async agent launched successfully",
	"Spawned successfully",
}

// resultText flattens a tool_result's content to the text it carries: a bare
// string as-is, an array of typed blocks as their text blocks joined by newline.
// Anything else (null, object, unparseable) yields "" — which reads as "not an
// ack", the fail-safe direction: a result we cannot classify is treated as a
// real result, and a real result that is wrongly excluded would only mean
// falling back to the subagent's own jsonl for completion.
func (b block) resultText() string {
	raw := bytes.TrimSpace(b.Content)
	if len(raw) == 0 {
		return ""
	}
	switch raw[0] {
	case '"':
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return ""
		}
		return s
	case '[':
		var bs []block
		if json.Unmarshal(raw, &bs) != nil {
			return ""
		}
		var parts []string
		for _, inner := range bs {
			if inner.Type == "text" && inner.Text != "" {
				parts = append(parts, inner.Text)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

// isLaunchAck reports whether this tool_result merely acknowledges a subagent
// spawn (see launchAckPrefixes) instead of reporting its outcome. Callers must
// never count an ack as the subagent's completion.
func (b block) isLaunchAck() bool {
	text := strings.TrimSpace(b.resultText())
	if text == "" {
		return false
	}
	for _, prefix := range launchAckPrefixes {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
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
//     output, so the awaited tool was approved and ran. This test is sound ONLY
//     because `since` is sampled at wall-clock hook-processing time
//     (AnchorSince): every assistant entry of the blocked turn itself — the
//     pre-prompt thinking/text and the pending tool_use's own message — is
//     generated before the PermissionRequest hook and so dated at-or-before
//     `since`, even though it flushes to disk after it (the H8 hazard,
//     docs/timing-hazards.md). Anchor `since` to the on-disk tail instead and
//     those late-flushed same-turn entries postdate it, falsely proving
//     resolution;
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

// BlockedEvidence is what one writer's OWN transcript tail says about whether it
// is still waiting on a dispatched tool. It exists for exactly one caller — the
// daemon's hydrate, which rebuilds prompt ownership from state.json after a
// restart (docs/subagent-permission-plan.md §9) — and it is deliberately shaped as
// a FALSIFIER, never a source.
//
// The asymmetry is the whole point. An unmatched tool_use means "a tool is
// dispatched and has not returned," which covers *awaiting approval* and
// *executing right now* with no third field to separate them, so deriving a
// pending prompt from it would raise a false RED on every session that happened to
// be mid-tool when the daemon restarted. But its ABSENCE — every dispatched tool
// in the window has its result — does prove the writer is no longer blocked: the
// tool returned, so whatever gate it sat behind opened. Applied only to ownership
// we already persisted, the check can only REMOVE, so its worst outcome is
// shortening a red, never inventing or missing one.
type BlockedEvidence int

const (
	// BlockedUnknown means the tail cannot answer: the file is missing, unreadable,
	// empty, or simply holds no tool_use at all (the pending one may have scrolled
	// past the window). The caller must KEEP the entry — this is the same
	// fail-closed reading permissionExit gives an unreadable transcript.
	BlockedUnknown BlockedEvidence = iota
	// BlockedYes means at least one tool_use in the tail has no matching
	// tool_result. The writer still has a tool in flight, so a prompt recorded
	// against it may well still be waiting. KEEP.
	BlockedYes
	// BlockedNo means the tail held tool_use blocks and EVERY one of them is
	// matched by a tool_result. The writer is demonstrably not waiting on a tool,
	// so a persisted prompt against it was answered — most likely while the daemon
	// was down, a window the Since := startup re-stamp would otherwise hide. DROP.
	BlockedNo
)

func (e BlockedEvidence) String() string {
	switch e {
	case BlockedYes:
		return "blocked"
	case BlockedNo:
		return "unblocked"
	default:
		return "unknown"
	}
}

// BlockedByPendingTool reports whether the writer that owns the transcript at
// `path` still has a tool dispatched and unanswered. Pass the writer's OWN file —
// SubagentPath(mainTranscript, agentID) — never the parent's.
//
// Two constraints, both measured, both a real missed RED if dropped
// (docs/subagent-permission-plan.md §9.7):
//
//   - It tests for ANY unmatched tool_use in the window, never the trailing one.
//     Claude Code emits parallel tool_use blocks from a single assistant message
//     routinely, so with a gated tool beside an auto-approved sibling the file
//     order is tool_use(gated), tool_use(sibling), tool_result(sibling): the
//     *trailing* tool_use is matched while the prompt still waits, and a
//     trailing-only test would drop a live red.
//   - It applies to the MAIN transcript exactly as it does to a subagent's. The
//     "Claude Code withholds the pending tool_use until it resolves" claim is
//     false — the main jsonl carries the pending tool_use within ~5 s of the hook
//     and keeps it unmatched for the whole wait (§9.7, V4). What it must NOT do is
//     cross the two: a subagent-raised prompt leaves the main tail fully matched,
//     so checking a subagent's entry against the parent file inverts the answer.
//
// The error is returned for the caller's logs only; every failure mode already
// maps to BlockedUnknown, which means keep.
func BlockedByPendingTool(path string, maxBytes int64) (BlockedEvidence, error) {
	entries, err := readTailEntries(path, maxBytes)
	if err != nil {
		return BlockedUnknown, err
	}
	// Collect the two sides across the WHOLE window before comparing. A result can
	// only follow its use, but the pairing is per-id and not per-line, so anything
	// that decides as it scans would be answering a different question.
	dispatched := map[string]bool{}
	answered := map[string]bool{}
	for _, e := range entries {
		for _, b := range e.blocks() {
			switch b.Type {
			case "tool_use":
				if b.ID != "" {
					dispatched[b.ID] = true
				}
			case "tool_result":
				if b.ToolUseID != "" {
					answered[b.ToolUseID] = true
				}
			}
		}
	}
	if len(dispatched) == 0 {
		// Nothing to falsify against. A truncated tail, a window that missed the
		// tool_use, or a writer that has genuinely dispatched nothing all land here,
		// and they are indistinguishable — so keep.
		return BlockedUnknown, nil
	}
	for id := range dispatched {
		if !answered[id] {
			return BlockedYes, nil
		}
	}
	return BlockedNo, nil
}

// taskToolNames are the tool_use names whose invocation spawns a subagent. Work
// done inside such a subagent is "work happening" for the delegating-green rule
// (docs/status-color-state-model.md §5 cases 5/14): a main thread that has ended
// its turn but still has an in-flight Task is delegating, not idle.
var taskToolNames = map[string]bool{"Task": true, "Agent": true}

// Task is a subagent the main thread launched via a Task/Agent tool_use, tagged
// with the metadata Claude Code stores in the tool_use input (the subagent type
// and the human description) and whether its tool_result has landed (Done). The
// daemon diffs the Task set across reconcile ticks to emit subagent_spawn/stop
// history events.
type Task struct {
	ID          string // the tool_use id (links spawn to stop)
	AgentType   string // subagent_type from the Task input (e.g. "Explore")
	Description string // the human description from the Task input
	Background  bool   // run_in_background from the Task input (a background fanout)
	Done        bool   // its tool_result has landed
}

// Tasks returns every subagent Task in the transcript tail, in launch order,
// each tagged Done if its tool_result has landed. Tail-bounded (maxBytes): a
// Task whose launching tool_use has scrolled out of the window is not reported.
// Returns a non-nil error only on I/O failure.
func Tasks(path string, maxBytes int64) ([]Task, error) {
	entries, err := readTailEntries(path, maxBytes)
	if err != nil {
		return nil, err
	}
	type meta struct {
		agentType, description string
		background             bool
	}
	launched := map[string]meta{}
	var order []string
	done := map[string]bool{}
	for _, e := range entries {
		for _, b := range e.blocks() {
			switch b.Type {
			case "tool_use":
				if taskToolNames[b.Name] && b.ID != "" {
					if _, seen := launched[b.ID]; !seen {
						order = append(order, b.ID)
					}
					launched[b.ID] = meta{b.Input.SubagentType, b.Input.Description, b.Input.RunInBackground}
				}
			case "tool_result":
				// A spawn ack is not a completion — see launchAckPrefixes. Without
				// this, every Agent fanout reads Done ~2s after it launches.
				if b.ToolUseID != "" && !b.isLaunchAck() {
					done[b.ToolUseID] = true
				}
			}
		}
	}
	tasks := make([]Task, 0, len(order))
	for _, id := range order {
		m := launched[id]
		tasks = append(tasks, Task{ID: id, AgentType: m.agentType, Description: m.description, Background: m.background, Done: done[id]})
	}
	return tasks, nil
}

// InFlightTasks counts the subagent Tasks the main thread has launched but not
// yet collected — the S dimension behind the delegating (green) status. The
// daemon reads it each reconcile tick to decide whether an idle main thread is
// actually delegating. It is Tasks filtered to the not-yet-Done; see there for
// the tail-bounding caveat. Returns a non-nil error only on I/O failure, letting
// the caller leave the last-known count rather than guess.
func InFlightTasks(path string, maxBytes int64) (int, error) {
	tasks, err := Tasks(path, maxBytes)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, t := range tasks {
		if !t.Done {
			n++
		}
	}
	return n, nil
}

// Usage is the billable accounting carried by one Claude assistant message.
// The legacy combined cache-creation total is retained while the two TTL
// buckets, request dimensions, and metered server tools remain distinct for
// accurate pricing. Cache reads typically dominate Claude Code sessions.
type Usage struct {
	InputTokens         int64  `json:"input_tokens"`
	OutputTokens        int64  `json:"output_tokens"`
	CacheReadTokens     int64  `json:"cache_read_input_tokens"`
	CacheCreationTokens int64  `json:"cache_creation_input_tokens"`
	CacheWrite5mTokens  int64  `json:"cache_write_5m_input_tokens,omitempty"`
	CacheWrite1hTokens  int64  `json:"cache_write_1h_input_tokens,omitempty"`
	ServiceTier         string `json:"service_tier,omitempty"`
	Speed               string `json:"speed,omitempty"`
	InferenceGeo        string `json:"inference_geo,omitempty"`
	WebSearchRequests   int64  `json:"web_search_requests,omitempty"`
	WebFetchRequests    int64  `json:"web_fetch_requests,omitempty"`
}

// IsZero reports whether no tokens were counted.
func (u Usage) IsZero() bool {
	return u.InputTokens == 0 && u.OutputTokens == 0 && u.CacheReadTokens == 0 &&
		u.CacheCreationTokens == 0 && u.CacheWrite5mTokens == 0 &&
		u.CacheWrite1hTokens == 0 && u.WebSearchRequests == 0 && u.WebFetchRequests == 0
}

func (u *Usage) add(o Usage) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.CacheReadTokens += o.CacheReadTokens
	u.CacheCreationTokens += o.CacheCreationTokens
	u.CacheWrite5mTokens += o.CacheWrite5mTokens
	u.CacheWrite1hTokens += o.CacheWrite1hTokens
	u.WebSearchRequests += o.WebSearchRequests
	u.WebFetchRequests += o.WebFetchRequests
	if o.ServiceTier != "" {
		u.ServiceTier = o.ServiceTier
	}
	if o.Speed != "" {
		u.Speed = o.Speed
	}
	if o.InferenceGeo != "" {
		u.InferenceGeo = o.InferenceGeo
	}
}

// UsageSince sums the token usage of assistant messages appended to the
// transcript at path since byteOffset, returning the summed delta and the new
// offset to resume from. Unlike the status readers it is NOT tail-bounded — it
// reads exactly the new bytes (cheap on a growing multi-MB transcript) and
// counts only complete, newline-terminated lines, so a line caught mid-write is
// re-read next call rather than double-counted. A file shorter than byteOffset
// (a /clear or session replacement truncated it) restarts from 0. The daemon
// emits a usage_sample history event from each non-zero delta and persists the
// returned offset per session.
func UsageSince(path string, byteOffset int64) (Usage, int64, error) {
	records, newOffset, err := UsageRecordsSince(path, byteOffset)
	if err != nil {
		return Usage{}, newOffset, err
	}
	var total Usage
	for _, record := range collapseUsageRecords(records) {
		total.add(record.Usage)
	}
	return total, newOffset, nil
}

// UsageSinceByModel is UsageSince broken down per model: it sums each new
// assistant message's tokens into a bucket keyed by its message.model, so the
// daemon can emit one usage_sample per model and price each at its own rate.
// Messages with no model land under the empty-string key. The offset,
// truncation, and partial-final-line semantics are identical to UsageSince (they
// share readNewLines), so either may be driven off the same per-session cursor.
// Returns a nil map when nothing new was appended.
func UsageSinceByModel(path string, byteOffset int64) (map[string]Usage, int64, error) {
	records, newOffset, err := UsageRecordsSince(path, byteOffset)
	if err != nil || len(records) == 0 {
		return nil, newOffset, err
	}
	byModel := map[string]Usage{}
	for _, record := range collapseUsageRecords(records) {
		u := byModel[record.Model]
		u.add(record.Usage)
		byModel[record.Model] = u
	}
	return byModel, newOffset, nil
}

// readNewLines returns the complete (newline-terminated) bytes appended to path
// since byteOffset, plus the offset to resume from — the shared, careful tail
// logic behind UsageSince/UsageSinceByModel. It reads exactly the new bytes
// (cheap on a growing multi-MB transcript), drops a line caught mid-write (the
// trailing partial is excluded and re-read next call rather than double-counted),
// and restarts from 0 when the file is shorter than byteOffset (a /clear or
// session replacement truncated it). Returns (nil, byteOffset, nil) when nothing
// complete is new, and (nil, byteOffset, err) only on I/O failure.
func readNewLines(path string, byteOffset int64) ([]byte, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, byteOffset, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, byteOffset, err
	}
	size := fi.Size()
	if size < byteOffset {
		byteOffset = 0 // truncated/replaced transcript
	}
	if size == byteOffset {
		return nil, byteOffset, nil
	}
	if _, err := f.Seek(byteOffset, io.SeekStart); err != nil {
		return nil, byteOffset, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, byteOffset, err
	}
	lastNL := bytes.LastIndexByte(data, '\n')
	if lastNL < 0 {
		return nil, byteOffset, nil // no complete line appended yet
	}
	complete := data[:lastNL+1]
	return complete, byteOffset + int64(len(complete)), nil
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

// AnchorSince picks the time a status transition should be dated from
// (StatusSince), given the wall-clock instant `now` the daemon processed the
// triggering hook, the chip's PREVIOUS StatusSince (`prev`, the clamp floor —
// see below), and whether the transition is into working. There are two
// opposite clock-skew risks, one per direction — see docs/timing-hazards.md:
//
//   - Into working: the hook reaches us AFTER Claude wrote the entry that
//     triggered it, so a wall-clock `now` sits ahead of that entry and would
//     filter a fast follow-up signal (an immediate Ctrl+C after a prompt) as
//     stale. Pull StatusSince back to the triggering entry (AnchorTime) so a
//     genuinely-later signal always reads as later, regardless of hook latency
//     (H1). Nothing here can misread the turn's own content: a working chip is
//     only ever DEMOTED, and only by the interrupt marker, which the turn never
//     writes on its own.
//
//   - Into idle (Stop/SessionStart) and into permission (PermissionRequest): the
//     only signal that should flip the chip is one dated AFTER the hook's cause
//     (the turn ending; the prompt being raised). But the turn's OWN entries are
//     dated at generation and flushed to the .jsonl a beat AFTER the hook reaches
//     us — the completing turn's final assistant message lands after its Stop,
//     and the blocked turn's pre-prompt thinking/text (and the pending tool_use
//     entry itself) land after its PermissionRequest. Anchoring to "the newest
//     turn entry on disk at hook time" lands BEFORE those late-flushed entries,
//     which the reconciler then reads as "activity after idle" (falsely
//     re-greening an ended turn — H7) or "assistant message after the prompt →
//     resolved" (falsely releasing a still-pending red chip, a missed-RED — H8,
//     the costliest error in the model). Wall-clock `now` is the race-free
//     anchor: the hook fires only after its cause, so every entry of the turn so
//     far — whenever it flushes — is dated at-or-before `now` and cannot
//     re-trigger, while a genuine later signal (a resumption; a post-approval
//     assistant message) is dated after `now` and still does.
//
// The pull-back never runs `now` backward: it applies only when the anchor is
// strictly before `now`, and a missing/unreadable transcript falls back to `now`.
//
// It is also floored at `prev`, the chip's StatusSince before this edge, so the
// result is max(anchor, prev) (itself capped at `now`). H1 wants an anchor
// slightly behind `now` — the entry that caused THIS hook. It does not want an
// arbitrarily old one, and `path` can supply exactly that: a subagent's hooks
// are attributed to the parent session and carry the PARENT's transcript_path,
// so when a teammate's PostToolUse drives the edge, the newest entry in the main
// transcript is whatever the main thread last wrote — possibly minutes ago,
// possibly never during this turn. Dating the edge from it makes StatusSince
// arbitrarily stale, which in turn defeats every "has it been in this state long
// enough?" damper downstream: the idle-title demotion's freshness gate and its
// IdleTitleGrace both go trivially true, so a chip re-greened one second ago is
// demoted on the very next reconcile tick and the pair oscillates
// (docs/subagent-permission-oscillation.md §3.3, §3.4 — a 13-minute-stale
// anchor drove a 95-second orange/green limit cycle).
//
// The floor keeps H1's whole benefit — an anchor between `prev` and `now` is
// still a genuine same-turn pull-back and is returned unchanged — while bounding
// the staleness by the one timestamp that is definitionally not stale: where the
// chip already sat. StatusSince then never moves backward across an edge, so
// each state gets its full grace period. A zero `prev` (the first transition an
// AgentInfo ever makes) is a no-op: no real transcript timestamp precedes it.
func AnchorSince(path string, now, prev time.Time, intoWorking bool, maxBytes int64) time.Time {
	if !intoWorking {
		return now
	}
	anchor, ok := AnchorTime(path, maxBytes)
	if !ok || !anchor.Before(now) {
		return now
	}
	if anchor.Before(prev) {
		// Floor at prev, but never past `now` — a prev at or ahead of the hook's
		// wall clock (skew, a restored chip) must not date the edge into the future.
		if prev.Before(now) {
			return prev
		}
		return now
	}
	return anchor
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
