package main

import (
	"context"
	"fmt"
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
	"github.com/tjmisko/switchboard/internal/statusexplain"
	"github.com/tjmisko/switchboard/internal/statustune"
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
	DrainResolutionRule(provider.RootKey) string
	DrainPromptDiagnostics(provider.RootKey) []string
	DrainLegacyEvents(provider.RootKey) []history.Event
}

type codexObserver interface {
	provider.Observer
	RegisterHookBinding(provider.RootKey, string) error
}

type codexBindingReconciler interface {
	ReconcileHookBinding(provider.RootKey, string) (codexprovider.BindingUpdate, error)
}

type codexRolloutBinder interface {
	RegisterHookRollout(provider.RootKey, string, string) error
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
	// admissions is graph admission's part of each root's decision record,
	// read by Explain (status_explain.go).
	admissions map[provider.RootKey]*admissionRecord

	// clock is the coordinator's time source for the decisions it starts on
	// its own (a tick, a timer, a hook with no stamp); nil means the wall
	// clock. Every status decision below takes the instant as a parameter.
	clock func() time.Time

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

	codexLimitMu    sync.Mutex
	codexLimitScans map[provider.RootKey]*codexLimitScan

	// piMu serializes the Pi hook reducer (pi_hooks.go) per daemon; it is
	// taken before the store lock, never under it. piTails caches the session
	// file reads that stand in for hooks after a restart (pi_session_file.go).
	piMu    sync.Mutex
	piRoots map[provider.RootKey]*piHookRoot
	piTails map[provider.RootKey]*piSessionTail
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
		piRoots: make(map[provider.RootKey]*piHookRoot), piTails: make(map[provider.RootKey]*piSessionTail),
	}
}

// now is the coordinator's clock.
func (c *agentCoordinator) now() time.Time {
	if c.clock == nil {
		return time.Now()
	}
	return c.clock()
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
	return provider.RootKey{PID: sess.PID, StartedAt: sess.StartedAt, Birth: sess.Birth}
}

// sessionHoldsRoot reports whether a tracked session is still the process
// lifetime a root key names: the same pid, discovery stamp and birth token.
// Every asynchronous result, timer and hook landing compares through it, so
// one gathered for an old lifetime cannot modify that pid's replacement (#97).
func sessionHoldsRoot(s *state.Session, key provider.RootKey) bool {
	return s != nil && s.PID == key.PID && s.StartedAt.Equal(key.StartedAt) && s.Birth == key.Birth
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
	c.reconcilePiRoots(c.now())
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
			delete(c.admissions, key)
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
	ref := provider.RootRef{PID: sess.PID, StartedAt: sess.StartedAt, Birth: sess.Birth, Provider: kind, CWD: sess.CWD}
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
	c.observeAt(ctx, ref, c.now())
}

// observeAt is observe with the tick's instant supplied. The clock is a
// parameter because some provider rules are about the DISTANCE between two
// ticks, not about either one — the call-identity latch refuses a confirmation
// taken too soon after its proposal — and a test driving two ticks in a row
// cannot express that against a wall clock it does not control.
func (c *agentCoordinator) observeAt(ctx context.Context, ref provider.RootRef, now time.Time) {
	if ref.Provider == agentgraph.ProviderCodex {
		c.scanCodexUsageLimit(ref, now)
		c.pollCodexStoppedRoot(ref, now)
	}
	observer := c.observer(ref.Provider)
	if observer == nil {
		c.recordObserveOutcome(ref, statusexplain.ReasonCoverageUnsupported)
		return
	}
	if ref.Provider == agentgraph.ProviderClaude {
		if ref.ProviderSessionID == "" {
			c.recordObserveOutcome(ref, statusexplain.ReasonBindingMissing)
			return // exact Claude identity has not arrived; never bind by cwd
		}
		c.restoreClaude(ref, now)
	}
	generation := c.begin(ref.Key())
	if ref.Provider == agentgraph.ProviderCodex {
		defer func() { c.reconcileCodexChildHooks(ref, now) }()
	}
	observation, err := observer.Observe(ctx, ref, now)
	if err != nil {
		c.recordDiagnostic(ref.Provider, "observe_error", now)
	}
	if observation.RootID == "" || len(observation.Nodes) == 0 {
		category := "snapshot_pending"
		if ref.Provider == agentgraph.ProviderCodex && observation.RootID == "" {
			category = "exact_binding_unavailable"
			c.recordObserveOutcome(ref, statusexplain.ReasonBindingMissing)
		}
		c.recordDiagnostic(ref.Provider, category, now)
		c.expireCurrent(ref, generation, now)
		return
	}
	compat, rule := claudeprovider.Compatibility{}, ""
	if ref.Provider == agentgraph.ProviderClaude {
		compat = c.claude.Projection(ref.Key())
		// Drained unconditionally, whether or not this tick moves the chip: the
		// rule belongs to the tick that computed it, and leaving it behind would
		// let it explain some later edge instead.
		rule = c.claude.DrainResolutionRule(ref.Key())
		// P3 and the reachability of the id fast path, drained on the same
		// principle: the tick that produced a category is the tick that owns it.
		// Bounded categories only — never a writer, a tool, or a call id.
		for _, category := range c.claude.DrainPromptDiagnostics(ref.Key()) {
			c.recordDiagnostic(ref.Provider, category, now)
		}
	}
	if c.applyObservationAs(ref, generation, observation, compat, now, rule, observedKind(observation)) && ref.Provider == agentgraph.ProviderClaude {
		for _, event := range c.claude.DrainLegacyEvents(ref.Key()) {
			c.sink.Record(event)
		}
	}
}

// observedKind is what an observer's answer is as evidence: a provider
// snapshot, unless the observer held its previous observation over a failed
// read, which keeps the provenance it had (a restore, a hook edge) and so must
// not be promoted to a snapshot by being returned from Observe.
func observedKind(observation agentgraph.Observation) state.GraphKind {
	switch observation.Source {
	case agentgraph.SourceRestoredLastKnown:
		return state.GraphRestored
	case agentgraph.SourceHook:
		return state.GraphHookEvent
	default:
		return state.GraphSnapshot
	}
}

// restoreClaude lands, once per tracked root, the Claude adapter's rebuild of
// the persisted compatibility block: restored last-known evidence, at now.
func (c *agentCoordinator) restoreClaude(ref provider.RootRef, now time.Time) {
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
	observation, err := c.claude.Restore(ref, restored, now)
	if err != nil {
		c.recordDiagnostic(agentgraph.ProviderClaude, "restore_error", now)
		return
	}
	generation := c.begin(key)
	c.applyObservationAs(ref, generation, observation, c.claude.Projection(key), now, "", state.GraphRestored)
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

// applyObservation lands an observer's sample: a provider snapshot.
func (c *agentCoordinator) applyObservation(ref provider.RootRef, generation uint64, observation agentgraph.Observation, compat claudeprovider.Compatibility, now time.Time) bool {
	return c.applyObservationAs(ref, generation, observation, compat, now, "", state.GraphSnapshot)
}

// applyObservationAs is the landing path. kind is what the observation is as
// evidence (a snapshot, a hook's event, a partial child edge, a transcript
// tail, a restore), stated by the caller that produced it; the status resolver
// weighs it with the root's other evidence (state.Session.LandAgentGraph). A
// graph only displaces the latest graph of its own kind, never another kind's:
// selection happens in the resolver, not here.
//
// rule is the content-free id of the decision that produced this observation —
// a hook edge's HookResult.Rule or an Observe tick's drained resolution reason —
// and is recorded on the transition, if any, that the observation causes. An
// empty rule falls back to RuleGraphAuthority: the provider attributed nothing,
// which is the honest record for a Codex edge, a restore, or a Claude
// observation whose transition came from fanout topology rather than from a
// prompt.
//
// The rule EXPLAINS the transition; it never decides one. Nothing below reads it
// except the history event.
func (c *agentCoordinator) applyObservationAs(ref provider.RootRef, generation uint64, observation agentgraph.Observation, compat claudeprovider.Compatibility, now time.Time, rule string, kind state.GraphKind) bool {
	landing := state.GraphLanding{Kind: kind}
	if ref.Provider == agentgraph.ProviderCodex {
		own := codexObservationRootAttention(observation)
		observation = c.overlayCodexHookRootObservation(ref.Key(), observation, now)
		observation = c.overlayCodexPendingObservation(ref.Key(), observation, now)
		// Order is load-bearing: the pending overlay claims the root first, so the
		// approval overlay only ever fills a chip no more specific human reason owns.
		observation = c.overlayCodexApprovalObservation(ref.Key(), observation, now)
		observation = c.overlayCodexChildObservation(ref.Key(), observation, now)
		// A request the overlays put on the root is the hooks' own (their
		// pending-input and approval latches, or a hook root carried over an
		// app-server gap), so the hook that closes it may resolve it.
		landing.HookLatched = kind != state.GraphHookEvent && own == agentgraph.AttentionNone &&
			codexObservationRootAttention(observation) != agentgraph.AttentionNone
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
	if observation.RootID == "" || len(observation.Nodes) == 0 {
		return false
	}
	if reason := priorSession.LandingRefusal(observation.RootID, observation.ObservedAt, kind); reason != "" {
		c.recordRejectionLocked(ref.Key(), observation, reason, now)
		return false
	}
	graph, err := state.ProjectAgentGraph(observation, priorSession.AgentGraph, now)
	if err != nil {
		c.recordDiagnosticLocked(ref.Provider, "invalid_observation", now)
		return false
	}
	beforeStatus, beforePending, beforeSession := "", "", observation.RootID
	beforeSince := time.Time{}
	if info := priorSession.Enrichment(); info != nil {
		beforeStatus, beforeSince = info.Status, info.StatusSince
		beforePending = info.PendingSummary()
		if info.SessionID != "" {
			beforeSession = info.SessionID
		}
	}
	applied, displayed, nativeOverride := false, false, false
	var refused statusexplain.Reason
	afterStatus, afterPending, afterSession := "", "", beforeSession
	nativeName, hasNativeName := observationRootName(observation)
	authoritativeNativeName := ref.Provider == agentgraph.ProviderCodex &&
		observation.Source == agentgraph.SourceCodexAppServer && observation.Complete && hasNativeName
	c.store.Apply(func(sessions map[int]*state.Session) {
		sess := sessions[ref.PID]
		if !sessionHoldsRoot(sess, ref.Key()) || agentgraph.ProviderKind(sess.Agent) != ref.Provider {
			return
		}
		// The published status is read under the lock on both sides: herdr can
		// move it between the snapshot above and here, and while herdr is the
		// authority the graph's own summary is not what the chip shows.
		if info := sess.Enrichment(); info != nil {
			beforeStatus, beforeSince = info.Status, info.StatusSince
		}
		if ref.Provider == agentgraph.ProviderClaude {
			applyClaudeCompatibility(sess.AgentBlock(state.AgentKindClaude), compat)
		}
		if refused = sess.LandAgentGraph(graph, landing, now); refused != "" {
			return
		}
		displayed = sess.AgentGraph != nil && sess.AgentGraph.Source == graph.Source && sess.AgentGraph.ObservedAt.Equal(graph.ObservedAt)
		if info := sess.Enrichment(); info != nil {
			afterStatus = info.Status
			afterPending = info.PendingSummary()
			if info.SessionID != "" {
				afterSession = info.SessionID
			}
		}
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
	if refused != "" {
		c.recordRejectionLocked(ref.Key(), observation, refused, now)
		return false
	}
	if !applied {
		c.history.Forget(observation.Provider, observation.RootID)
		return false
	}
	// History follows the published graph: evidence the resolver keeps but
	// does not show (a hook under a held request) projects nothing.
	var canonical []history.Event
	if displayed {
		canonical, err = c.history.Project(history.AgentStateContext{PID: ref.PID, CWD: ref.CWD}, observation, now)
		if err != nil {
			c.recordDiagnosticLocked(ref.Provider, "history_projection_error", now)
			canonical = nil
		}
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
	c.recordAdmissionLocked(ref.Key(), graph)
	for _, event := range canonical {
		c.sink.Record(event)
	}
	statusChanged := afterStatus != beforeStatus
	if statusChanged {
		c.sink.Record(history.Event{
			Ts: now, Type: history.EventTransition, SessionID: observation.RootID,
			PID: ref.PID, Agent: string(ref.Provider), CWD: ref.CWD,
			From: beforeStatus, To: afterStatus,
			Rule: transitionRule(rule), DurPrevMs: history.HeldMs(beforeSince, now),
			Subagents: graph.Summary.LiveChildren,
		})
	}
	// The provider graph used to write only history events. `diagnose` reads the
	// canonical statustune decision lines from the journal, so every Claude
	// transition — and every deliberate hold while red — was invisible there.
	// Log the same finite rule id at this landing point. Ordinary same-color
	// working/idle observations stay quiet; red holds remain visible because they
	// are precisely where "I answered it and it is still red" needs an explanation.
	if ref.Provider == agentgraph.ProviderClaude &&
		(statusChanged || (rule != "" && (beforeStatus == state.StatusPermission || afterStatus == state.StatusPermission))) {
		pending := afterPending
		if beforeStatus == state.StatusPermission && afterStatus != state.StatusPermission {
			pending = beforePending
		}
		age := time.Duration(0)
		if !beforeSince.IsZero() && now.After(beforeSince) {
			age = now.Sub(beforeSince)
		}
		from, to := beforeStatus, afterStatus
		if from == "" {
			from = "unknown"
		}
		if to == "" {
			to = "unknown"
		}
		statustune.Decision{
			PID: ref.PID, Session: shortSessionID(afterSession),
			From: from, To: to, Rule: transitionRule(rule),
			Reason:    fmt.Sprintf("provider graph source=%s", observation.Source),
			Subagents: graph.Summary.LiveChildren, Pending: pending, Age: age,
		}.Log()
	}
	return true
}

// transitionRule is what a transition records as its `rule`. An unattributed
// edge keeps the blanket id rather than an empty field, so `switchboard-ctl
// history` and diagnose can always tell "no rule reached the record" apart from
// "this line predates the rule field".
func transitionRule(rule string) string {
	if rule == "" {
		return statustune.RuleGraphAuthority
	}
	return rule
}

func observationRootName(observation agentgraph.Observation) (string, bool) {
	for _, node := range observation.Nodes {
		if node.ID == observation.RootID {
			return strings.TrimSpace(node.Nickname), true
		}
	}
	return "", false
}

// overlayCodexHookObservation keeps the app-server's structural detail and
// provenance while applying a newer hook's immediate root status. A hook is
// intentionally partial: it must not erase the authoritative thread name,
// child graph, completeness verdict, or freshness horizon. When app-server
// evidence is absent, stale, or explicitly lacks the root runtime, the hook
// remains the bounded fallback authority exactly as it was before composition.
func overlayCodexHookObservation(hook agentgraph.Observation, current *state.AgentGraph) agentgraph.Observation {
	if current == nil || current.RootID != hook.RootID || len(hook.Nodes) != 1 {
		return hook
	}
	overlay := observationFromState(agentgraph.ProviderCodex, current)
	if !codexAppServerGraphOwnsHookHorizon(current, hook.ObservedAt) {
		overlay.Source = agentgraph.SourceHook
		overlay.FreshUntil = hook.FreshUntil
		overlay.Complete = false
	}
	// Source is real provenance, not an internal routing bit. Mark every graph
	// rebuilt from current state so downstream child composition does not treat
	// its already-overlaid nodes as a fresh provider snapshot.
	overlay.Diagnostic = codexComposedObservationDiagnostic
	overlay.ObservedAt = hook.ObservedAt
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

// codexAppServerGraphOwnsHookHorizon reports whether a hook at hookAt composes
// onto app-server evidence that is still authoritative. The source check is
// load-bearing: codexAppServerRootUnavailable also returns false for graphs
// whose source is not the app-server.
func codexAppServerGraphOwnsHookHorizon(current *state.AgentGraph, hookAt time.Time) bool {
	return current != nil && current.Source == agentgraph.SourceCodexAppServer &&
		current.Fresh(hookAt) && !codexAppServerRootUnavailable(current)
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
	// The published graph lapsing is not new evidence: it re-lands as the kind
	// it already was, so the resolver weighs whatever else is still fresh.
	c.applyObservationAs(ref, generation, observation, compat, now, "", sess.DisplayGraphKind())
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
		if sessionHoldsRoot(&sess, key) {
			return sess, true
		}
	}
	return state.Session{}, false
}

// compatibilityFromState rebuilds the adapter's compatibility view from the
// persisted block — the input to a restore, and to the re-observation of a session
// whose graph went stale.
//
// Attention is NOT stamped here. It used to be hardcoded to AttentionApproval
// because the block had nowhere to carry it, which republished every question-red
// as an approval-red; now the per-call records carry the real one, and the legacy
// map — which still carries none — is left blank so restoredPending derives it
// from the tool rather than being handed a guess (askuserquestion-model-plan.md
// §2.4).
func compatibilityFromState(info *state.AgentInfo) claudeprovider.Compatibility {
	if info == nil {
		return claudeprovider.Compatibility{}
	}
	compat := claudeprovider.Compatibility{
		SessionID: info.SessionID, Transcript: info.Transcript, Status: info.Status,
		StatusSince: info.StatusSince, InFlightSubagents: info.InFlightSubagents,
		PendingWriters: append([]string(nil), info.PendingWriters...), PendingTool: info.PendingTool,
		Pending:     make(map[string]claudeprovider.PendingPrompt, len(info.Pending)),
		PendingSets: pendingSetsFromRecords(info.PendingPrompts),
		Workflows:   make([]fanout.Workflow, len(info.Workflows)),
	}
	for writer, prompt := range info.Pending {
		compat.Pending[writer] = claudeprovider.PendingPrompt{
			Tool: prompt.Tool, InputHash: prompt.InputHash, Since: prompt.Since,
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
	info.PendingPrompts = pendingRecordsFromSets(compat.PendingSets)
	info.PendingWriters = append([]string(nil), compat.PendingWriters...)
	info.PendingTool = compat.PendingTool
}

// pendingRecordsFromSets flattens the adapter's writer→open-set map into the
// persisted record list, writers in ascending order and each writer's prompts left
// in their own oldest-first order. The order is stamped here rather than left to
// map iteration because these records are encoded into the publish gate's change
// key; a set that reordered itself would republish to every bar on every tick.
//
// Only a confirmed call id is persisted. Proposed/ambiguous latch state remains
// in-memory: it carries no identity safe enough to use after a restart.
func pendingRecordsFromSets(sets map[string][]claudeprovider.PendingPrompt) []state.PendingPromptRecord {
	if len(sets) == 0 {
		return nil
	}
	writers := make([]string, 0, len(sets))
	for writer := range sets {
		writers = append(writers, writer)
	}
	sort.Strings(writers)
	records := make([]state.PendingPromptRecord, 0, len(sets))
	for _, writer := range writers {
		for _, prompt := range sets[writer] {
			callID := ""
			if prompt.Latch == claudeprovider.CallLatchBound {
				callID = prompt.CallID
			}
			records = append(records, state.PendingPromptRecord{
				Writer: writer, Tool: prompt.Tool, InputHash: prompt.InputHash,
				CallID: callID, Attention: string(prompt.Attention), Since: prompt.Since,
			})
		}
	}
	if len(records) == 0 {
		return nil
	}
	return records
}

// pendingSetsFromRecords is the inverse, regrouping the flat record list by
// writer in the adapter's own bare ("" = main) key spelling.
//
// The main thread is normalized on the way in because this reads a block from
// either side of the wire projection: a live AgentInfo carries the bare key, while
// every snapshot copy — which is what restoreClaude and expireCurrent actually
// hand it — carries "main". Left untranslated the two spellings become two
// writers, and the writer whose set was not found would be rebuilt from the legacy
// scalar as a second, unclearable red beside its own prompts.
func pendingSetsFromRecords(records []state.PendingPromptRecord) map[string][]claudeprovider.PendingPrompt {
	if len(records) == 0 {
		return nil
	}
	sets := make(map[string][]claudeprovider.PendingPrompt, len(records))
	for _, record := range records {
		writer := record.Writer
		if writer == state.PendingWriterMain {
			writer = ""
		}
		sets[writer] = append(sets[writer], claudeprovider.PendingPrompt{
			Tool: record.Tool, InputHash: record.InputHash,
			CallID: record.CallID, Attention: agentgraph.AttentionState(record.Attention), Since: record.Since,
		})
		if record.CallID != "" {
			last := len(sets[writer]) - 1
			sets[writer][last].Latch = claudeprovider.CallLatchBound
		}
	}
	return sets
}

// HandleHook is the RPC graph-aware hook callback. The incoming Claude
// AgentID is intentionally passed raw exactly once; the adapter performs its
// own canonicalization. Codex hook status is only a fallback beneath a fresh
// app-server observation. Pi has no adapter: its hooks go to the Pi reducer.
func (c *agentCoordinator) HandleHook(req rpc.Request, sess state.Session) {
	if req.Agent == state.AgentKindPi {
		if sess.Agent != state.AgentKindPi {
			c.recordDiagnostic(agentgraph.ProviderPi, "hook_provider_mismatch", c.now())
			return
		}
		c.handlePiHook(req, sess)
		return
	}
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
		now = c.now()
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
		c.restoreClaude(ref, now)
		generation := c.begin(ref.Key())
		result := c.claude.ApplyHook(claudeprovider.HookSignal{
			Root: ref, Event: req.Event, AgentID: req.AgentID, AgentType: req.AgentType,
			ToolName: req.ToolName, ToolInputHash: req.ToolInputHash, ToolUseID: req.ToolUseID, At: now,
		})
		if !result.Applied {
			return
		}
		if req.Event == "PermissionRequest" && result.Changed && result.PromptDepth == 2 {
			// P1 (askuserquestion-model-plan.md §5): parallel dispatch is measured
			// at 7.6% of tool-using turns, but nobody established how often that
			// becomes two concurrent permission prompts for one writer. The counter
			// must therefore read as EPISODES, not edges, so it is gated twice:
			// depth == 2 fires only on the 1->2 transition, so an 8-way dispatch
			// counts once rather than seven times; Changed excludes the dedupe path,
			// where a verbatim redelivery returns the writer's unchanged depth and
			// would otherwise re-count an episode that opened nothing.
			//
			// One upward bias survives and cannot be removed without call identity:
			// a hook registered twice (a user settings.json and a project one) fires
			// the same edge with two different wall-clock stamps, which is not a
			// verbatim redelivery and so opens a second prompt. Phase 4's id-matched
			// open is what makes those two edges one prompt.
			//
			// Content-free: a bounded category and a count, never a tool name or its
			// input.
			c.recordDiagnostic(ref.Provider, "prompt_parallel_episode", now)
		}
		comparison := claudeprovider.CompareShadow(result.Projection.Status, result.Observation, agentgraph.Summary{}, now)
		if !comparison.Match {
			c.recordDiagnostic(ref.Provider, comparison.Rule, now)
		}
		// result.Rule names the edge the adapter just decided by — the red opening,
		// one call's clear, a hold that left it red — and is the whole point of
		// Phase 3: a transition recorded without it says only that the graph spoke.
		c.applyObservationAs(ref, generation, result.Observation, result.Projection, now, result.Rule, state.GraphHookEvent)
	case agentgraph.ProviderCodex:
		rootID := req.SessionID
		if rootID == "" {
			rootID = ref.ProviderSessionID
		}
		if req.Transcript != "" {
			if binder, ok := c.codex.(codexRolloutBinder); ok {
				if err := binder.RegisterHookRollout(ref.Key(), rootID, req.Transcript); err != nil {
					c.recordDiagnostic(ref.Provider, "rollout_binding_error", now)
					return
				}
			}
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
		if !sessionHoldsRoot(sess, ref.Key()) || sess.Agent != state.AgentKindCodex {
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
		if !sessionHoldsRoot(sess, input.key) || sess.Agent != state.AgentKindCodex ||
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
			// A hook-only graph cannot carry a native name. This branch remains for
			// hydrated data written before composed hooks preserved app-server
			// provenance; new composed graphs take the complete app-server branch.
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
