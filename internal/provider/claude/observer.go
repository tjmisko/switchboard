// Package claude adapts Claude Code's hook and append-only fanout evidence to
// the provider-neutral agent graph without sharing Claude-specific inference
// with other providers or the neutral reducer.
package claude

import (
	"context"
	"errors"
	"fmt"
	"maps"
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

const transcriptStopQuietWindow = 90 * time.Second

const (
	preToolCandidateTTL           = 60 * time.Second
	maxPreToolCandidatesPerWriter = maxPendingPromptsPerWriter
)

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
	// It is present on PreToolUse/PostToolUse and absent on PermissionRequest
	// (docs/claude-code-hook-schema.md §2). A matcher-limited PreToolUse can stage
	// it for a later unique join at red onset; otherwise the prompt latches it
	// lazily from the transcript (PendingCall).
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
	ref            provider.RootRef
	runtime        agentgraph.RuntimeState
	runtimeAt      time.Time
	lastRootHookAt time.Time
	// pending maps a writer ("" is the main thread) to every prompt that writer
	// currently holds open, ordered oldest-first by Since. The writer routes
	// evidence — no sibling may ever clear another writer's prompt — but the
	// individual call closes the prompt, so one writer's parallel calls must not
	// collapse onto one record. A writer with no open prompt has no key at all;
	// an empty slice is never stored.
	pending map[string][]PendingPrompt
	// preToolCandidates stages opaque call identity from matcher-limited
	// PreToolUse hooks. Staging never opens attention; PermissionRequest consumes
	// a candidate only when writer, tool and non-empty input hash identify one
	// distinct call. The set is bounded, short-lived and never persisted.
	preToolCandidates map[string][]preToolCandidate
	// preToolContested quarantines a call id claimed by incompatible candidates
	// or prompts. It prevents a later redelivery from reintroducing the id during
	// the same short joining window.
	preToolContested map[string]time.Time
	// promptAnchors maps a writer to the newest instant at which any prompt it
	// currently holds opened — INCLUDING prompts already retired. It is the clock
	// every whole-file resolution rule dates from (writerResolutionAnchor), and it
	// is kept beside `pending` rather than derived from it for one reason:
	// retiring one of a writer's parallel prompts must never move that clock
	// backwards. The entry is dropped with the writer's last prompt, so the next
	// red starts its own anchor.
	promptAnchors map[string]time.Time
	overlays      map[string]childOverlay
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

type preToolCandidate struct {
	Tool       string
	InputHash  string
	CallID     string
	ObservedAt time.Time
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
	// At is the instant the state was decided, stamped onto the prompt so a
	// proposal can be told apart from a proposal made long enough ago to be worth
	// confirming (callLatchConfirmGrace).
	At time.Time
}

// The bounded diagnostic categories the call-identity latch reports.
//
// Every one of them is an EDGE, counted at most once per prompt because each
// records a terminal latch state. That is what makes them comparable with one
// another: a category that re-fired on every tick would count tick-seconds of a
// human's think-time rather than events, and one such counter beside two edge
// counters turns the whole table into a ratio nobody can read.
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
	// claim the same tool_use id, either by offering it in the same tick or by one
	// of them already holding it. The lifted fanout floor rests on this never
	// happening, so it is counted rather than assumed, and the prompt goes
	// CallLatchContested — refused, and refused once.
	DiagnosticCallIDCollision = "prompt_call_id_collision"

	DiagnosticPreToolStaged    = "pretooluse_staged"
	DiagnosticPreToolJoinHit   = "pretooluse_join_hit"
	DiagnosticPreToolJoinMiss  = "pretooluse_join_miss"
	DiagnosticPreToolAmbiguous = "pretooluse_join_ambiguous"
	DiagnosticPreToolCollision = "pretooluse_call_id_collision"
	DiagnosticPreToolExpired   = "pretooluse_expired"
)

// maxPromptDiagnosticsPerRoot bounds the undrained buffer. Latch outcomes are
// one-shot per prompt and PreTool candidates are capped per writer, but a burst
// of hooks before the coordinator drains the root must not grow without limit.
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
	anchors := maps.Clone(rs.promptAnchors)
	runtime, runtimeAt, turnStartedAt, lastRootHookAt := rs.runtime, rs.runtimeAt, rs.turnStartedAt, rs.lastRootHookAt
	tuning := o.tuning
	o.mu.Unlock()

	// Provider I/O is deliberately outside every state lock. fanout.Observer has
	// its own cursor lock and returns detached data.
	structured, scanErr := o.fanout.Observe(fanout.Root{
		SessionID: root.ProviderSessionID, Transcript: root.Transcript,
		PID: root.PID, Agent: string(agentgraph.ProviderClaude), CWD: root.CWD,
	}, now)
	latches, latchDiagnostics := latchPendingCalls(root.Transcript, pending, tuning.TailBytes, now)
	resolutions := resolvePending(root.Transcript, pending, anchors, structured.Snapshot, now, tuning)
	runtime = reconcileRootRuntime(root.Transcript, runtime, runtimeAt, turnStartedAt, lastRootHookAt, now, tuning.TailBytes)

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
	if runtime != agentgraph.RuntimeUnknown && rs.lastRootHookAt.Equal(lastRootHookAt) && rs.runtimeAt.Equal(runtimeAt) {
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
	appendPromptDiagnostics(rs, latchDiagnostics...)
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
		setPendingPrompts(rs, resolution.Writer, remaining)
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
	if writer == "" && signal.At.After(rs.lastRootHookAt) {
		rs.lastRootHookAt = signal.At
	}
	changed := false
	rule := statustune.RuleGraphHookNoChange
	if signal.Event == "SessionStart" {
		clear(rs.preToolCandidates)
		clear(rs.preToolContested)
	} else {
		expirePreToolCandidates(rs, signal.At)
	}

	if signal.Event == "PreToolUse" {
		stagePreToolCandidate(rs, writer, signal)
		return false, rule, 0
	}

	if signal.Event == "PermissionRequest" {
		next := PendingPrompt{
			Tool: signal.ToolName, InputHash: signal.ToolInputHash,
			Attention: attentionForTool(signal.ToolName), Since: signal.At,
		}
		if callID := consumePreToolCandidate(rs, writer, signal); callID != "" {
			next.CallID, next.Latch = callID, CallLatchBound
		}
		prompts, opened := openPendingPrompt(rs.pending[writer], next)
		setPendingPrompts(rs, writer, prompts)
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
		consumeCompletedPreToolCandidate(rs, signal.ToolUseID)
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
			setPendingPrompts(rs, writer, closePendingPromptAt(prompts, index))
			changed = true
		} else if len(prompts) > 0 {
			rule = statustune.RuleGraphPromptHeld
			// Only when NOTHING in the set matched. A shape match refused by the
			// fanout floor is a different hold — "a shape matched and nothing
			// identified the writer" — and relabelling it would report the identity
			// guard working on an edge identity never got to judge, which is the one
			// distinction the knob table asks this rule to carry.
			if index < 0 && callMismatchHeld(prompts, signal) {
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
	// StopFailure ends the turn exactly as Stop does: Claude Code fires it, and
	// not Stop, when an API error (a usage limit, an overload) ends the turn.
	case "Stop", "StopFailure":
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
		o.fanout.RecordChildLifecycle(rs.ref.ProviderSessionID, writer, signal.Event == "SubagentStart", signal.At)
		rule = statustune.RuleGraphFanoutRescan
	}
	return changed, rule, depth
}

func preToolIdentityTool(tool string) bool {
	return tool == "AskUserQuestion" || tool == "ExitPlanMode"
}

func appendPromptDiagnostics(rs *rootState, categories ...string) {
	for _, category := range categories {
		if category != "" {
			rs.promptDiagnostics = append(rs.promptDiagnostics, category)
		}
	}
	if len(rs.promptDiagnostics) > maxPromptDiagnosticsPerRoot {
		rs.promptDiagnostics = rs.promptDiagnostics[len(rs.promptDiagnostics)-maxPromptDiagnosticsPerRoot:]
	}
}

func expirePreToolCandidates(rs *rootState, at time.Time) {
	for writer, candidates := range rs.preToolCandidates {
		kept := candidates[:0]
		for _, candidate := range candidates {
			if at.Sub(candidate.ObservedAt) > preToolCandidateTTL {
				appendPromptDiagnostics(rs, DiagnosticPreToolExpired)
				continue
			}
			kept = append(kept, candidate)
		}
		if len(kept) == 0 {
			delete(rs.preToolCandidates, writer)
		} else {
			rs.preToolCandidates[writer] = kept
		}
	}
	for callID, contestedAt := range rs.preToolContested {
		if at.Sub(contestedAt) > preToolCandidateTTL {
			delete(rs.preToolContested, callID)
		}
	}
}

func stagePreToolCandidate(rs *rootState, writer string, signal HookSignal) {
	if !preToolIdentityTool(signal.ToolName) || signal.ToolInputHash == "" || signal.ToolUseID == "" {
		return
	}
	if _, contested := rs.preToolContested[signal.ToolUseID]; contested {
		return
	}

	duplicate, conflict := false, false
	for owner, prompts := range rs.pending {
		for _, prompt := range prompts {
			if prompt.CallID != signal.ToolUseID ||
				(prompt.Latch != CallLatchBound && prompt.Latch != CallLatchProposed) {
				continue
			}
			if prompt.Latch == CallLatchBound && owner == writer && prompt.Tool == signal.ToolName && prompt.InputHash == signal.ToolInputHash {
				duplicate = true
			} else {
				conflict = true
			}
		}
	}
	for owner, candidates := range rs.preToolCandidates {
		for _, candidate := range candidates {
			if candidate.CallID != signal.ToolUseID {
				continue
			}
			if owner == writer && candidate.Tool == signal.ToolName && candidate.InputHash == signal.ToolInputHash {
				duplicate = true
			} else {
				conflict = true
			}
		}
	}
	if conflict {
		quarantinePreToolCallID(rs, signal.ToolUseID, signal.At)
		return
	}
	if duplicate {
		return
	}

	candidates := append(rs.preToolCandidates[writer], preToolCandidate{
		Tool: signal.ToolName, InputHash: signal.ToolInputHash,
		CallID: signal.ToolUseID, ObservedAt: signal.At,
	})
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].ObservedAt.Before(candidates[j].ObservedAt) })
	if len(candidates) > maxPreToolCandidatesPerWriter {
		candidates = candidates[len(candidates)-maxPreToolCandidatesPerWriter:]
	}
	rs.preToolCandidates[writer] = candidates
	appendPromptDiagnostics(rs, DiagnosticPreToolStaged)
}

func quarantinePreToolCallID(rs *rootState, callID string, at time.Time) {
	if callID == "" {
		return
	}
	if _, already := rs.preToolContested[callID]; already {
		return
	}
	for writer, candidates := range rs.preToolCandidates {
		candidates = slices.DeleteFunc(candidates, func(candidate preToolCandidate) bool {
			return candidate.CallID == callID
		})
		if len(candidates) == 0 {
			delete(rs.preToolCandidates, writer)
		} else {
			rs.preToolCandidates[writer] = candidates
		}
	}
	for writer, prompts := range rs.pending {
		for i := range prompts {
			if prompts[i].CallID == callID &&
				(prompts[i].Latch == CallLatchBound || prompts[i].Latch == CallLatchProposed) {
				prompts[i].CallID = ""
				prompts[i].Latch = CallLatchContested
				prompts[i].LatchAt = at
			}
		}
		rs.pending[writer] = prompts
	}
	rs.preToolContested[callID] = at
	appendPromptDiagnostics(rs, DiagnosticPreToolCollision, DiagnosticCallIDCollision)
}

func consumePreToolCandidate(rs *rootState, writer string, signal HookSignal) string {
	if !preToolIdentityTool(signal.ToolName) {
		return ""
	}
	candidates := rs.preToolCandidates[writer]
	distinct := make(map[string]bool)
	hadToolCandidate := false
	for _, candidate := range candidates {
		if candidate.Tool != signal.ToolName {
			continue
		}
		hadToolCandidate = true
		if signal.ToolInputHash != "" && candidate.InputHash == signal.ToolInputHash {
			distinct[candidate.CallID] = true
		}
	}

	switch len(distinct) {
	case 0:
		// A same-writer/tool candidate that failed the positive hash join can no
		// longer be assigned safely. Purge it so a later call with the old shape
		// cannot inherit this call's id.
		removePreToolCandidates(rs, writer, func(candidate preToolCandidate) bool {
			return candidate.Tool == signal.ToolName
		})
		if hadToolCandidate {
			appendPromptDiagnostics(rs, DiagnosticPreToolJoinMiss)
		}
		return ""
	case 1:
		var callID string
		for candidateID := range distinct {
			callID = candidateID
		}
		removePreToolCandidates(rs, writer, func(candidate preToolCandidate) bool {
			return candidate.Tool == signal.ToolName && candidate.InputHash == signal.ToolInputHash
		})
		if preToolCallIDClaimed(rs.pending, callID) {
			quarantinePreToolCallID(rs, callID, signal.At)
			appendPromptDiagnostics(rs, DiagnosticPreToolJoinMiss)
			return ""
		}
		appendPromptDiagnostics(rs, DiagnosticPreToolJoinHit)
		return callID
	default:
		removePreToolCandidates(rs, writer, func(candidate preToolCandidate) bool {
			return candidate.Tool == signal.ToolName && candidate.InputHash == signal.ToolInputHash
		})
		appendPromptDiagnostics(rs, DiagnosticPreToolAmbiguous)
		return ""
	}
}

func preToolCallIDClaimed(pending map[string][]PendingPrompt, callID string) bool {
	for _, prompts := range pending {
		for _, prompt := range prompts {
			if prompt.CallID == callID &&
				(prompt.Latch == CallLatchBound || prompt.Latch == CallLatchProposed) {
				return true
			}
		}
	}
	return false
}

func consumeCompletedPreToolCandidate(rs *rootState, callID string) {
	if callID == "" {
		return
	}
	for writer := range rs.preToolCandidates {
		removePreToolCandidates(rs, writer, func(candidate preToolCandidate) bool {
			return candidate.CallID == callID
		})
	}
	delete(rs.preToolContested, callID)
}

func removePreToolCandidates(rs *rootState, writer string, remove func(preToolCandidate) bool) {
	candidates := slices.DeleteFunc(rs.preToolCandidates[writer], remove)
	if len(candidates) == 0 {
		delete(rs.preToolCandidates, writer)
		return
	}
	rs.preToolCandidates[writer] = candidates
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
//
// It is also the one choke point that moves the writer's resolution anchor, and
// the anchor only ever moves FORWARD while the writer holds anything — see
// writerResolutionAnchor for the missed RED that a regressing anchor buys.
func setPendingPrompts(rs *rootState, writer string, prompts []PendingPrompt) {
	if len(prompts) == 0 {
		delete(rs.pending, writer)
		delete(rs.promptAnchors, writer)
		return
	}
	rs.pending[writer] = prompts
	if newest := newestPromptSince(prompts); newest.After(rs.promptAnchors[writer]) {
		rs.promptAnchors[writer] = newest
	}
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

// writerResolutionAnchor is the instant the whole-file rules date from: the
// newest prompt this writer has held since its red opened, including the ones
// already retired. rootState.promptAnchors carries that memory; the live set is
// the floor under it, so a writer whose anchor was never recorded (a restore,
// say) still reads today's newest-prompt rule.
//
// It must only ever move FORWARD, and that is the whole point. Taking the newest
// still-open prompt alone lets the anchor REGRESS the moment a call-scoped rule
// retires the newest of a writer's parallel prompts — and it now can, because an
// id-matched result closes exactly one prompt. The next tick would then re-read
// the assistant entry that DISPATCHED the call it just closed as evidence
// younger than the anchor, and ResolveKind maps any assistant entry to "the
// writer resumed", closing the older prompt whose call is still open and
// unanswered. That is a missed RED, silent for the rest of the wait, and two
// gated calls of different tools in one turn are enough to reach it.
func writerResolutionAnchor(anchor time.Time, prompts []PendingPrompt) time.Time {
	if newest := newestPromptSince(prompts); newest.After(anchor) {
		return newest
	}
	return anchor
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
		setPendingPrompts(rs, child.ID, kept)
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
		pending: make(map[string][]PendingPrompt), promptAnchors: make(map[string]time.Time),
		preToolCandidates: make(map[string][]preToolCandidate), preToolContested: make(map[string]time.Time),
		overlays: make(map[string]childOverlay),
		known:    make(map[string]fanout.Lifecycle), retained: make(map[string]fanout.Child),
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
	case "UserPromptSubmit", "PreToolUse", "PostToolUse", "PermissionRequest", "Stop", "StopFailure", "SessionStart", "SubagentStart", "SubagentStop":
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
// parallel calls was answered — so each is evaluated against the writer's
// resolution ANCHOR and closes the writer's whole open set when it fires. Dating
// the rules from that anchor is what keeps them safe: evidence younger than the
// last prompt the writer opened cannot prove the older ones resolved, and the
// anchor is what stops a call-scoped clear from walking that clock backwards
// (writerResolutionAnchor).
func resolvePending(mainTranscript string, pending map[string][]PendingPrompt, anchors map[string]time.Time, snapshot fanout.Snapshot, now time.Time, tuning statustune.Tuning) []promptResolution {
	terminal := make(map[string]bool, len(snapshot.Children))
	for _, child := range snapshot.Children {
		terminal[child.ID] = child.LifecycleTerminal()
	}
	var resolutions []promptResolution
	for writer, prompts := range pending {
		if len(prompts) == 0 {
			continue
		}
		since := writerResolutionAnchor(anchors[writer], prompts)
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
// The binding rule is deliberately the strictest one that can ever fire. A
// prompt binds only when ALL of the following hold, and every one of them exists
// because dropping it produced a missed RED:
//
//   - its writer has exactly ONE unlatched prompt of that tool, and its file
//     exactly ONE eligible unmatched call of it (two are indistinguishable);
//   - the call is not dated before the prompt opened by more than the two clocks
//     can skew (callLatchSkewGrace) — otherwise the FIRST latch attempt, which
//     runs milliseconds after the hook because ApplyHook signals the coordinator,
//     would bind whatever sibling happened to be executing, ~5 s before the
//     prompt's own tool_use can possibly be on disk;
//   - no other writer offers or already holds that id (P3's runtime half);
//   - and the SAME id was the sole candidate on an earlier read taken at least
//     callLatchConfirmGrace ago (CallLatchProposed) — one read's uniqueness is
//     not uniqueness when one message's parallel tool_use blocks reach disk
//     0.5–1.5 s apart, and two reads inside that gap agree about the same
//     partial file.
//
// Anything else stays unlatched: zero candidates retries next tick (the flush
// lag), and ambiguity or contention is terminal. The asymmetry is the point — an
// unlatched prompt keeps today's (tool, hash) rule and at worst stays red a few
// seconds too long, whereas a WRONG bind lets some other call's result clear a
// prompt nobody answered, a missed RED, silent, costing the whole remaining wait.
func latchPendingCalls(mainTranscript string, pending map[string][]PendingPrompt, tailBytes int64, now time.Time) ([]promptLatch, []string) {
	writers := make([]string, 0, len(pending))
	for writer := range pending {
		writers = append(writers, writer)
	}
	sort.Strings(writers)

	// Candidates for every writer first, decisions second: the cross-writer
	// uniqueness check that P3 gates the lifted fanout floor on cannot be made
	// until every writer's claim is on the table.
	type toolKey struct{ writer, tool string }
	calls := make(map[toolKey][]transcript.PendingToolCall)
	unlatched := make(map[toolKey]int)
	// An id -> writer map plus a separate presence set: the main thread's writer
	// key is "", so a bare lookup could not tell "bound to the main thread" from
	// "not bound", and WHICH writer holds it is exactly what separates a prompt
	// skipping its own writer's other call from two writers claiming one id.
	boundBy := make(map[string]string)
	boundHeld := make(map[string]bool)
	for _, writer := range writers {
		for _, prompt := range pending[writer] {
			if prompt.Latch == CallLatchBound && prompt.CallID != "" {
				boundBy[prompt.CallID], boundHeld[prompt.CallID] = writer, true
			}
		}
	}
	for _, writer := range writers {
		path := transcript.SubagentPath(mainTranscript, writer)
		for _, prompt := range pending[writer] {
			if !latchablePrompt(prompt) {
				continue
			}
			key := toolKey{writer, prompt.Tool}
			unlatched[key]++
			if _, read := calls[key]; read {
				continue // one read per (writer, tool), however many prompts share it
			}
			found, err := transcript.PendingCall(path, prompt.Tool, tailBytes)
			if err != nil {
				found = nil // unreadable reads as "not flushed yet": keep waiting
			}
			calls[key] = found
		}
	}

	// Each prompt's own eligible set, then the cross-writer view of it. Claims are
	// counted over ELIGIBLE candidates rather than raw ones so a call that is
	// nobody's plausible owner cannot be reported as contested.
	type promptCandidates struct {
		writer    string
		prompt    PendingPrompt
		eligible  []string
		contested bool
	}
	var considered []promptCandidates
	claims := make(map[string]map[string]bool)
	for _, writer := range writers {
		for _, prompt := range pending[writer] {
			if !latchablePrompt(prompt) {
				continue
			}
			candidates := promptCandidates{writer: writer, prompt: prompt}
			for _, call := range calls[toolKey{writer, prompt.Tool}] {
				if boundHeld[call.ID] {
					// A call some prompt already owns is not a candidate for this one.
					// That is what lets a writer holding one bound and one unlatched
					// prompt of the same tool still identify the second — but only when
					// the owner is that same writer. Another writer's bound id appearing
					// here is P3 failing in the sequential ordering, and it must be
					// counted rather than quietly dropped: dropping it is what let one
					// writer stay bound to a call another writer also claims.
					candidates.contested = candidates.contested || boundBy[call.ID] != writer
					continue
				}
				if !ownableCall(call, prompt) {
					continue
				}
				candidates.eligible = append(candidates.eligible, call.ID)
				if claims[call.ID] == nil {
					claims[call.ID] = make(map[string]bool, 1)
				}
				claims[call.ID][writer] = true
			}
			considered = append(considered, candidates)
		}
	}

	var latches []promptLatch
	var diagnostics []string
	for _, candidates := range considered {
		prompt, writer := candidates.prompt, candidates.writer
		contested := candidates.contested
		if !contested && len(candidates.eligible) == 1 {
			// Two writers appear to own one call. That should be impossible —
			// tool_use ids are per-call and each writer's calls live in its own file —
			// so it is counted and refused rather than resolved by a tiebreak.
			contested = len(claims[candidates.eligible[0]]) > 1
		}
		switch {
		case contested:
			latches = append(latches, promptLatch{writer, prompt.identity(), "", CallLatchContested, now})
			diagnostics = append(diagnostics, DiagnosticCallIDCollision)
		case len(candidates.eligible) == 0:
			// Nothing this prompt could own is on disk yet; retried next tick. A
			// proposal already made is deliberately kept: a tail that briefly stops
			// naming the candidate is not evidence against it.
		case len(candidates.eligible) > 1 || unlatched[toolKey{writer, prompt.Tool}] > 1:
			latches = append(latches, promptLatch{writer, prompt.identity(), "", CallLatchAmbiguous, now})
			diagnostics = append(diagnostics, DiagnosticCallAmbiguous)
		case prompt.Latch == CallLatchProposed && prompt.CallID == candidates.eligible[0]:
			if now.Before(prompt.LatchAt.Add(callLatchConfirmGrace)) {
				// Too soon to be a second view of the file. The proposal is left
				// exactly as it stands — including its original LatchAt, which is what
				// the grace is measured from — and the next read that clears the grace
				// confirms or withdraws it.
				continue
			}
			latches = append(latches, promptLatch{writer, prompt.identity(), candidates.eligible[0], CallLatchBound, now})
			diagnostics = append(diagnostics, DiagnosticCallLatched)
		default:
			// The first read to name this candidate. It is recorded, not counted: a
			// proposal is not an outcome, and counting one per tick would make this
			// table a mix of level and edge counters that no ratio can be read off.
			latches = append(latches, promptLatch{writer, prompt.identity(), candidates.eligible[0], CallLatchProposed, now})
		}
	}
	for _, latch := range latches {
		applyPromptLatch(pending, latch)
	}
	return latches, diagnostics
}

// callLatchSkewGrace is how far BEFORE its prompt opened a candidate call may be
// dated and still be that prompt's own.
//
// The comparison is between two clocks with a known offset: the transcript dates
// an assistant entry at GENERATION time, 6–374 ms before the matching
// PermissionRequest hook fires, and one message's parallel tool_use blocks are
// separate entries 0.5–1.5 s apart, so a prompt's own call can honestly be dated
// a second or two before the prompt. Anything older belongs to an earlier
// dispatch — a sibling still executing, or a call from a turn the user
// interrupted, which sits unmatched in the tail forever. The window is therefore
// set just past the widest measured spread, and everything outside it is refused.
//
// It is not a heuristic tiebreak: it can only ever REMOVE a candidate, and every
// removal costs at most a stale red while the bind it prevents would cost a
// missed one.
const callLatchSkewGrace = 3 * time.Second

// callLatchConfirmGrace is how long a proposal must stand before a second read
// may confirm it.
//
// The confirmation rule exists to defeat one shape: a read landing inside the
// 0.5–1.5 s gap between one assistant message's parallel tool_use entries sees a
// gated call's auto-approved sibling ALONE and reads it as unique. Counting
// reads alone does not defeat it. ApplyHook signals the coordinator, so every
// hook edge from ANY writer schedules an Observe for this root, and a fanned-out
// session routinely delivers two of them milliseconds apart — two reads of the
// same partial file, agreeing for the same wrong reason. The grace is what makes
// the confirming read a genuinely later view of the file.
//
// It is set just past the widest measured inter-entry spread, like
// callLatchSkewGrace and for the same evidence. It costs nothing in practice:
// the periodic reconcile is 5 s, so the read that would have confirmed a
// proposal too early is followed by one that confirms it on schedule.
const callLatchConfirmGrace = 2 * time.Second

// ownableCall reports whether a candidate could be the call this prompt gates,
// on the only ordering evidence the file carries. There is no upper bound: a
// blocked writer dispatches nothing, so a call dated after the prompt is either
// its own or its own message's sibling, and the ambiguity rule handles that.
func ownableCall(call transcript.PendingToolCall, prompt PendingPrompt) bool {
	return !call.At.Before(prompt.Since.Add(-callLatchSkewGrace))
}

// latchablePrompt reports whether a prompt may still bind a call id — it has a
// tool, it is not residual, and its latch has not gone terminal.
//
// A residual record may not bind: it stands for a writer's leftover red rather
// than for one call, so an id-matched clear against it would turn a single
// honest red into a green with real calls still blocking (see
// PendingPrompt.Residual). A prompt restored from a per-call record is not
// residual and latches like any other — that is how a red carried across a
// restart still clears on its own call's result instead of waiting for the
// writer's whole file to advance.
func latchablePrompt(prompt PendingPrompt) bool {
	if prompt.Tool == "" || prompt.Residual {
		return false
	}
	return prompt.Latch == CallLatchUnbound || prompt.Latch == CallLatchProposed
}

// applyPromptLatch writes one latch onto the writer's open set, addressing the
// prompt by identity. A miss means a hook closed or replaced that prompt while
// the scan was in flight, and the latch is simply dropped — the hook is the
// newer evidence, and the next tick re-reads anyway.
func applyPromptLatch(pending map[string][]PendingPrompt, latch promptLatch) {
	prompts := pending[latch.Writer]
	index := slices.IndexFunc(prompts, func(prompt PendingPrompt) bool {
		return latchablePrompt(prompt) && prompt.identity() == latch.Identity
	})
	if index < 0 {
		return
	}
	prompts[index].CallID, prompts[index].Latch, prompts[index].LatchAt = latch.CallID, latch.State, latch.At
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
	callIDs := make([]string, 0, len(prompts))
	for _, prompt := range prompts {
		if prompt.Latch == CallLatchBound && prompt.CallID != "" {
			callIDs = append(callIDs, prompt.CallID)
		}
	}
	if len(callIDs) == 0 {
		return nil, agentgraph.RuntimeActive, statustune.RuleGraphCallResolved
	}
	// ONE read of the tail for the writer's whole bound set. `since` is the
	// caller's own resolution anchor, deliberately the same value it just gave
	// ResolveKind: the per-id rules fall back to that timestamp rule when a call
	// has no result yet, so passing anything else would let the fallback decide
	// something the caller has already declined to decide on identical evidence.
	kinds, err := transcript.ResolveKindForCalls(path, since, callIDs, tailBytes)
	if err != nil {
		return nil, agentgraph.RuntimeActive, statustune.RuleGraphCallResolved
	}
	var closed []PendingPrompt
	declined := 0
	for _, prompt := range prompts {
		if prompt.Latch != CallLatchBound || prompt.CallID == "" {
			continue
		}
		switch kinds[prompt.CallID] {
		case transcript.ResolutionDeclined:
			closed = append(closed, prompt)
			declined++
		case transcript.ResolutionResumed:
			closed = append(closed, prompt)
		}
	}
	// The exit describes the WRITER, not whichever prompt sat last in the slice.
	// A decline only returns control to the user if nothing else the writer just
	// closed resumed it: a tick that carries one rejection and one approval leaves
	// the agent executing the approved call, so painting the chip idle there would
	// misreport both the color and the rule on the strength of slice order alone.
	if declined > 0 && declined == len(closed) {
		return closed, agentgraph.RuntimeIdle, statustune.RuleGraphCallDeclined
	}
	return closed, agentgraph.RuntimeActive, statustune.RuleGraphCallResolved
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

func reconcileRootRuntime(path string, runtime agentgraph.RuntimeState, since, turnStartedAt, lastHookAt, now time.Time, tailBytes int64) agentgraph.RuntimeState {
	if runtime != agentgraph.RuntimeIdle && runtime != agentgraph.RuntimeActive {
		return runtime
	}
	signal, at, err := transcript.NewestRuntimeSignal(path, tailBytes)
	if err == nil && signal == transcript.SignalStopped && !at.Before(turnStartedAt) && !at.Before(lastHookAt) && now.Sub(lastHookAt) >= transcriptStopQuietWindow {
		return agentgraph.RuntimeIdle
	}
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

// clonePendingSets detaches a writer→open-set map. Empty in, nil out: the clone
// is only ever read, and a nil map reads identically, so callers that hand the
// result on (Compatibility.Clone) do not turn "no prompts" into an empty map that
// encodes and compares differently from the absence it stands for.
func clonePendingSets(pending map[string][]PendingPrompt) map[string][]PendingPrompt {
	if len(pending) == 0 {
		return nil
	}
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
