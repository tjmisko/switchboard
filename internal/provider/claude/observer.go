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
	At            time.Time
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
	Runtime agentgraph.RuntimeState
	Rule    string
}

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
	for _, resolution := range resolutions {
		if current, ok := rs.pending[resolution.Writer]; !ok || !slices.Equal(current, resolution.Prompts) {
			continue // a newer hook opened or closed a prompt while the scan ran
		}
		delete(rs.pending, resolution.Writer)
		if rs.resolvedRule == "" {
			// resolutions is sorted by writer and the main thread's key is "", so
			// when several writers resolve on one tick the main thread's rule is the
			// one kept. It is the writer whose resolution most often flips the chip,
			// and one id per transition is all the record has room for.
			rs.resolvedRule = resolution.Rule
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
		if index := matchingPromptIndex(prompts, signal); index >= 0 &&
			!(writer == "" && rs.fanout.InFlight > 0) {
			setPendingPrompts(rs.pending, writer, closePendingPromptAt(prompts, index))
			changed = true
			rule = statustune.RuleGraphToolMatchCleared
		} else if len(prompts) > 0 {
			rule = statustune.RuleGraphPromptHeld
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
	if slices.Contains(prompts, next) {
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

// matchingPromptIndex returns the oldest prompt the signal resolves, or -1. The
// oldest match is taken because prompts are answered in the order they were
// presented, and because it leaves the newest Since in place — the backstop
// clock then runs from the most recent evidence, which is the stale-red
// direction rather than the missed-red one.
func matchingPromptIndex(prompts []PendingPrompt, signal HookSignal) int {
	for i, prompt := range prompts {
		if promptMatches(prompt, signal) {
			return i
		}
	}
	return -1
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

func promptMatches(pending PendingPrompt, signal HookSignal) bool {
	if signal.Event != "PostToolUse" || signal.ToolName == "" || signal.ToolName != pending.Tool {
		return false
	}
	if pending.InputHash != "" && signal.ToolInputHash != "" && pending.InputHash != signal.ToolInputHash {
		return false
	}
	return true
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
			resolutions = append(resolutions, promptResolution{writer, prompts, agentgraph.RuntimeIdle, statustune.RuleGraphChildTerminal})
			continue
		}
		path := transcript.SubagentPath(mainTranscript, writer)
		kind, err := transcript.ResolveKind(path, since, tuning.TailBytes)
		switch kind {
		case transcript.ResolutionResumed:
			resolutions = append(resolutions, promptResolution{writer, prompts, agentgraph.RuntimeActive, statustune.RuleGraphWriterResumed})
			continue
		case transcript.ResolutionInterrupted:
			resolutions = append(resolutions, promptResolution{writer, prompts, agentgraph.RuntimeIdle, statustune.RuleGraphWriterInterrupted})
			continue
		}
		if err != nil && writer == "" && !since.IsZero() && now.Sub(since) >= tuning.PermissionDecayTTL {
			resolutions = append(resolutions, promptResolution{writer, prompts, agentgraph.RuntimeIdle, statustune.RuleGraphMainUnreadableTTL})
			continue
		}
		if evidence, evidenceErr := transcript.BlockedByPendingTool(path, tuning.TailBytes); evidenceErr == nil && evidence == transcript.BlockedYes {
			continue
		}
		if writerQuiescentPastCap(path, since, now, tuning.PendingWriterStaleCap) {
			resolutions = append(resolutions, promptResolution{writer, prompts, agentgraph.RuntimeIdle, statustune.RuleGraphStaleBackstop})
		}
	}
	sort.Slice(resolutions, func(i, j int) bool { return resolutions[i].Writer < resolutions[j].Writer })
	return resolutions
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
