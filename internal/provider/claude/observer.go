// Package claude adapts Claude Code's hook and append-only fanout evidence to
// the provider-neutral agent graph without sharing Claude-specific inference
// with other providers or the neutral reducer.
package claude

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/fanout"
	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/provider"
	"github.com/tjmisko/switchboard/internal/statustune"
	"github.com/tjmisko/switchboard/internal/transcript"
)

var (
	// ErrClosed is returned after Close. Close itself is idempotent.
	ErrClosed = errors.New("claude observer is closed")
	// ErrWrongProvider rejects accidental routing of a non-Claude root.
	ErrWrongProvider = errors.New("claude observer received a non-Claude root")
	// ErrMissingSession rejects CWD-based or otherwise heuristic binding. Claude's
	// exact session ID is the graph root identity.
	ErrMissingSession = errors.New("claude observer requires an exact session id")
	// ErrSuperseded means Forget or a session rotation replaced the root while an
	// out-of-lock transcript scan was in flight. Callers may simply observe again.
	ErrSuperseded = errors.New("claude observation was superseded")
)

const defaultFreshness = 15 * time.Second

// maxPendingPromptsPerWriter bounds one writer's open prompt set so a leak
// cannot grow without limit. It is a backstop, NOT a model of dispatch width:
// the widest parallel turn in the measured corpus is 8
// (askuserquestion-model-plan.md §2.2), but that is an observed maximum, not a
// limit Claude Code enforces, and nothing in the emitter caps parallel tool_use.
// Setting the ceiling AT the observed maximum would make the first 9-way turn
// evict a live prompt from that same turn — the surviving 8 get answered, the
// set empties, and the chip goes green while the evicted call still blocks the
// agent. That is a missed RED, silent and costing the whole remaining wait, and
// eviction cannot tell it apart from the leak this bound exists to catch.
//
// So the ceiling is set far outside anything observed. A PendingPrompt is four
// words; 32 costs nothing and buys 4x headroom over the corpus. Overflow past
// that is no longer plausibly one turn's parallel dispatch, and the OLDEST
// record is dropped: it is the one most likely to be a leak — a prompt whose
// clear we already missed (an unmatched hash, a lost PostToolUse) — and the one
// writer_stale_backstop was about to sweep anyway, while the newest is the one
// most likely still blocking the agent right now.
const maxPendingPromptsPerWriter = 32

// HookSignal is the provider-owned hook envelope C6 translates the existing RPC
// payload into. ToolInputHash is a correlator only; raw tool input is never
// accepted, retained, diagnosed, or projected.
type HookSignal struct {
	Root          provider.RootRef
	Event         string
	AgentID       string
	AgentType     string
	ToolName      string
	ToolInputHash string
	// ToolUseID is Claude Code's exact identity for the call the edge is about.
	// It is present on the tool events and absent on PermissionRequest
	// (docs/claude-code-hook-schema.md §2), which is why a prompt cannot be opened
	// with one and has to latch it from the transcript instead (PendingCall).
	// Unlike ToolInputHash it names a CALL and not a call SHAPE, so two writers
	// running byte-identical commands are distinguishable by it and a rewritten
	// input is not.
	ToolUseID string
	At        time.Time
}

// HookResult is a detached post-hook view suitable for shadow comparison and a
// compatibility projection. Rule is a finite, content-free decision name.
type HookResult struct {
	Applied bool
	Changed bool
	Rule    string
	// PromptDepth is how many prompts the signal's writer still holds open after
	// an edge that touches prompt ownership (PermissionRequest, PostToolUse), and
	// zero on every other edge. It is a small bounded integer, never content: it
	// exists so the P1 probe can size how often one writer really blocks on two
	// calls at once (askuserquestion-model-plan.md §5).
	PromptDepth int
	Root        provider.RootKey
	Observation agentgraph.Observation
	Projection  Compatibility
}

// Option customizes an Observer at construction time.
type Option func(*Observer)

// WithFreshness changes the half-open observation freshness window. A
// non-positive duration restores the default.
func WithFreshness(freshness time.Duration) Option {
	return func(o *Observer) {
		if freshness > 0 {
			o.freshness = freshness
		}
	}
}

// WithFanoutObserver injects a fanout observer, primarily for coordinated
// migration tests. Nil leaves the constructor-owned observer in place.
func WithFanoutObserver(observer *fanout.Observer) Option {
	return func(o *Observer) {
		if observer != nil {
			o.fanout = observer
		}
	}
}

// WithTuning keeps the compatibility backstops aligned with the existing
// daemon state machine. It must be supplied before concurrent use.
func WithTuning(tuning statustune.Tuning) Option {
	return func(o *Observer) { o.tuning = tuning }
}

// Observer fuses hook edges with Claude's transcript/fanout artifact observer.
// Its lock protects in-memory transitions only. All filesystem reads occur in
// Observe before the result is merged under the lock.
type Observer struct {
	mu        sync.Mutex
	roots     map[provider.RootKey]*rootState
	updates   *provider.InvalidationQueue
	fanout    *fanout.Observer
	freshness time.Duration
	tuning    statustune.Tuning
	closed    bool
}

type rootState struct {
	ref       provider.RootRef
	runtime   agentgraph.RuntimeState
	runtimeAt time.Time
	// pending maps a writer ("" is the main thread) to every prompt that writer
	// currently holds open, ordered oldest-first by Since. The writer routes
	// evidence — no sibling may ever clear another writer's prompt — but the
	// individual call closes the prompt, so one writer's parallel calls must not
	// collapse onto one record. A writer with no open prompt has no key at all;
	// an empty slice is never stored.
	pending  map[string][]PendingPrompt
	overlays map[string]childOverlay
	// resolvedRule names the transcript rule the most recent Observe actually
	// applied, held only until the coordinator drains it onto the transition that
	// resolution produced (DrainResolutionRule). It is the Observe-tick half of
	// HookResult.Rule: without it every red released by the transcript is recorded
	// as the blanket "the graph said so", which is what made the 17s stale red
	// invisible in the history for a week (docs/attention-latency-report.md §2.3).
	// A content-free rule id, never a writer, a tool or a path.
	resolvedRule string
	// promptDiagnostics are the bounded categories the most recent Observe tick's
	// call-identity latch wants counted, held only until the coordinator drains
	// them onto its diagnostic table (DrainPromptDiagnostics). They answer "is the
	// fast path actually reachable in practice?" — how often a prompt binds an id,
	// how often the tail is too ambiguous to try, and whether the uniqueness the
	// lifted fanout floor rests on ever fails. Categories only, never a writer, a
	// tool, a call id or a path.
	promptDiagnostics []string

	fanout        fanout.Snapshot
	known         map[string]fanout.Lifecycle
	retained      map[string]fanout.Child
	hasFanout     bool
	turnStartedAt time.Time
	priorSummary  agentgraph.Summary
	observation   agentgraph.Observation
	projection    Compatibility
	legacyEvents  []history.Event
}

type childOverlay struct {
	AgentType string
	Runtime   agentgraph.RuntimeState
	UpdatedAt time.Time
}

// promptResolution is transcript evidence about a writer, not about one call:
// "this writer resumed", "this writer is terminal", "this writer has been quiet
// past the cap". Such evidence closes every prompt the writer held. Prompts
// carries the exact open set the scan reasoned about so the merge can detect a
// hook that opened or closed a prompt while the I/O was in flight.
type promptResolution struct {
	Writer  string
	Prompts []PendingPrompt
	// Closed is the subset of Prompts this resolution actually retires. It is all
	// of them for a whole-file rule, and exactly the id-matched ones for a
	// call-scoped rule — a writer whose answered question left another call still
	// executing must keep its red, so the resolution has to be able to say "these
	// two, not the set".
	Closed  []PendingPrompt
	Runtime agentgraph.RuntimeState
	Rule    string
}

// promptLatch is one prompt's call-identity outcome from an Observe tick's
// transcript read, carried out of the I/O so it can be merged under the lock.
// The prompt is addressed by identity rather than by index because a hook may
// have reordered or closed prompts while the scan was in flight.
type promptLatch struct {
	Writer   string
	Identity promptIdentity
	CallID   string
	State    CallLatchState
}

// The bounded diagnostic categories the call-identity latch reports.
const (
	// DiagnosticCallLatched — a prompt bound the exact call it gates. The
	// numerator of "is the id fast path reachable?".
	DiagnosticCallLatched = "prompt_call_latched"
	// DiagnosticCallAmbiguous — a prompt's writer had two or more indistinguishable
	// unmatched calls of that tool, so it stays on the (tool, hash) rule forever.
	// The denominator: a high rate here means the fast path is mostly theory.
	DiagnosticCallAmbiguous = "prompt_call_ambiguous"
	// DiagnosticCallIDCollision — P3's runtime half
	// (askuserquestion-model-plan.md §5): two writers of one session appeared to
	// claim the same tool_use id. The lifted fanout floor rests on this never
	// happening, so it is counted rather than assumed, and the colliding id is
	// refused rather than bound.
	DiagnosticCallIDCollision = "prompt_call_id_collision"
)

// maxPromptDiagnosticsPerRoot bounds the undrained buffer. A tick can only emit
// a few of these — the latch state is one-shot per prompt and prompts are capped
// per writer — but a root nobody drains must not grow without limit.
const maxPromptDiagnosticsPerRoot = 64

// NewObserver constructs one process-wide Claude observer. historyDir is used
// only by fanout's exact-once legacy event seen sets; tests should pass a
// temporary directory, and production should pass the configured history dir.
func NewObserver(historyDir string, options ...Option) *Observer {
	o := &Observer{
		roots:     make(map[provider.RootKey]*rootState),
		updates:   provider.NewInvalidationQueue(64),
		fanout:    fanout.NewObserver(historyDir),
		freshness: defaultFreshness,
		tuning:    statustune.Default(),
	}
	for _, option := range options {
		if option != nil {
			option(o)
		}
	}
	return o
}

// Observe implements provider.Observer. It reads fanout and transcript evidence
// outside the adapter lock, then rejects a result if Forget/session rotation
// superseded the root while I/O was in flight.
func (o *Observer) Observe(ctx context.Context, root provider.RootRef, now time.Time) (agentgraph.Observation, error) {
	if err := validateRoot(root); err != nil {
		return agentgraph.Observation{}, err
	}
	if err := ctx.Err(); err != nil {
		return agentgraph.Observation{}, err
	}
	if now.IsZero() {
		now = time.Now()
	}

	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return agentgraph.Observation{}, ErrClosed
	}
	rs := o.ensureRootLocked(root)
	pending := clonePendingSets(rs.pending)
	runtime, runtimeAt := rs.runtime, rs.runtimeAt
	tuning := o.tuning
	o.mu.Unlock()

	// Provider I/O is deliberately outside every state lock. fanout.Observer has
	// its own cursor lock and returns detached data.
	structured, scanErr := o.fanout.Observe(fanout.Root{
		SessionID: root.ProviderSessionID, Transcript: root.Transcript,
		PID: root.PID, Agent: string(agentgraph.ProviderClaude), CWD: root.CWD,
	}, now)
	latches, latchDiagnostics := latchPendingCalls(root.Transcript, pending, tuning.TailBytes)
	resolutions := resolvePending(root.Transcript, pending, structured.Snapshot, now, tuning)
	runtime = reconcileRootRuntime(root.Transcript, runtime, runtimeAt, tuning.TailBytes)

	if err := ctx.Err(); err != nil {
		return agentgraph.Observation{}, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return agentgraph.Observation{}, ErrClosed
	}
	if current := o.roots[root.Key()]; current != rs || current.ref.ProviderSessionID != root.ProviderSessionID {
		return agentgraph.Observation{}, ErrSuperseded
	}
	rs.ref = root
	if runtime != agentgraph.RuntimeUnknown {
		if runtime != rs.runtime {
			rs.runtimeAt = now
		}
		rs.runtime = runtime
	}
	if len(structured.Events) > 0 {
		rs.legacyEvents = append(rs.legacyEvents, structured.Events...)
	}
	if !rs.observation.ObservedAt.IsZero() && now.Before(rs.observation.ObservedAt) {
		return rs.observation.Clone(), ErrSuperseded
	}
	// The rule is this tick's, and only this tick's: a resolution that was fenced
	// out below explains nothing, and a rule left over from an earlier tick would
	// mislabel whatever transition happens to land next.
	rs.resolvedRule = ""
	// Latches are merged BEFORE the resolutions, and for a mechanical reason: the
	// resolution fence compares the writer's live set against the one the scan
	// reasoned about, and the scan reasoned about the LATCHED clone. Merging the
	// same latches here first is what keeps that comparison honest — a set that
	// still differs afterwards really was changed by a hook.
	for _, latch := range latches {
		applyPromptLatch(rs.pending, latch)
	}
	if len(latchDiagnostics) > 0 {
		rs.promptDiagnostics = append(rs.promptDiagnostics, latchDiagnostics...)
		if len(rs.promptDiagnostics) > maxPromptDiagnosticsPerRoot {
			rs.promptDiagnostics = rs.promptDiagnostics[len(rs.promptDiagnostics)-maxPromptDiagnosticsPerRoot:]
		}
	}
	for _, resolution := range resolutions {
		current, ok := rs.pending[resolution.Writer]
		if !ok || !slices.Equal(current, resolution.Prompts) {
			continue // a newer hook opened or closed a prompt while the scan ran
		}
		// A whole-file rule closes the writer's whole set; a call-scoped one closes
		// exactly the prompts whose own result landed. Either way the writer's entry
		// survives while anything is still open: red leaves at len == 0 and nowhere
		// else.
		remaining := slices.DeleteFunc(slices.Clone(current), func(prompt PendingPrompt) bool {
			return slices.Contains(resolution.Closed, prompt)
		})
		setPendingPrompts(rs.pending, resolution.Writer, remaining)
		if rs.resolvedRule == "" {
			// resolutions is sorted by writer and the main thread's key is "", so
			// when several writers resolve on one tick the main thread's rule is the
			// one kept. It is the writer whose resolution most often flips the chip,
			// and one id per transition is all the record has room for.
			rs.resolvedRule = resolution.Rule
		}
		if len(remaining) > 0 {
			// The writer is still blocked, so the exit runtime this resolution
			// carries describes one closed call rather than the end of the wait.
			// Applying it here would date the chip's exit from a wait still running.
			continue
		}
		if resolution.Writer == "" {
			rs.runtime = resolution.Runtime
			rs.runtimeAt = now
		} else if overlay, ok := rs.overlays[resolution.Writer]; ok {
			overlay.Runtime, overlay.UpdatedAt = resolution.Runtime, now
			rs.overlays[resolution.Writer] = overlay
		}
	}
	if scanErr == nil {
		o.mergeFanoutLocked(rs, structured.Snapshot)
		if clearTerminalPrompts(rs, structured.Snapshot.ObservedAt) && rs.resolvedRule == "" {
			// The same reason resolvePending gives, reached by the other door: this
			// sweep runs against the freshly merged snapshot, so it retires a
			// terminal child's prompt that the pre-merge scan could not see.
			rs.resolvedRule = statustune.RuleGraphChildTerminal
		}
	}
	observation, err := o.rebuildLocked(rs, now, agentgraph.SourceClaudeTranscript, scanErr == nil && structured.Snapshot.Complete)
	if err != nil {
		return observation, err
	}
	if scanErr != nil {
		// Preserve the last graph as a partial, newly-dated observation while making
		// the I/O failure visible to orchestration. Legacy Reconcile likewise holds
		// its last count rather than replacing it with a guessed zero.
		return observation, scanErr
	}
	return observation.Clone(), nil
}

// ApplyHook ingests one exact Claude hook edge. It performs no filesystem I/O;
// transcript reconciliation happens on Observe. Child hook activity never
// changes the root runtime, except that child attention participates in the
// shared reducer just like every other live-node attention state.
func (o *Observer) ApplyHook(signal HookSignal) HookResult {
	if err := validateRoot(signal.Root); err != nil {
		return HookResult{Root: signal.Root.Key()}
	}
	if signal.At.IsZero() {
		signal.At = time.Now()
	}
	signal.AgentID = normalizeAgentID(signal.AgentID)
	if !recognizedHook(signal.Event) {
		return HookResult{Root: signal.Root.Key()}
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return HookResult{Root: signal.Root.Key()}
	}
	rs := o.ensureRootLocked(signal.Root)
	changed, rule, depth := o.applyHookLocked(rs, signal)
	observation, _ := o.rebuildLocked(rs, signal.At, agentgraph.SourceHook, rs.hasFanout && rs.fanout.Complete)
	o.updates.Signal(signal.Root.Key())
	return HookResult{
		Applied: true, Changed: changed, Rule: rule, PromptDepth: depth, Root: signal.Root.Key(),
		Observation: observation.Clone(), Projection: rs.projection.Clone(),
	}
}

func (o *Observer) applyHookLocked(rs *rootState, signal HookSignal) (bool, string, int) {
	writer := signal.AgentID
	changed := false
	rule := statustune.RuleGraphHookNoChange

	if signal.Event == "PermissionRequest" {
		next := PendingPrompt{
			Tool: signal.ToolName, InputHash: signal.ToolInputHash,
			Attention: attentionForTool(signal.ToolName), Since: signal.At,
		}
		prompts, opened := openPendingPrompt(rs.pending[writer], next)
		setPendingPrompts(rs.pending, writer, prompts)
		if writer != "" {
			overlay := rs.overlays[writer]
			overlay.AgentType = firstNonempty(signal.AgentType, overlay.AgentType)
			overlay.Runtime = agentgraph.RuntimeIdle
			overlay.UpdatedAt = signal.At
			rs.overlays[writer] = overlay
		}
		return opened, statustune.RuleGraphPermissionRecorded, len(prompts)
	}

	depth := 0
	switch signal.Event {
	case "SessionStart":
		if rs.runtime != agentgraph.RuntimeIdle {
			rs.runtime, rs.runtimeAt, changed = agentgraph.RuntimeIdle, signal.At, true
		}
		rule = statustune.RuleGraphSessionStarted
	case "UserPromptSubmit":
		if writer == "" {
			rs.turnStartedAt = signal.At
			clear(rs.retained)
			if rs.runtime != agentgraph.RuntimeActive {
				changed = true
			}
			rs.runtime, rs.runtimeAt = agentgraph.RuntimeActive, signal.At
			rule = statustune.RuleGraphPromptSubmitted
		} else {
			changed = setChildRuntime(rs, writer, signal.AgentType, agentgraph.RuntimeActive, signal.At)
			rule = statustune.RuleGraphChildActivity
		}
	case "PostToolUse":
		// The call closes the prompt, so a match removes exactly the prompt it
		// names and leaves the writer's other open calls blocking. Answering one
		// of two parallel calls must not take the chip out of red.
		prompts := rs.pending[writer]
		index := matchingPromptIndex(prompts, signal)
		// The fanout floor: an empty agent_id with teammates in flight might be
		// the main thread OR a teammate whose hook lost its writer, and a
		// (tool, hash) match cannot tell them apart — the hash names a call SHAPE,
		// and fanned-out teammates run byte-identical commands routinely. An ID
		// match can: tool_use ids are per-call and are never claimed by two
		// writers (pinned by TestNoCallIDShouldBeClaimedByTwoWritersAcrossOneSession,
		// and counted at runtime as prompt_call_id_collision), so a teammate's
		// completion carries an id that simply is not this prompt's and is held by
		// promptMatches before the floor is ever consulted. The floor therefore
		// buys nothing on an id match and costs the whole latency this phase
		// exists to remove, so it applies to shape matches only.
		if index >= 0 && (idMatched(prompts[index], signal) || !(writer == "" && rs.fanout.InFlight > 0)) {
			rule = statustune.RuleGraphToolMatchCleared
			if idMatched(prompts[index], signal) {
				rule = statustune.RuleGraphCallMatchCleared
			}
			setPendingPrompts(rs.pending, writer, closePendingPromptAt(prompts, index))
			changed = true
		} else if len(prompts) > 0 {
			rule = statustune.RuleGraphPromptHeld
			if callMismatchHeld(prompts, signal) {
				rule = statustune.RuleGraphCallMismatchHeld
			}
		} else {
			rule = statustune.RuleGraphNonOwnerHeld
		}
		depth = len(rs.pending[writer])
		if writer == "" {
			if rs.runtime != agentgraph.RuntimeActive {
				changed = true
			}
			rs.runtime, rs.runtimeAt = agentgraph.RuntimeActive, signal.At
		} else if setChildRuntime(rs, writer, signal.AgentType, agentgraph.RuntimeActive, signal.At) {
			changed = true
		}
	case "Stop":
		if writer == "" {
			if rs.runtime != agentgraph.RuntimeIdle {
				changed = true
			}
			rs.runtime, rs.runtimeAt = agentgraph.RuntimeIdle, signal.At
			rule = statustune.RuleGraphRootStopped
		} else {
			changed = setChildRuntime(rs, writer, signal.AgentType, agentgraph.RuntimeIdle, signal.At)
			rule = statustune.RuleGraphChildActivity
		}
	case "SubagentStart", "SubagentStop":
		// Directory/journal scan remains authoritative for spawn and completion.
		// These hooks only invalidate the cached snapshot.
		rule = statustune.RuleGraphFanoutRescan
	}
	return changed, rule, depth
}

// openPendingPrompt inserts next into a writer's open set, keeping it ordered
// oldest-first by Since, and reports whether the set actually changed. A hook
// redelivered verbatim — same tool, correlator and instant — is the same prompt
// rather than a second one, so it is deduped: a retry must not inflate the open
// set into a red nothing can ever clear.
func openPendingPrompt(prompts []PendingPrompt, next PendingPrompt) ([]PendingPrompt, bool) {
	// Dedupe on IDENTITY, not on the whole struct. An open prompt accumulates
	// latch state the redelivered edge cannot carry (the hook has no
	// tool_use_id), so a whole-struct compare would stop recognizing a prompt the
	// moment it bound its call id and let the retry open a second red that
	// nothing can ever clear.
	if slices.ContainsFunc(prompts, func(prompt PendingPrompt) bool {
		return prompt.identity() == next.identity()
	}) {
		return prompts, false
	}
	index := sort.Search(len(prompts), func(i int) bool { return prompts[i].Since.After(next.Since) })
	opened := slices.Insert(slices.Clip(prompts), index, next)
	if len(opened) > maxPendingPromptsPerWriter {
		opened = opened[len(opened)-maxPendingPromptsPerWriter:]
	}
	return opened, true
}

// closePendingPromptAt removes exactly one prompt, never the writer's entry.
func closePendingPromptAt(prompts []PendingPrompt, index int) []PendingPrompt {
	return slices.Delete(slices.Clone(prompts), index, index+1)
}

// setPendingPrompts stores a writer's open set, dropping the key entirely once
// the set is empty. Callers rely on `len(pending)` counting blocked writers, so
// an empty slice must never be left behind.
func setPendingPrompts(pending map[string][]PendingPrompt, writer string, prompts []PendingPrompt) {
	if len(prompts) == 0 {
		delete(pending, writer)
		return
	}
	pending[writer] = prompts
}

// matchingPromptIndex returns the prompt the signal resolves, or -1.
//
// An exact call-id match wins outright, wherever it sits in the set: it is the
// one candidate that is not a guess, and taking an older SHAPE match ahead of it
// would close a prompt the signal says nothing about while leaving the answered
// one red — and, when the fanout floor is in play, would refuse the clear
// altogether because the prompt it landed on has no id to lift the floor with.
//
// Failing that, the oldest shape match is taken: prompts are answered in the
// order they were presented, and it leaves the newest Since in place, so the
// backstop clock runs from the most recent evidence — the stale-red direction
// rather than the missed-red one.
func matchingPromptIndex(prompts []PendingPrompt, signal HookSignal) int {
	shape := -1
	for i, prompt := range prompts {
		if !promptMatches(prompt, signal) {
			continue
		}
		if idMatched(prompt, signal) {
			return i
		}
		if shape < 0 {
			shape = i
		}
	}
	return shape
}

// foldPromptAttention reduces a writer's open prompts to the one attention its
// node carries, using the neutral reducer's own precedence (approval ahead of
// user_input) so a node folds the same way the summary does.
func foldPromptAttention(prompts []PendingPrompt) agentgraph.AttentionState {
	folded := agentgraph.AttentionNone
	for _, prompt := range prompts {
		if prompt.Attention == agentgraph.AttentionApproval {
			return agentgraph.AttentionApproval
		}
		if prompt.Attention == agentgraph.AttentionUserInput {
			folded = agentgraph.AttentionUserInput
		}
	}
	return folded
}

// newestPromptSince is the instant the writer's most recent prompt opened. Every
// writer-scoped backstop dates from it rather than from the oldest prompt: an
// assistant message between two parallel prompts is usually the turn that
// dispatched the second one, so resolving the older prompt against it would
// clear a red the agent is still blocked on.
func newestPromptSince(prompts []PendingPrompt) time.Time {
	if len(prompts) == 0 {
		return time.Time{}
	}
	return prompts[len(prompts)-1].Since
}

func (o *Observer) mergeFanoutLocked(rs *rootState, snapshot fanout.Snapshot) {
	initial := !rs.hasFanout
	var initialTerminals []fanout.Child
	for _, child := range snapshot.Children {
		previous, known := rs.known[child.ID]
		rs.known[child.ID] = child.Lifecycle
		switch {
		case !child.LifecycleTerminal():
			delete(rs.retained, child.ID)
		case known && !lifecycleTerminal(previous):
			rs.retained[child.ID] = child
		case !known && initial:
			initialTerminals = append(initialTerminals, child)
		case !known && (rs.turnStartedAt.IsZero() || !child.SpawnedAt.Before(rs.turnStartedAt)):
			rs.retained[child.ID] = child
		}
	}
	if initial && len(initialTerminals) > 0 {
		if !rs.turnStartedAt.IsZero() {
			for _, child := range initialTerminals {
				if !child.SpawnedAt.Before(rs.turnStartedAt) {
					rs.retained[child.ID] = child
				}
			}
			initialTerminals = nil
		}
	}
	if initial && len(initialTerminals) > 0 {
		// On restart, append-only artifacts cannot prove turn membership. Retain
		// only the latest completion instant as the bounded most-recent cohort.
		latest := initialTerminals[0].CompletedAt
		for _, child := range initialTerminals[1:] {
			if child.CompletedAt.After(latest) {
				latest = child.CompletedAt
			}
		}
		for _, child := range initialTerminals {
			if child.CompletedAt.Equal(latest) {
				rs.retained[child.ID] = child
			}
		}
	}
	rs.fanout = snapshot.Clone()
	rs.hasFanout = true
}

// clearTerminalPrompts retires the prompts of children the merged snapshot shows
// as finished, and reports whether it retired any — the caller records that as
// the reason the red closed.
func clearTerminalPrompts(rs *rootState, observedAt time.Time) bool {
	retired := false
	for _, child := range rs.fanout.Children {
		prompts := rs.pending[child.ID]
		if !child.LifecycleTerminal() || len(prompts) == 0 {
			continue
		}
		// A prompt opened after the scan is newer evidence than the terminal
		// lifecycle the scan saw, so it survives; the rest are retired per prompt.
		kept := slices.DeleteFunc(slices.Clone(prompts), func(prompt PendingPrompt) bool {
			return !prompt.Since.After(observedAt)
		})
		if len(kept) != len(prompts) {
			retired = true
		}
		setPendingPrompts(rs.pending, child.ID, kept)
	}
	return retired
}

func (o *Observer) rebuildLocked(rs *rootState, now time.Time, source agentgraph.SourceKind, complete bool) (agentgraph.Observation, error) {
	nodes := []agentgraph.Node{{
		ID: rs.ref.ProviderSessionID, Runtime: rs.runtime, Attention: foldPromptAttention(rs.pending[""]),
		Lifecycle: agentgraph.LifecycleRunning, StartedAt: rs.ref.StartedAt,
		UpdatedAt: rs.runtimeAt,
	}}

	byID := make(map[string]fanout.Child, len(rs.fanout.Children)+len(rs.retained))
	for _, child := range rs.fanout.Children {
		if !child.LifecycleTerminal() {
			byID[child.ID] = child
		}
	}
	for id, child := range rs.retained {
		byID[id] = child
	}
	for writer, prompts := range rs.pending {
		if writer == "" {
			continue
		}
		child, exists := byID[writer]
		if !exists || (child.LifecycleTerminal() && newestPromptSince(prompts).After(rs.fanout.ObservedAt)) {
			byID[writer] = fanout.Child{ID: writer, Lifecycle: fanout.LifecycleRunning}
		}
	}

	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		child := byID[id]
		node := graphNode(rs.ref.ProviderSessionID, child)
		if overlay, ok := rs.overlays[id]; ok {
			node.Role = firstNonempty(node.Role, overlay.AgentType)
			if !node.Lifecycle.Terminal() && (node.UpdatedAt.IsZero() || !overlay.UpdatedAt.Before(node.UpdatedAt)) {
				node.Runtime = overlay.Runtime
				node.UpdatedAt = overlay.UpdatedAt
			}
		}
		if prompts := rs.pending[id]; len(prompts) > 0 && !node.Lifecycle.Terminal() {
			node.Attention = foldPromptAttention(prompts)
		}
		nodes = append(nodes, node)
	}

	observation := agentgraph.Observation{
		Provider: agentgraph.ProviderClaude, RootID: rs.ref.ProviderSessionID,
		Nodes: nodes, Source: source, ObservedAt: now,
		FreshUntil: now.Add(o.freshness), Complete: complete,
	}
	normalized, err := agentgraph.Normalize(observation)
	if err != nil {
		return normalized, err
	}
	summary := agentgraph.Reduce(normalized, rs.priorSummary, now)
	rs.priorSummary = summary
	rs.observation = normalized.Clone()
	rs.projection = projectCompatibility(rs, summary)
	return normalized.Clone(), nil
}

func graphNode(rootID string, child fanout.Child) agentgraph.Node {
	lifecycle := graphLifecycle(child.Lifecycle)
	parentID := child.ParentID
	if parentID == "" {
		parentID = rootID
	}
	description := child.Description
	if description == "" {
		description = child.WorkflowName
	}
	runtime := agentgraph.RuntimeActive
	switch child.Lifecycle {
	case fanout.LifecyclePending:
		runtime = agentgraph.RuntimeNotLoaded
	case fanout.LifecycleCompleted, fanout.LifecycleInterrupted:
		runtime = agentgraph.RuntimeIdle
	}
	return agentgraph.Node{
		ID: child.ID, ParentID: parentID, Nickname: child.Nickname,
		Role: child.AgentType, Description: description,
		Runtime: runtime, Attention: agentgraph.AttentionNone, Lifecycle: lifecycle,
		StartedAt: child.StartedAt, UpdatedAt: child.UpdatedAt, CompletedAt: child.CompletedAt,
	}
}

func graphLifecycle(lifecycle fanout.Lifecycle) agentgraph.LifecycleState {
	switch lifecycle {
	case fanout.LifecyclePending:
		return agentgraph.LifecyclePending
	case fanout.LifecycleRunning:
		return agentgraph.LifecycleRunning
	case fanout.LifecycleCompleted:
		return agentgraph.LifecycleCompleted
	case fanout.LifecycleInterrupted:
		return agentgraph.LifecycleInterrupted
	default:
		return agentgraph.LifecycleUnknown
	}
}

func (o *Observer) ensureRootLocked(root provider.RootRef) *rootState {
	key := root.Key()
	rs := o.roots[key]
	if rs != nil && rs.ref.ProviderSessionID == root.ProviderSessionID {
		rs.ref = root
		return rs
	}
	if rs != nil {
		o.fanout.Forget(rs.ref.ProviderSessionID)
	}
	var carriedEvents []history.Event
	if rs != nil {
		carriedEvents = rs.legacyEvents
	}
	rs = &rootState{
		ref: root, runtime: agentgraph.RuntimeUnknown,
		pending: make(map[string][]PendingPrompt), overlays: make(map[string]childOverlay),
		known: make(map[string]fanout.Lifecycle), retained: make(map[string]fanout.Child),
		legacyEvents: carriedEvents,
	}
	o.roots[key] = rs
	return rs
}

func validateRoot(root provider.RootRef) error {
	if root.Provider != "" && root.Provider != agentgraph.ProviderClaude {
		return fmt.Errorf("%w: %q", ErrWrongProvider, root.Provider)
	}
	if strings.TrimSpace(root.ProviderSessionID) == "" {
		return ErrMissingSession
	}
	return nil
}

func recognizedHook(event string) bool {
	switch event {
	case "UserPromptSubmit", "PostToolUse", "PermissionRequest", "Stop", "SessionStart", "SubagentStart", "SubagentStop":
		return true
	default:
		return false
	}
}

func attentionForTool(tool string) agentgraph.AttentionState {
	if tool == "AskUserQuestion" {
		return agentgraph.AttentionUserInput
	}
	return agentgraph.AttentionApproval
}

// promptMatches reports whether the signal resolves this exact prompt.
//
// Identity outranks shape. When BOTH sides name a call, the ids decide and
// nothing else is consulted: equal ids are the same call however far the input
// was rewritten between the prompt and the completion (the AskUserQuestion case
// that made the fast path unreachable), and UNEQUAL ids are a real negative —
// unlike a hash mismatch, which is equally "the approved call, rewritten" and "a
// sibling call", an id mismatch says outright that this is a different call, so
// the prompt is held rather than guessed at.
//
// Either side missing an id falls through to the pre-existing (tool, hash) rule
// verbatim. That is the common case for now: a prompt whose tool_use has not
// flushed yet, or whose tail was ambiguous, never binds one.
func promptMatches(pending PendingPrompt, signal HookSignal) bool {
	if signal.Event != "PostToolUse" || signal.ToolName == "" || signal.ToolName != pending.Tool {
		return false
	}
	if pending.Latch == CallLatchBound && pending.CallID != "" && signal.ToolUseID != "" {
		return pending.CallID == signal.ToolUseID
	}
	if pending.InputHash != "" && signal.ToolInputHash != "" && pending.InputHash != signal.ToolInputHash {
		return false
	}
	return true
}

// idMatched reports whether a match rested on call identity rather than on the
// (tool, hash) shape. Only such a match may lift the fanout floor, so the two
// have to stay distinguishable after matchingPromptIndex has picked one.
func idMatched(pending PendingPrompt, signal HookSignal) bool {
	return pending.Latch == CallLatchBound && pending.CallID != "" && pending.CallID == signal.ToolUseID
}

// callMismatchHeld reports whether the writer holds a prompt this signal
// positively is NOT — same tool, both sides identified, different calls. It is
// only a diagnosis: the hold already happened by promptMatches returning false.
// Recording it separately matters because a repeated writer_prompt_held on one
// red means "the correlator cannot name the call" while a repeated
// writer_call_mismatch_held means "it can, and this is a different call" — the
// first is a defect to chase, the second is the guard working.
func callMismatchHeld(prompts []PendingPrompt, signal HookSignal) bool {
	if signal.ToolUseID == "" {
		return false
	}
	for _, prompt := range prompts {
		if prompt.Tool == signal.ToolName && prompt.Latch == CallLatchBound && prompt.CallID != signal.ToolUseID {
			return true
		}
	}
	return false
}

func setChildRuntime(rs *rootState, id, agentType string, runtime agentgraph.RuntimeState, at time.Time) bool {
	// A non-attention hook is a cross-check, not an authoritative spawn source.
	// Retain it only for a child already established by fanout or prompt ownership.
	if len(rs.pending[id]) == 0 {
		if _, known := rs.known[id]; !known {
			return false
		}
	}
	prior := rs.overlays[id]
	next := prior
	next.AgentType = firstNonempty(agentType, prior.AgentType)
	next.Runtime, next.UpdatedAt = runtime, at
	rs.overlays[id] = next
	return prior != next
}

func normalizeAgentID(id string) string {
	rest, ok := strings.CutPrefix(id, "agent-")
	if !ok || rest == "" {
		return id
	}
	return rest
}

func lifecycleTerminal(lifecycle fanout.Lifecycle) bool {
	return lifecycle == fanout.LifecycleCompleted || lifecycle == fanout.LifecycleInterrupted
}

// resolvePending turns transcript evidence into writer-scoped resolutions. Every
// rule here reads a whole file — none of them can name which of a writer's
// parallel calls was answered — so each is evaluated against the writer's NEWEST
// prompt and closes the writer's whole open set when it fires. Dating the rules
// from the newest prompt is what keeps that safe: evidence younger than the last
// prompt to open cannot prove the older ones resolved.
func resolvePending(mainTranscript string, pending map[string][]PendingPrompt, snapshot fanout.Snapshot, now time.Time, tuning statustune.Tuning) []promptResolution {
	terminal := make(map[string]bool, len(snapshot.Children))
	for _, child := range snapshot.Children {
		terminal[child.ID] = child.LifecycleTerminal()
	}
	var resolutions []promptResolution
	for writer, prompts := range pending {
		if len(prompts) == 0 {
			continue
		}
		since := newestPromptSince(prompts)
		if terminal[writer] {
			resolutions = append(resolutions, promptResolution{writer, prompts, prompts, agentgraph.RuntimeIdle, statustune.RuleGraphChildTerminal})
			continue
		}
		path := transcript.SubagentPath(mainTranscript, writer)
		kind, err := transcript.ResolveKind(path, since, tuning.TailBytes)
		switch kind {
		case transcript.ResolutionResumed:
			resolutions = append(resolutions, promptResolution{writer, prompts, prompts, agentgraph.RuntimeActive, statustune.RuleGraphWriterResumed})
			continue
		case transcript.ResolutionInterrupted:
			resolutions = append(resolutions, promptResolution{writer, prompts, prompts, agentgraph.RuntimeIdle, statustune.RuleGraphWriterInterrupted})
			continue
		}
		if err != nil && writer == "" && !since.IsZero() && now.Sub(since) >= tuning.PermissionDecayTTL {
			resolutions = append(resolutions, promptResolution{writer, prompts, prompts, agentgraph.RuntimeIdle, statustune.RuleGraphMainUnreadableTTL})
			continue
		}
		// Call-scoped resolution sits AFTER the whole-file rules — they close the
		// writer's whole set, so anything they decide subsumes this — and, load
		// bearingly, BEFORE the blocked-by-pending-tool hold. That hold is exactly
		// the state a writer is in when one of its parallel calls was answered and
		// another still executes: whole-file evidence cannot tell those apart and
		// must keep the red, while the answered call's own id-matched tool_result
		// can and does.
		if closed, runtime, rule := resolveLatchedCalls(path, prompts, since, tuning.TailBytes); len(closed) > 0 {
			resolutions = append(resolutions, promptResolution{writer, prompts, closed, runtime, rule})
			continue
		}
		if evidence, evidenceErr := transcript.BlockedByPendingTool(path, tuning.TailBytes); evidenceErr == nil && evidence == transcript.BlockedYes {
			continue
		}
		if writerQuiescentPastCap(path, since, now, tuning.PendingWriterStaleCap) {
			resolutions = append(resolutions, promptResolution{writer, prompts, prompts, agentgraph.RuntimeIdle, statustune.RuleGraphStaleBackstop})
		}
	}
	sort.Slice(resolutions, func(i, j int) bool { return resolutions[i].Writer < resolutions[j].Writer })
	return resolutions
}

// latchPendingCalls binds each still-unidentified prompt to the exact call it
// gates, reading the writer's OWN transcript. It mutates the caller's detached
// clone so the resolution pass below sees the new ids, and returns the same
// changes as deltas for the under-lock merge.
//
// It runs on the Observe tick and NOT in ApplyHook, which does no filesystem
// I/O. That is not only the contract: PermissionRequest carries no tool_use_id,
// and the pending tool_use reaches disk +4.6/+4.9/+5.0 s AFTER the hook
// (docs/subagent-permission-plan.md §9.7), so a hook-time read would find
// nothing and latch nothing while looking implemented. Against the measured
// 45–300 s of user think-time, one tick later is early enough by two orders of
// magnitude.
//
// The binding rule is deliberately the strictest one that can ever fire: a
// prompt binds only when its writer has exactly ONE unbound prompt of that tool
// and its file exactly ONE unmatched call of it. Anything else stays unbound —
// zero candidates retries next tick (the flush lag), two or more is terminal.
// The asymmetry is the point: an unbound prompt keeps today's (tool, hash) rule
// and at worst stays red a few seconds too long, whereas a WRONG bind lets the
// sibling call's own result clear a prompt nobody answered — a missed RED,
// silent, costing the whole remaining wait.
func latchPendingCalls(mainTranscript string, pending map[string][]PendingPrompt, tailBytes int64) ([]promptLatch, []string) {
	writers := make([]string, 0, len(pending))
	for writer := range pending {
		writers = append(writers, writer)
	}
	sort.Strings(writers)

	// Candidates for every writer first, decisions second: the cross-writer
	// uniqueness check that P3 gates the lifted fanout floor on cannot be made
	// until every writer's claim is on the table.
	type toolKey struct{ writer, tool string }
	candidates := make(map[toolKey][]string)
	unbound := make(map[toolKey]int)
	// A set rather than an id->writer map: the main thread's writer key is "", so
	// a map lookup could not tell "bound to the main thread" from "not bound".
	bound := make(map[string]bool)
	for _, writer := range writers {
		for _, prompt := range pending[writer] {
			if prompt.Latch == CallLatchBound && prompt.CallID != "" {
				bound[prompt.CallID] = true
			}
		}
	}
	for _, writer := range writers {
		path := transcript.SubagentPath(mainTranscript, writer)
		for _, prompt := range pending[writer] {
			if !latchablePrompt(prompt) || prompt.Latch != CallLatchUnbound {
				continue
			}
			key := toolKey{writer, prompt.Tool}
			unbound[key]++
			if _, read := candidates[key]; read {
				continue // one read per (writer, tool), however many prompts share it
			}
			ids, err := transcript.PendingCall(path, prompt.Tool, tailBytes)
			if err != nil {
				ids = nil // unreadable reads as "not flushed yet": keep waiting
			}
			// A call another prompt already owns is not a candidate for this one.
			// That is what lets a writer holding one bound and one unbound prompt of
			// the same tool still identify the second.
			candidates[key] = slices.DeleteFunc(ids, func(id string) bool { return bound[id] })
		}
	}

	claims := make(map[string]map[string]bool, len(candidates))
	for key, ids := range candidates {
		for _, id := range ids {
			if claims[id] == nil {
				claims[id] = make(map[string]bool, 1)
			}
			claims[id][key.writer] = true
		}
	}

	var latches []promptLatch
	var diagnostics []string
	for _, writer := range writers {
		for _, prompt := range pending[writer] {
			if !latchablePrompt(prompt) || prompt.Latch != CallLatchUnbound {
				continue
			}
			key := toolKey{writer, prompt.Tool}
			ids := candidates[key]
			if len(ids) == 0 {
				continue // the tool_use has not flushed yet; retried next tick
			}
			if len(ids) > 1 || unbound[key] > 1 {
				latches = append(latches, promptLatch{writer, prompt.identity(), "", CallLatchAmbiguous})
				diagnostics = append(diagnostics, DiagnosticCallAmbiguous)
				continue
			}
			if len(claims[ids[0]]) > 1 {
				// Two writers appear to own one call. That should be impossible —
				// tool_use ids are per-call and each writer's calls live in its own
				// file — so it is counted and refused rather than resolved by a
				// tiebreak, and the prompt is left to retry on today's rule.
				diagnostics = append(diagnostics, DiagnosticCallIDCollision)
				continue
			}
			latches = append(latches, promptLatch{writer, prompt.identity(), ids[0], CallLatchBound})
			diagnostics = append(diagnostics, DiagnosticCallLatched)
		}
	}
	for _, latch := range latches {
		applyPromptLatch(pending, latch)
	}
	return latches, diagnostics
}

// latchablePrompt reports whether a prompt may ever bind a call id. A restored
// record may not: it stands for a writer's residual red rather than for one
// call, so an id-matched clear against it would turn a single honest red into a
// green with real calls still blocking (see PendingPrompt.Restored).
func latchablePrompt(prompt PendingPrompt) bool {
	return prompt.Tool != "" && !prompt.Restored
}

// applyPromptLatch writes one latch onto the writer's open set, addressing the
// prompt by identity. A miss means a hook closed or replaced that prompt while
// the scan was in flight, and the latch is simply dropped — the hook is the
// newer evidence, and the next tick re-reads anyway.
func applyPromptLatch(pending map[string][]PendingPrompt, latch promptLatch) {
	prompts := pending[latch.Writer]
	index := slices.IndexFunc(prompts, func(prompt PendingPrompt) bool {
		return prompt.Latch == CallLatchUnbound && prompt.identity() == latch.Identity
	})
	if index < 0 {
		return
	}
	prompts[index].CallID, prompts[index].Latch = latch.CallID, latch.State
}

// resolveLatchedCalls closes the prompts whose OWN call has come back, and says
// how the last of them went. It is the second signal the id buys: an answer
// clears the red even when the PostToolUse hook never arrives, within one tick
// instead of waiting for the writer's whole file to advance.
//
// A decline exits to idle rather than working. The user rejected the call and
// control came back to them; painting the chip green would say the agent is
// busy when it is waiting.
func resolveLatchedCalls(path string, prompts []PendingPrompt, since time.Time, tailBytes int64) ([]PendingPrompt, agentgraph.RuntimeState, string) {
	var closed []PendingPrompt
	runtime, rule := agentgraph.RuntimeActive, statustune.RuleGraphCallResolved
	for _, prompt := range prompts {
		if prompt.Latch != CallLatchBound || prompt.CallID == "" {
			continue
		}
		// `since` is the caller's own newest-prompt anchor, deliberately the same
		// value it just gave ResolveKind. ResolveKindFor falls back to that
		// timestamp rule when the id has no result yet, so passing anything else
		// would let the fallback decide something the caller has already declined
		// to decide on identical evidence.
		kind, err := transcript.ResolveKindFor(path, since, prompt.CallID, tailBytes)
		if err != nil {
			continue
		}
		switch kind {
		case transcript.ResolutionDeclined:
			closed = append(closed, prompt)
			runtime, rule = agentgraph.RuntimeIdle, statustune.RuleGraphCallDeclined
		case transcript.ResolutionResumed:
			closed = append(closed, prompt)
		}
	}
	return closed, runtime, rule
}

func writerQuiescentPastCap(path string, since, now time.Time, cap time.Duration) bool {
	if cap <= 0 || since.IsZero() {
		return false
	}
	quiescentSince := since
	if fi, err := os.Stat(path); err == nil && fi.ModTime().After(quiescentSince) {
		quiescentSince = fi.ModTime()
	}
	return now.Sub(quiescentSince) >= cap
}

func reconcileRootRuntime(path string, runtime agentgraph.RuntimeState, since time.Time, tailBytes int64) agentgraph.RuntimeState {
	if runtime != agentgraph.RuntimeIdle && runtime != agentgraph.RuntimeActive {
		return runtime
	}
	fi, err := os.Stat(path)
	if err != nil || !fi.ModTime().After(since) {
		return runtime
	}
	signal, at, err := transcript.NewestSignal(path, tailBytes)
	if err != nil || !at.After(since) {
		return runtime
	}
	if runtime == agentgraph.RuntimeIdle && signal == transcript.SignalActivity {
		return agentgraph.RuntimeActive
	}
	if runtime == agentgraph.RuntimeActive && signal == transcript.SignalInterrupt {
		return agentgraph.RuntimeIdle
	}
	return runtime
}

func clonePendingSets(pending map[string][]PendingPrompt) map[string][]PendingPrompt {
	clone := make(map[string][]PendingPrompt, len(pending))
	for writer, prompts := range pending {
		clone[writer] = slices.Clone(prompts)
	}
	return clone
}

func clonePending(pending map[string]PendingPrompt) map[string]PendingPrompt {
	clone := make(map[string]PendingPrompt, len(pending))
	for writer, prompt := range pending {
		clone[writer] = prompt
	}
	return clone
}

func firstNonempty(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

// Updates implements provider.Observer. Signals are non-blocking, coalesced
// invalidations; periodic observation remains the delivery backstop.
func (o *Observer) Updates() <-chan provider.RootKey { return o.updates.Updates() }

// Forget implements provider.Observer and is idempotent.
func (o *Observer) Forget(key provider.RootKey) {
	o.mu.Lock()
	rs := o.roots[key]
	delete(o.roots, key)
	o.mu.Unlock()
	if rs != nil {
		o.fanout.Forget(rs.ref.ProviderSessionID)
	}
}

// Close implements provider.Observer. The implementation owns no goroutines;
// it marks the cache closed exactly once and leaves the invalidation channel
// open to avoid send/close races, matching provider.InvalidationQueue's contract.
func (o *Observer) Close() error {
	o.mu.Lock()
	if !o.closed {
		o.closed = true
		clear(o.roots)
	}
	o.mu.Unlock()
	o.fanout.Prune(map[string]bool{})
	return nil
}
