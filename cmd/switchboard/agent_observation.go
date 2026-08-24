package main

import (
	"context"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/fanout"
	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/provider"
	claudeprovider "github.com/tjmisko/switchboard/internal/provider/claude"
	codexprovider "github.com/tjmisko/switchboard/internal/provider/codex"
	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/state"
)

const (
	// Hook fallback is deliberately finite because it is partial, edge-triggered
	// evidence. State-specific windows keep ordinary colors useful when a Codex
	// TUI has no attachable app-server while bounding the damage from a missed
	// resolution edge.
	codexHookActiveFreshness    = 10 * time.Minute
	codexHookAttentionFreshness = 24 * time.Hour
	codexHookIdleFreshness      = 7 * 24 * time.Hour
)

type claudeObserver interface {
	provider.Observer
	ApplyHook(claudeprovider.HookSignal) claudeprovider.HookResult
	Restore(provider.RootRef, claudeprovider.Compatibility, time.Time) (agentgraph.Observation, error)
	Projection(provider.RootKey) claudeprovider.Compatibility
	DrainLegacyEvents(provider.RootKey) []history.Event
}

type codexObserver interface {
	provider.Observer
	RegisterHookBinding(provider.RootKey, string) error
}

type codexBindingReconciler interface {
	ReconcileHookBinding(provider.RootKey, string) (codexprovider.BindingUpdate, error)
}

type trackedProviderRoot struct {
	provider provider.Observer
	kind     agentgraph.ProviderKind
	rootID   string
	restored bool
}

// agentCoordinator is the single graph-observation landing path. Periodic and
// event-triggered observations may do slow I/O concurrently with hook delivery,
// but every root carries a monotonically increasing generation. A hook that
// lands while an older observation is blocked fences that old result out.
type agentCoordinator struct {
	store   *state.Store
	sink    *history.Sink
	claude  claudeObserver
	codex   codexObserver
	history *history.AgentStateProjector

	requests *provider.InvalidationQueue

	mu          sync.Mutex
	generation  map[provider.RootKey]uint64
	tracked     map[provider.RootKey]trackedProviderRoot
	diagnostics map[string]rpc.AgentDiagnostic
	lastLog     map[string]time.Time

	cancel context.CancelFunc
	wg     sync.WaitGroup
	start  sync.Once
	once   sync.Once

	namingMu      sync.Mutex
	naming        map[provider.RootKey]*codexNamingState
	namer         codexprovider.NameGenerator
	namingModel   string
	namingTimeout time.Duration

	codexHookMu        sync.Mutex
	codexHookRoots     map[provider.RootKey]*codexHookRootState
	codexStarts        map[provider.RootKey]*pendingCodexStart
	codexStartSettle   time.Duration
	codexApprovalGrace time.Duration
	codexWaitEpisode   uint64
	codexTimerWG       sync.WaitGroup
}

type codexNamingState struct {
	conversationID string
	candidate      *codexNamingCandidate
	completedAt    time.Time
	attempt        uint64
	cancel         context.CancelFunc
}

type codexNamingCandidate struct {
	turnID  string
	prompt  string
	at      time.Time
	cwdBase string
}

type codexNamingInput struct {
	key            provider.RootKey
	conversationID string
	attempt        uint64
	context        codexprovider.NamingContext
}

func newAgentCoordinator(store *state.Store, sink *history.Sink, claude claudeObserver, codex codexObserver) *agentCoordinator {
	return &agentCoordinator{
		store: store, sink: sink, claude: claude, codex: codex,
		history: history.NewAgentStateProjector(), requests: provider.NewInvalidationQueue(64),
		generation: make(map[provider.RootKey]uint64), tracked: make(map[provider.RootKey]trackedProviderRoot),
		diagnostics: make(map[string]rpc.AgentDiagnostic), lastLog: make(map[string]time.Time),
		naming: make(map[provider.RootKey]*codexNamingState), namer: codexprovider.EphemeralNamer{},
		namingModel: codexprovider.DefaultDisplayNameModel, namingTimeout: 45 * time.Second,
		codexHookRoots: make(map[provider.RootKey]*codexHookRootState), codexStarts: make(map[provider.RootKey]*pendingCodexStart),
		codexStartSettle: codexHookStartSettle, codexApprovalGrace: codexHookApprovalGrace,
	}
}

func (c *agentCoordinator) Start(parent context.Context, interval time.Duration) {
	c.start.Do(func() {
		if interval <= 0 {
			interval = time.Second
		}
		ctx, cancel := context.WithCancel(parent)
		c.mu.Lock()
		c.cancel = cancel
		c.mu.Unlock()
		c.wg.Add(1)
		go c.run(ctx, interval)
	})
}

func (c *agentCoordinator) Close() {
	c.once.Do(func() {
		c.mu.Lock()
		cancel := c.cancel
		c.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		c.namingMu.Lock()
		for _, naming := range c.naming {
			if naming.cancel != nil {
				naming.cancel()
			}
		}
		clear(c.naming)
		c.namingMu.Unlock()
		c.codexHookMu.Lock()
		for _, pending := range c.codexStarts {
			if pending.timer.Stop() {
				pending.finish(&c.codexTimerWG)
			}
		}
		for _, root := range c.codexHookRoots {
			c.clearCodexApprovalsLocked(root)
		}
		clear(c.codexStarts)
		clear(c.codexHookRoots)
		c.codexHookMu.Unlock()
		c.codexTimerWG.Wait()
		c.wg.Wait()
		if c.claude != nil {
			_ = c.claude.Close()
		}
		if c.codex != nil {
			_ = c.codex.Close()
		}
	})
}

// Request schedules an immediate observation. A full queue drops requests;
// provider invalidations and the periodic pass are required delivery backstops.
func (c *agentCoordinator) Request(key provider.RootKey) {
	c.requests.Signal(key)
}

// RequestCleanup wakes the serialized loop after a root was removed. The zero
// key intentionally matches no root; reconcileKey still refreshes the tracked
// set first and performs every pending Forget outside the store lock.
func (c *agentCoordinator) RequestCleanup() { c.Request(provider.RootKey{}) }

func providerRootKey(sess state.Session) provider.RootKey {
	return provider.RootKey{PID: sess.PID, StartedAt: sess.StartedAt}
}

func (c *agentCoordinator) run(ctx context.Context, interval time.Duration) {
	defer c.wg.Done()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	c.reconcileAll(ctx)
	var claudeUpdates, codexUpdates <-chan provider.RootKey
	if c.claude != nil {
		claudeUpdates = c.claude.Updates()
	}
	if c.codex != nil {
		codexUpdates = c.codex.Updates()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case key := <-c.requests.Updates():
			c.reconcileKey(ctx, key)
		case key := <-claudeUpdates:
			c.reconcileKey(ctx, key)
		case key := <-codexUpdates:
			c.reconcileKey(ctx, key)
		case <-ticker.C:
			c.reconcileAll(ctx)
		}
	}
}

func (c *agentCoordinator) reconcileAll(ctx context.Context) {
	refs := c.refreshTrackedRoots()
	for _, ref := range refs {
		if ctx.Err() != nil {
			return
		}
		c.observe(ctx, ref)
	}
}

func (c *agentCoordinator) reconcileKey(ctx context.Context, key provider.RootKey) {
	refs := c.refreshTrackedRoots()
	for _, ref := range refs {
		if ref.Key() == key {
			c.observe(ctx, ref)
			return
		}
	}
}

// refreshTrackedRoots takes only a detached state snapshot. Observer Forget and
// history-lane cleanup happen after that store read lock has been released.
func (c *agentCoordinator) refreshTrackedRoots() []provider.RootRef {
	snap := c.store.Snapshot()
	refs := make([]provider.RootRef, 0, len(snap.Sessions))
	live := make(map[provider.RootKey]provider.RootRef, len(snap.Sessions))
	for _, sess := range snap.Sessions {
		ref, ok := providerRootRef(sess)
		if !ok {
			continue
		}
		refs = append(refs, ref)
		live[ref.Key()] = ref
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].PID != refs[j].PID {
			return refs[i].PID < refs[j].PID
		}
		return refs[i].StartedAt.Before(refs[j].StartedAt)
	})

	type forgotten struct {
		key  provider.RootKey
		root trackedProviderRoot
	}
	var gone []forgotten
	var retired []trackedProviderRoot
	var codexBindings []provider.RootRef
	c.mu.Lock()
	for key, root := range c.tracked {
		if _, ok := live[key]; !ok {
			c.generation[key]++
			gone = append(gone, forgotten{key: key, root: root})
			delete(c.tracked, key)
		}
	}
	for key, ref := range live {
		root := c.tracked[key]
		root.kind = ref.Provider
		root.provider = c.observer(ref.Provider)
		if ref.ProviderSessionID != "" {
			priorRootID := root.rootID
			if root.rootID != "" && root.rootID != ref.ProviderSessionID {
				retired = append(retired, root)
				root.restored = false
			}
			root.rootID = ref.ProviderSessionID
			if ref.Provider == agentgraph.ProviderCodex && priorRootID != ref.ProviderSessionID {
				codexBindings = append(codexBindings, ref)
			}
		}
		c.tracked[key] = root
	}
	c.mu.Unlock()
	for _, item := range gone {
		c.forgetCodexHookState(item.key)
		if item.root.provider != nil {
			item.root.provider.Forget(item.key)
		}
		if item.root.rootID != "" {
			c.history.Forget(item.root.kind, item.root.rootID)
		}
		c.cancelCodexNaming(item.key, true)
	}
	for _, root := range retired {
		c.history.Forget(root.kind, root.rootID)
	}
	for _, ref := range codexBindings {
		if c.codex == nil {
			break
		}
		if err := c.codex.RegisterHookBinding(ref.Key(), ref.ProviderSessionID); err != nil {
			c.recordDiagnostic(ref.Provider, "binding_conflict", time.Now())
		}
	}
	return refs
}

func providerRootRef(sess state.Session) (provider.RootRef, bool) {
	var kind agentgraph.ProviderKind
	switch sess.Agent {
	case state.AgentKindClaude:
		kind = agentgraph.ProviderClaude
	case state.AgentKindCodex:
		kind = agentgraph.ProviderCodex
	default:
		return provider.RootRef{}, false
	}
	ref := provider.RootRef{PID: sess.PID, StartedAt: sess.StartedAt, Provider: kind, CWD: sess.CWD}
	if info := sess.Enrichment(); info != nil {
		ref.ProviderSessionID = info.SessionID
		ref.Transcript = info.Transcript
	}
	if ref.ProviderSessionID == "" && sess.AgentGraph != nil {
		ref.ProviderSessionID = sess.AgentGraph.RootID
	}
	return ref, ref.PID > 0 && !ref.StartedAt.IsZero()
}

func (c *agentCoordinator) observer(kind agentgraph.ProviderKind) provider.Observer {
	switch kind {
	case agentgraph.ProviderClaude:
		if c.claude == nil {
			return nil
		}
		return c.claude
	case agentgraph.ProviderCodex:
		if c.codex == nil {
			return nil
		}
		return c.codex
	default:
		return nil
	}
}

func (c *agentCoordinator) observe(ctx context.Context, ref provider.RootRef) {
	observer := c.observer(ref.Provider)
	if observer == nil {
		return
	}
	if ref.Provider == agentgraph.ProviderClaude {
		if ref.ProviderSessionID == "" {
			return // exact Claude identity has not arrived; never bind by cwd
		}
		c.restoreClaude(ref)
	}
	generation := c.begin(ref.Key())
	now := time.Now()
	if ref.Provider == agentgraph.ProviderCodex {
		defer func() { c.reconcileCodexChildHooks(ref, time.Now()) }()
	}
	observation, err := observer.Observe(ctx, ref, now)
	if err != nil {
		c.recordDiagnostic(ref.Provider, "observe_error", now)
	}
	if observation.RootID == "" || len(observation.Nodes) == 0 {
		category := "snapshot_pending"
		if ref.Provider == agentgraph.ProviderCodex && observation.RootID == "" {
			category = "exact_binding_unavailable"
		}
		c.recordDiagnostic(ref.Provider, category, now)
		c.expireCurrent(ref, generation, now)
		return
	}
	compat := claudeprovider.Compatibility{}
	if ref.Provider == agentgraph.ProviderClaude {
		compat = c.claude.Projection(ref.Key())
	}
	if c.applyObservation(ref, generation, observation, compat, now) && ref.Provider == agentgraph.ProviderClaude {
		for _, event := range c.claude.DrainLegacyEvents(ref.Key()) {
			c.sink.Record(event)
		}
	}
}

func (c *agentCoordinator) restoreClaude(ref provider.RootRef) {
	key := ref.Key()
	c.mu.Lock()
	tracked := c.tracked[key]
	if tracked.restored {
		c.mu.Unlock()
		return
	}
	tracked.restored = true
	c.tracked[key] = tracked
	c.mu.Unlock()

	sess, ok := sessionForKey(c.store.Snapshot(), key)
	if !ok || sess.Claude == nil {
		return
	}
	restored := compatibilityFromState(sess.Claude)
	observation, err := c.claude.Restore(ref, restored, time.Now())
	if err != nil {
		c.recordDiagnostic(agentgraph.ProviderClaude, "restore_error", time.Now())
		return
	}
	generation := c.begin(key)
	c.applyObservation(ref, generation, observation, c.claude.Projection(key), time.Now())
}

func (c *agentCoordinator) begin(key provider.RootKey) uint64 {
	c.mu.Lock()
	c.generation[key]++
	generation := c.generation[key]
	c.mu.Unlock()
	return generation
}

func (c *agentCoordinator) current(key provider.RootKey, generation uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.generation[key] == generation
}

func (c *agentCoordinator) applyObservation(ref provider.RootRef, generation uint64, observation agentgraph.Observation, compat claudeprovider.Compatibility, now time.Time) bool {
	return c.applyObservationWithHookOwnership(ref, generation, observation, compat, now, false)
}

func (c *agentCoordinator) applyObservationWithHookOwnership(ref provider.RootRef, generation uint64, observation agentgraph.Observation, compat claudeprovider.Compatibility, now time.Time, hookOwnsTransition bool) bool {
	if ref.Provider == agentgraph.ProviderCodex {
		observation = c.overlayCodexHookRootObservation(ref.Key(), observation, now)
		observation = c.overlayCodexPendingObservation(ref.Key(), observation, now)
		observation = c.overlayCodexChildObservation(ref.Key(), observation, now)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generation[ref.Key()] != generation {
		return false
	}
	snap := c.store.Snapshot()
	priorSession, ok := sessionForKey(snap, ref.Key())
	if !ok || agentgraph.ProviderKind(priorSession.Agent) != ref.Provider {
		return false
	}
	if !hookOwnsTransition && !shouldApplyObservation(observation, priorSession.AgentGraph, now) {
		return false
	}
	graph, err := state.ProjectAgentGraph(observation, priorSession.AgentGraph, now)
	if err != nil {
		c.recordDiagnosticLocked(ref.Provider, "invalid_observation", now)
		return false
	}
	canonical, err := c.history.Project(history.AgentStateContext{PID: ref.PID, CWD: ref.CWD}, observation, now)
	if err != nil {
		c.recordDiagnosticLocked(ref.Provider, "history_projection_error", now)
		canonical = nil
	}
	beforeStatus, beforeSince := "", time.Time{}
	if info := priorSession.Enrichment(); info != nil {
		beforeStatus, beforeSince = info.Status, info.StatusSince
	}
	applied, nativeOverride := false, false
	nativeName, hasNativeName := observationRootName(observation)
	authoritativeNativeName := ref.Provider == agentgraph.ProviderCodex &&
		observation.Source == agentgraph.SourceCodexAppServer && observation.Complete && hasNativeName
	c.store.Apply(func(sessions map[int]*state.Session) {
		sess := sessions[ref.PID]
		if sess == nil || !sess.StartedAt.Equal(ref.StartedAt) || agentgraph.ProviderKind(sess.Agent) != ref.Provider {
			return
		}
		if ref.Provider == agentgraph.ProviderClaude {
			applyClaudeCompatibility(sess.AgentBlock(state.AgentKindClaude), compat)
		}
		sess.SetAgentGraph(graph)
		if ref.Provider == agentgraph.ProviderCodex && sess.DisplayName != nil {
			if !sess.DisplayName.ValidFor(observation.RootID) {
				sess.DisplayName = nil
			} else if authoritativeNativeName {
				if sess.DisplayName.NativeBaseline == nil {
					baseline := nativeName
					sess.DisplayName.NativeBaseline = &baseline
				} else if nativeName != *sess.DisplayName.NativeBaseline {
					sess.DisplayName = nil
					nativeOverride = true
				}
			}
		}
		applied = true
	})
	if !applied {
		c.history.Forget(observation.Provider, observation.RootID)
		return false
	}
	if nativeOverride {
		c.recordDiagnosticLocked(agentgraph.ProviderCodex, "native-override", now)
	}
	root := c.tracked[ref.Key()]
	if root.rootID != "" && root.rootID != observation.RootID {
		c.history.Forget(root.kind, root.rootID)
	}
	root.provider, root.kind, root.rootID = c.observer(ref.Provider), ref.Provider, observation.RootID
	c.tracked[ref.Key()] = root
	for _, event := range canonical {
		c.sink.Record(event)
	}
	if graph.Summary.Status != beforeStatus {
		c.sink.Record(history.Event{
			Ts: now, Type: history.EventTransition, SessionID: observation.RootID,
			PID: ref.PID, Agent: string(ref.Provider), CWD: ref.CWD,
			From: beforeStatus, To: graph.Summary.Status,
			Rule: "agent_graph_authority", DurPrevMs: history.HeldMs(beforeSince, now),
			Subagents: graph.Summary.LiveChildren,
		})
	}
	return true
}

func observationRootName(observation agentgraph.Observation) (string, bool) {
	for _, node := range observation.Nodes {
		if node.ID == observation.RootID {
			return strings.TrimSpace(node.Nickname), true
		}
	}
	return "", false
}

func shouldApplyObservation(observation agentgraph.Observation, current *state.AgentGraph, now time.Time) bool {
	if observation.RootID == "" || len(observation.Nodes) == 0 {
		return false
	}
	if current == nil || current.RootID != observation.RootID {
		return true
	}
	currentFresh := current.Fresh(now)
	candidateFresh := observation.Fresh(now)
	if observation.Provider == agentgraph.ProviderCodex {
		// Exact event time is the first authority within one Codex conversation.
		// Hooks can therefore provide the immediate edge while a subsequent
		// app-server sample corrects it, and a delayed sample can never repaint
		// over a newer hook. Claude retains its established hook/transcript policy.
		if observation.ObservedAt.Before(current.ObservedAt) {
			return false
		}
		if observation.ObservedAt.After(current.ObservedAt) {
			if currentFresh && !candidateFresh && current.Source != observation.Source {
				return false
			}
			return true
		}
	}
	currentRank := sourceRank(current.Source)
	candidateRank := sourceRank(observation.Source)
	if currentFresh && currentRank > candidateRank {
		return false
	}
	if candidateFresh && candidateRank > currentRank {
		return true
	}
	if currentFresh && !candidateFresh && current.Source != observation.Source {
		return false
	}
	return !observation.ObservedAt.Before(current.ObservedAt)
}

// overlayCodexHookObservation keeps the app-server's structural detail while
// applying a newer hook's immediate root status. A hook is intentionally
// partial: it must not erase the authoritative thread name or child graph.
func overlayCodexHookObservation(hook agentgraph.Observation, current *state.AgentGraph) agentgraph.Observation {
	if current == nil || current.RootID != hook.RootID || len(hook.Nodes) != 1 {
		return hook
	}
	overlay := observationFromState(agentgraph.ProviderCodex, current)
	overlay.Source = agentgraph.SourceHook
	overlay.ObservedAt = hook.ObservedAt
	overlay.FreshUntil = hook.FreshUntil
	overlay.Complete = false
	for i := range overlay.Nodes {
		if overlay.Nodes[i].ID != hook.RootID {
			continue
		}
		overlay.Nodes[i].Runtime = hook.Nodes[0].Runtime
		overlay.Nodes[i].Attention = hook.Nodes[0].Attention
		overlay.Nodes[i].Lifecycle = hook.Nodes[0].Lifecycle
		overlay.Nodes[i].UpdatedAt = hook.Nodes[0].UpdatedAt
		break
	}
	return overlay
}

func sourceRank(source agentgraph.SourceKind) int {
	switch source {
	case agentgraph.SourceCodexAppServer, agentgraph.SourceClaudeTranscript:
		return 4
	case agentgraph.SourceHook:
		return 3
	case agentgraph.SourceCodexRollout:
		return 2
	case agentgraph.SourceRestoredLastKnown:
		return 1
	default:
		return 0
	}
}

func (c *agentCoordinator) expireCurrent(ref provider.RootRef, generation uint64, now time.Time) {
	if !c.current(ref.Key(), generation) {
		return
	}
	sess, ok := sessionForKey(c.store.Snapshot(), ref.Key())
	if !ok || sess.AgentGraph == nil || sess.AgentGraph.Fresh(now) {
		return
	}
	observation := observationFromState(ref.Provider, sess.AgentGraph)
	compat := claudeprovider.Compatibility{}
	if ref.Provider == agentgraph.ProviderClaude {
		compat = compatibilityFromState(sess.Claude)
	}
	c.applyObservation(ref, generation, observation, compat, now)
}

func observationFromState(kind agentgraph.ProviderKind, graph *state.AgentGraph) agentgraph.Observation {
	observation := agentgraph.Observation{
		Provider: kind, RootID: graph.RootID, Source: graph.Source,
		ObservedAt: graph.ObservedAt, FreshUntil: graph.FreshUntil, Complete: graph.Complete,
		Nodes: make([]agentgraph.Node, len(graph.Nodes)),
	}
	for i, node := range graph.Nodes {
		observation.Nodes[i] = agentgraph.Node{
			ID: node.ID, ParentID: node.ParentID, Nickname: node.Nickname, Role: node.Role,
			Description: node.Description, Runtime: node.Runtime, Attention: node.Attention,
			Lifecycle: node.Lifecycle, StartedAt: node.StartedAt, UpdatedAt: node.UpdatedAt,
			CompletedAt: node.CompletedAt,
			Usage: agentgraph.Usage{
				InputTokens: node.Usage.InputTokens, CachedInputTokens: node.Usage.CachedInputTokens,
				CacheWriteInputTokens: node.Usage.CacheWriteInputTokens, OutputTokens: node.Usage.OutputTokens,
				ReasoningOutputTokens: node.Usage.ReasoningOutputTokens, TotalTokens: node.Usage.TotalTokens,
				ModelContextWindow: node.Usage.ModelContextWindow,
			},
		}
	}
	return observation
}

func sessionForKey(snapshot state.Snapshot, key provider.RootKey) (state.Session, bool) {
	for _, sess := range snapshot.Sessions {
		if sess.PID == key.PID && sess.StartedAt.Equal(key.StartedAt) {
			return sess, true
		}
	}
	return state.Session{}, false
}

func compatibilityFromState(info *state.AgentInfo) claudeprovider.Compatibility {
	if info == nil {
		return claudeprovider.Compatibility{}
	}
	compat := claudeprovider.Compatibility{
		SessionID: info.SessionID, Transcript: info.Transcript, Status: info.Status,
		StatusSince: info.StatusSince, InFlightSubagents: info.InFlightSubagents,
		PendingWriters: append([]string(nil), info.PendingWriters...), PendingTool: info.PendingTool,
		Pending:   make(map[string]claudeprovider.PendingPrompt, len(info.Pending)),
		Workflows: make([]fanout.Workflow, len(info.Workflows)),
	}
	for writer, prompt := range info.Pending {
		compat.Pending[writer] = claudeprovider.PendingPrompt{
			Tool: prompt.Tool, InputHash: prompt.InputHash, Attention: agentgraph.AttentionApproval, Since: prompt.Since,
		}
	}
	for i, workflow := range info.Workflows {
		compat.Workflows[i] = fanout.Workflow{
			RunID: workflow.RunID, Name: workflow.Name, AgentsStarted: workflow.AgentsStarted,
			AgentsDone: workflow.AgentsDone, InFlight: workflow.InFlight,
		}
	}
	return compat
}

func applyClaudeCompatibility(info *state.AgentInfo, compat claudeprovider.Compatibility) {
	info.SessionID, info.Transcript = compat.SessionID, compat.Transcript
	info.Status, info.StatusSince = compat.Status, compat.StatusSince
	info.InFlightSubagents = compat.InFlightSubagents
	info.Workflows = make([]state.WorkflowStatus, len(compat.Workflows))
	for i, workflow := range compat.Workflows {
		info.Workflows[i] = state.WorkflowStatus{
			RunID: workflow.RunID, Name: workflow.Name, AgentsStarted: workflow.AgentsStarted,
			AgentsDone: workflow.AgentsDone, InFlight: workflow.InFlight,
		}
	}
	info.Pending = make(map[string]state.PendingPrompt, len(compat.Pending))
	for writer, prompt := range compat.Pending {
		info.Pending[writer] = state.PendingPrompt{Tool: prompt.Tool, InputHash: prompt.InputHash, Since: prompt.Since}
	}
	if len(info.Pending) == 0 {
		info.Pending = nil
	}
	info.PendingWriters = append([]string(nil), compat.PendingWriters...)
	info.PendingTool = compat.PendingTool
}

// HandleHook is the RPC graph-aware hook callback. The incoming Claude
// AgentID is intentionally passed raw exactly once; the adapter performs its
// own canonicalization. Codex hook status is only a fallback beneath a fresh
// app-server observation.
func (c *agentCoordinator) HandleHook(req rpc.Request, sess state.Session) {
	ref, ok := providerRootRef(sess)
	if !ok {
		return
	}
	agent := req.Agent
	if agent == "" {
		agent = state.AgentKindClaude
	}
	if agentgraph.ProviderKind(agent) != ref.Provider {
		c.recordDiagnostic(ref.Provider, "hook_provider_mismatch", time.Now())
		return
	}
	if req.SessionID != "" && ref.Provider != agentgraph.ProviderCodex {
		ref.ProviderSessionID = req.SessionID
	}
	if req.Transcript != "" {
		ref.Transcript = req.Transcript
	}
	now := req.ObservedAt
	if now.IsZero() {
		now = time.Now()
	}
	switch ref.Provider {
	case agentgraph.ProviderClaude:
		if c.claude == nil {
			return
		}
		if ref.ProviderSessionID == "" {
			c.recordDiagnostic(ref.Provider, "exact_binding_unavailable", now)
			return
		}
		c.restoreClaude(ref)
		generation := c.begin(ref.Key())
		result := c.claude.ApplyHook(claudeprovider.HookSignal{
			Root: ref, Event: req.Event, AgentID: req.AgentID, AgentType: req.AgentType,
			ToolName: req.ToolName, ToolInputHash: req.ToolInputHash, At: now,
		})
		if !result.Applied {
			return
		}
		comparison := claudeprovider.CompareShadow(result.Projection.Status, result.Observation, agentgraph.Summary{}, now)
		if !comparison.Match {
			c.recordDiagnostic(ref.Provider, comparison.Rule, now)
		}
		c.applyObservation(ref, generation, result.Observation, result.Projection, now)
	case agentgraph.ProviderCodex:
		rootID := req.SessionID
		if rootID == "" {
			rootID = ref.ProviderSessionID
		}
		if rootID == "" {
			c.recordDiagnostic(ref.Provider, "exact_binding_unavailable", now)
			return
		}
		if req.Event == "SubagentStart" || req.Event == "SubagentStop" {
			c.enqueueCodexChildHook(ref, req, rootID, now)
			return
		}
		if shouldSettleCodexSessionStart(req) {
			c.deferCodexSessionStart(ref, req, rootID, now)
			return
		}
		acceptedStart, introduced := c.consumeCodexSessionStart(ref.Key(), rootID)
		if !acceptedStart {
			c.recordDiagnostic(ref.Provider, "stale_observation_rejected", now)
			return
		}
		c.handleCodexHookNow(ref, req, rootID, now, introduced)
	}
}

// reconcileCodexBinding applies an exact hook conversation ID to one process
// lifetime before reducing that same hook. A rotation clears only
// conversation-bound display metadata; the prior graph stays visible until the
// new hook observation lands immediately afterwards.
func (c *agentCoordinator) reconcileCodexBinding(ref provider.RootRef, threadID string, now time.Time) (provider.RootRef, bool) {
	accepted := false
	rotated := false
	c.store.Apply(func(sessions map[int]*state.Session) {
		sess := sessions[ref.PID]
		if sess == nil || !sess.StartedAt.Equal(ref.StartedAt) || sess.Agent != state.AgentKindCodex {
			return
		}
		currentID := ""
		if info := sess.Enrichment(); info != nil {
			currentID = info.SessionID
		}
		if currentID == "" && sess.AgentGraph != nil {
			currentID = sess.AgentGraph.RootID
		}
		rotated = currentID != "" && currentID != threadID
		if rotated || (sess.DisplayName != nil && !sess.DisplayName.ValidFor(threadID)) {
			sess.DisplayName = nil
		}
		// Keep the prior graph visible until the new hook observation lands in the
		// following atomic store update. ProjectAgentGraph ignores a prior summary
		// from another root, so this avoids publishing a transient empty status.
		sess.AgentBlock(state.AgentKindCodex).SessionID = threadID
		accepted = true
	})
	if rotated {
		c.cancelCodexNaming(ref.Key(), true)
		c.mu.Lock()
		c.generation[ref.Key()]++
		if tracked := c.tracked[ref.Key()]; tracked.rootID != "" {
			c.history.Forget(tracked.kind, tracked.rootID)
			tracked.rootID = ""
			c.tracked[ref.Key()] = tracked
		}
		c.mu.Unlock()
		c.recordDiagnostic(ref.Provider, "conversation_rotated", now)
	}
	ref.ProviderSessionID = threadID
	return ref, accepted
}

func (c *agentCoordinator) SetCodexDisplayNamer(namer codexprovider.NameGenerator, model string) {
	if namer != nil {
		c.namer = namer
	}
	if strings.TrimSpace(model) != "" {
		c.namingModel = strings.TrimSpace(model)
	}
}

func (c *agentCoordinator) retainCodexNamingCandidate(ref provider.RootRef, conversationID, turnID, prompt string, at time.Time) {
	prompt = boundedNamingText(prompt)
	if conversationID == "" || prompt == "" {
		return
	}
	if sess, ok := sessionForKey(c.store.Snapshot(), ref.Key()); !ok ||
		conversationIDForSession(sess) != conversationID || sess.DisplayName.ValidFor(conversationID) {
		return
	}
	canceled := false
	c.namingMu.Lock()
	naming := c.naming[ref.Key()]
	if naming == nil || naming.conversationID != conversationID {
		if naming != nil && naming.cancel != nil {
			naming.cancel()
			canceled = true
		}
		naming = &codexNamingState{conversationID: conversationID}
		c.naming[ref.Key()] = naming
	}
	if (!naming.completedAt.IsZero() && !at.After(naming.completedAt)) ||
		(naming.candidate != nil && !at.After(naming.candidate.at)) {
		c.namingMu.Unlock()
		return
	}
	cwdBase := ""
	if strings.TrimSpace(ref.CWD) != "" {
		cwdBase = filepath.Base(ref.CWD)
	}
	naming.candidate = &codexNamingCandidate{turnID: turnID, prompt: prompt, at: at, cwdBase: cwdBase}
	c.namingMu.Unlock()
	if canceled {
		c.recordDiagnostic(agentgraph.ProviderCodex, "canceled", at)
	}
}

func (c *agentCoordinator) completeCodexNaming(ref provider.RootRef, conversationID, turnID, response string, at time.Time) {
	response = boundedNamingText(response)
	var input codexNamingInput
	var ctx context.Context
	var canceled bool
	c.namingMu.Lock()
	naming := c.naming[ref.Key()]
	if naming == nil || naming.conversationID != conversationID || naming.candidate == nil {
		c.namingMu.Unlock()
		return
	}
	candidate := naming.candidate
	if !at.After(candidate.at) || (candidate.turnID != "" && turnID != "" && candidate.turnID != turnID) {
		c.namingMu.Unlock()
		return
	}
	naming.candidate = nil
	naming.completedAt = at
	if response == "" {
		c.namingMu.Unlock()
		c.recordDiagnostic(agentgraph.ProviderCodex, "canceled", at)
		return
	}
	if naming.cancel != nil {
		naming.cancel()
		canceled = true
	}
	naming.attempt++
	ctx, naming.cancel = context.WithCancel(context.Background())
	input = codexNamingInput{
		key: ref.Key(), conversationID: conversationID, attempt: naming.attempt,
		context: codexprovider.NamingContext{
			CWDBase: candidate.cwdBase, UserPrompt: candidate.prompt, AssistantResponse: response,
		},
	}
	c.namingMu.Unlock()
	sess, eligible := sessionForKey(c.store.Snapshot(), input.key)
	if !eligible || conversationIDForSession(sess) != conversationID || sess.DisplayName.ValidFor(conversationID) {
		c.cancelCodexNaming(input.key, false)
		return
	}
	if canceled {
		c.recordDiagnostic(agentgraph.ProviderCodex, "canceled", at)
	}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		committed := c.runCodexNaming(ctx, input)
		c.namingMu.Lock()
		if naming := c.naming[input.key]; naming != nil && naming.conversationID == input.conversationID && naming.attempt == input.attempt {
			naming.cancel = nil
			if committed || naming.candidate == nil {
				delete(c.naming, input.key)
			}
		}
		c.namingMu.Unlock()
	}()
}

func (c *agentCoordinator) runCodexNaming(ctx context.Context, input codexNamingInput) bool {
	name := ""
	origin := state.DisplayNameGenerated
	timeout := c.namingTimeout
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	for range 2 {
		attemptCtx, cancel := context.WithTimeout(ctx, timeout)
		candidate, err := c.namer.Generate(attemptCtx, input.context, c.namingModel)
		cancel()
		if err == nil {
			if normalized, ok := codexprovider.NormalizeGeneratedName(candidate); ok {
				name = normalized
				break
			}
		}
		if ctx.Err() != nil {
			return false
		}
	}
	if name == "" {
		name = codexprovider.FallbackName(input.context)
		origin = state.DisplayNameFallback
	}
	if ctx.Err() != nil || !c.currentCodexNamingAttempt(input) {
		return false
	}
	committed := false
	c.store.Apply(func(sessions map[int]*state.Session) {
		sess := sessions[input.key.PID]
		if sess == nil || !sess.StartedAt.Equal(input.key.StartedAt) || sess.Agent != state.AgentKindCodex ||
			conversationIDForSession(*sess) != input.conversationID || sess.DisplayName.ValidFor(input.conversationID) ||
			!c.currentCodexNamingAttempt(input) {
			return
		}
		record := &state.DisplayName{Value: name, Origin: origin, ConversationID: input.conversationID}
		if baseline, ok := authoritativeNativeName(*sess, input.conversationID); ok {
			record.NativeBaseline = &baseline
		}
		sess.DisplayName = record
		committed = true
	})
	if !committed {
		c.recordDiagnostic(agentgraph.ProviderCodex, "stale-result", time.Now())
		return false
	}
	if origin == state.DisplayNameFallback {
		c.recordDiagnostic(agentgraph.ProviderCodex, "fallback", time.Now())
	} else {
		c.recordDiagnostic(agentgraph.ProviderCodex, "generated", time.Now())
	}
	return true
}

func (c *agentCoordinator) currentCodexNamingAttempt(input codexNamingInput) bool {
	c.namingMu.Lock()
	defer c.namingMu.Unlock()
	naming := c.naming[input.key]
	return naming != nil && naming.conversationID == input.conversationID && naming.attempt == input.attempt
}

func (c *agentCoordinator) cancelCodexNaming(key provider.RootKey, diagnostic bool) {
	c.namingMu.Lock()
	naming := c.naming[key]
	if naming != nil && naming.cancel != nil {
		naming.cancel()
	}
	delete(c.naming, key)
	c.namingMu.Unlock()
	if diagnostic && naming != nil {
		c.recordDiagnostic(agentgraph.ProviderCodex, "canceled", time.Now())
	}
}

func boundedNamingText(value string) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) > 1000 {
		runes = runes[:1000]
	}
	return string(runes)
}

func conversationIDForSession(sess state.Session) string {
	if info := sess.Enrichment(); info != nil && strings.TrimSpace(info.SessionID) != "" {
		return strings.TrimSpace(info.SessionID)
	}
	if sess.AgentGraph != nil {
		return strings.TrimSpace(sess.AgentGraph.RootID)
	}
	return ""
}

func authoritativeNativeName(sess state.Session, conversationID string) (string, bool) {
	graph := sess.AgentGraph
	if graph == nil || graph.RootID != conversationID {
		return "", false
	}
	for _, node := range graph.Nodes {
		if node.ID == conversationID {
			name := strings.TrimSpace(node.Nickname)
			if graph.Source == agentgraph.SourceCodexAppServer && graph.Complete {
				return name, true
			}
			// Hook observations cannot carry a native name. A nonempty name on a
			// hook graph can only have survived overlayCodexHookObservation from
			// the preceding complete app-server graph, so it remains authoritative
			// display metadata even while the hook owns the current status edge.
			if graph.Source == agentgraph.SourceHook && name != "" {
				return name, true
			}
			return "", false
		}
	}
	return "", false
}

func (c *agentCoordinator) recordDiagnostic(provider agentgraph.ProviderKind, category string, at time.Time) {
	c.mu.Lock()
	c.recordDiagnosticLocked(provider, category, at)
	c.mu.Unlock()
}

func (c *agentCoordinator) recordDiagnosticLocked(provider agentgraph.ProviderKind, category string, at time.Time) {
	key := string(provider) + ":" + category
	diagnostic := c.diagnostics[key]
	diagnostic.Provider, diagnostic.Category = string(provider), category
	diagnostic.Count++
	if diagnostic.LastAt.IsZero() || at.After(diagnostic.LastAt) {
		diagnostic.LastAt = at
	}
	c.diagnostics[key] = diagnostic
	if last := c.lastLog[key]; last.IsZero() || at.Sub(last) >= time.Minute {
		log.Printf("agent-observer: provider=%s category=%s count=%d", provider, category, diagnostic.Count)
		c.lastLog[key] = at
	}
}

func (c *agentCoordinator) Diagnostics() []rpc.AgentDiagnostic {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]rpc.AgentDiagnostic, 0, len(c.diagnostics))
	for _, diagnostic := range c.diagnostics {
		out = append(out, diagnostic)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Category < out[j].Category
	})
	return out
}
