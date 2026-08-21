// Package claude adapts Claude Code's hook and append-only fanout evidence to
// the provider-neutral agent graph without sharing Claude-specific inference
// with other providers or the neutral reducer.
package claude

import (
	"context"
	"errors"
	"fmt"
	"os"
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
	Applied     bool
	Changed     bool
	Rule        string
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
	pending   map[string]PendingPrompt
	overlays  map[string]childOverlay

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

type promptResolution struct {
	Writer  string
	Since   time.Time
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
	pending := clonePending(rs.pending)
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
	for _, resolution := range resolutions {
		if current, ok := rs.pending[resolution.Writer]; !ok || current.Since != resolution.Since {
			continue // a newer hook replaced this prompt while the scan ran
		}
		delete(rs.pending, resolution.Writer)
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
		clearTerminalPrompts(rs, structured.Snapshot.ObservedAt)
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
	changed, rule := o.applyHookLocked(rs, signal)
	observation, _ := o.rebuildLocked(rs, signal.At, agentgraph.SourceHook, rs.hasFanout && rs.fanout.Complete)
	o.updates.Signal(signal.Root.Key())
	return HookResult{
		Applied: true, Changed: changed, Rule: rule, Root: signal.Root.Key(),
		Observation: observation.Clone(), Projection: rs.projection.Clone(),
	}
}

func (o *Observer) applyHookLocked(rs *rootState, signal HookSignal) (bool, string) {
	writer := signal.AgentID
	changed := false
	rule := "hook_no_summary_change"

	if signal.Event == "PermissionRequest" {
		attention := attentionForTool(signal.ToolName)
		prior, exists := rs.pending[writer]
		next := PendingPrompt{
			Tool: signal.ToolName, InputHash: signal.ToolInputHash,
			Attention: attention, Since: signal.At,
		}
		rs.pending[writer] = next
		if writer != "" {
			overlay := rs.overlays[writer]
			overlay.AgentType = firstNonempty(signal.AgentType, overlay.AgentType)
			overlay.Runtime = agentgraph.RuntimeIdle
			overlay.UpdatedAt = signal.At
			rs.overlays[writer] = overlay
		}
		changed = !exists || prior != next
		return changed, "permission_recorded_for_writer"
	}

	switch signal.Event {
	case "SessionStart":
		if rs.runtime != agentgraph.RuntimeIdle {
			rs.runtime, rs.runtimeAt, changed = agentgraph.RuntimeIdle, signal.At, true
		}
		rule = "root_session_started"
	case "UserPromptSubmit":
		if writer == "" {
			rs.turnStartedAt = signal.At
			clear(rs.retained)
			if rs.runtime != agentgraph.RuntimeActive {
				changed = true
			}
			rs.runtime, rs.runtimeAt = agentgraph.RuntimeActive, signal.At
			rule = "root_prompt_submitted"
		} else {
			changed = setChildRuntime(rs, writer, signal.AgentType, agentgraph.RuntimeActive, signal.At)
			rule = "child_activity_only"
		}
	case "PostToolUse":
		if pending, owns := rs.pending[writer]; owns && promptMatches(pending, signal) &&
			!(writer == "" && rs.fanout.InFlight > 0) {
			delete(rs.pending, writer)
			changed = true
			rule = "writer_tool_match_cleared"
		} else if owns {
			rule = "writer_prompt_held"
		} else {
			rule = "non_owner_prompt_held"
		}
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
			rule = "root_stopped"
		} else {
			changed = setChildRuntime(rs, writer, signal.AgentType, agentgraph.RuntimeIdle, signal.At)
			rule = "child_activity_only"
		}
	case "SubagentStart", "SubagentStop":
		// Directory/journal scan remains authoritative for spawn and completion.
		// These hooks only invalidate the cached snapshot.
		rule = "fanout_rescan_requested"
	}
	return changed, rule
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

func clearTerminalPrompts(rs *rootState, observedAt time.Time) {
	for _, child := range rs.fanout.Children {
		prompt, pending := rs.pending[child.ID]
		if child.LifecycleTerminal() && pending && !prompt.Since.After(observedAt) {
			delete(rs.pending, child.ID)
		}
	}
}

func (o *Observer) rebuildLocked(rs *rootState, now time.Time, source agentgraph.SourceKind, complete bool) (agentgraph.Observation, error) {
	rootAttention := agentgraph.AttentionNone
	if pending, ok := rs.pending[""]; ok {
		rootAttention = pending.Attention
	}
	nodes := []agentgraph.Node{{
		ID: rs.ref.ProviderSessionID, Runtime: rs.runtime, Attention: rootAttention,
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
	for writer := range rs.pending {
		if writer == "" {
			continue
		}
		child, exists := byID[writer]
		if !exists || (child.LifecycleTerminal() && rs.pending[writer].Since.After(rs.fanout.ObservedAt)) {
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
		if pending, ok := rs.pending[id]; ok && !node.Lifecycle.Terminal() {
			node.Attention = pending.Attention
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
		pending: make(map[string]PendingPrompt), overlays: make(map[string]childOverlay),
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
	if _, pending := rs.pending[id]; !pending {
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

func resolvePending(mainTranscript string, pending map[string]PendingPrompt, snapshot fanout.Snapshot, now time.Time, tuning statustune.Tuning) []promptResolution {
	terminal := make(map[string]bool, len(snapshot.Children))
	for _, child := range snapshot.Children {
		terminal[child.ID] = child.LifecycleTerminal()
	}
	var resolutions []promptResolution
	for writer, prompt := range pending {
		if terminal[writer] {
			resolutions = append(resolutions, promptResolution{writer, prompt.Since, agentgraph.RuntimeIdle, "child_terminal"})
			continue
		}
		path := transcript.SubagentPath(mainTranscript, writer)
		kind, err := transcript.ResolveKind(path, prompt.Since, tuning.TailBytes)
		switch kind {
		case transcript.ResolutionResumed:
			resolutions = append(resolutions, promptResolution{writer, prompt.Since, agentgraph.RuntimeActive, "writer_resumed"})
			continue
		case transcript.ResolutionInterrupted:
			resolutions = append(resolutions, promptResolution{writer, prompt.Since, agentgraph.RuntimeIdle, "writer_interrupted"})
			continue
		}
		if err != nil && writer == "" && !prompt.Since.IsZero() && now.Sub(prompt.Since) >= tuning.PermissionDecayTTL {
			resolutions = append(resolutions, promptResolution{writer, prompt.Since, agentgraph.RuntimeIdle, "main_unreadable_ttl"})
			continue
		}
		if evidence, evidenceErr := transcript.BlockedByPendingTool(path, tuning.TailBytes); evidenceErr == nil && evidence == transcript.BlockedYes {
			continue
		}
		if writerQuiescentPastCap(path, prompt.Since, now, tuning.PendingWriterStaleCap) {
			resolutions = append(resolutions, promptResolution{writer, prompt.Since, agentgraph.RuntimeIdle, "writer_stale_backstop"})
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
