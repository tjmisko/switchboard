package main

import (
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/provider"
	claudeprovider "github.com/tjmisko/switchboard/internal/provider/claude"
	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/state"
)

const (
	codexHookStartSettle     = 250 * time.Millisecond
	codexHookApprovalGrace   = 30 * time.Second
	codexHookLegacyGrace     = 500 * time.Millisecond
	codexChildHookQueueLimit = 256
	codexChildHookRetention  = codexHookActiveFreshness
)

type codexPendingInput struct {
	turnID, toolUseID, writer, toolName, inputHash string
}

type codexPendingApproval struct {
	turnID, toolUseID, writer, toolName, inputHash string
	episode                                        uint64
	startedAt                                      time.Time
	redPublishedAt                                 time.Time
	ref                                            provider.RootRef
	sessionID                                      string
	timer                                          *time.Timer
	done                                           sync.Once
}

func (p *codexPendingApproval) finish(wg *sync.WaitGroup) { p.done.Do(wg.Done) }

type codexHookRootState struct {
	sessionID string
	latestAt  time.Time
	retired   map[string]struct{}
	pending   map[string]codexPendingInput
	approvals map[string]*codexPendingApproval

	// Standalone app-server snapshots retain topology but report interactive
	// TUI roots as notLoaded. Preserve the last exact hook-owned root state under
	// its original bounded freshness deadline so those structural snapshots do
	// not erase useful status or extend hook evidence indefinitely.
	rootNode       agentgraph.Node
	rootObservedAt time.Time
	rootFreshUntil time.Time

	// Child lifecycle hooks are exact edges, not graph authority. They wait here
	// until a fresh app-server topology proves that agentID names a non-root node.
	// receiveSequence is a root-lifetime tie breaker for equal hook timestamps.
	childQueue       []codexChildHookEdge
	childOverlays    map[string]codexChildHookOverlay
	childLastApplied map[string]codexChildHookCursor
	childProvider    map[string]agentgraph.Node
	receiveSequence  uint64
}

type codexChildHookEdge struct {
	event              string
	agentID            string
	at                 time.Time
	receiveSequence    uint64
	unmatchedDiagnosed bool
}

type codexChildHookOverlay struct {
	event           string
	at              time.Time
	receiveSequence uint64
	runtime         agentgraph.RuntimeState
	lifecycle       agentgraph.LifecycleState
	runtimeOwned    bool
	lifecycleOwned  bool
	expiresAt       time.Time
}

type codexChildHookCursor struct {
	event           string
	at              time.Time
	receiveSequence uint64
}

type pendingCodexStart struct {
	sessionID string
	timer     *time.Timer
	ref       provider.RootRef
	req       rpc.Request
	now       time.Time
	done      sync.Once
}

func (p *pendingCodexStart) finish(wg *sync.WaitGroup) { p.done.Do(wg.Done) }

func newCodexHookRootState(sessionID string, latestAt time.Time) *codexHookRootState {
	return &codexHookRootState{
		sessionID:        sessionID,
		latestAt:         latestAt,
		retired:          make(map[string]struct{}),
		pending:          make(map[string]codexPendingInput),
		approvals:        make(map[string]*codexPendingApproval),
		childOverlays:    make(map[string]codexChildHookOverlay),
		childLastApplied: make(map[string]codexChildHookCursor),
		childProvider:    make(map[string]agentgraph.Node),
	}
}

func shouldSettleCodexSessionStart(req rpc.Request) bool {
	if req.Event != "SessionStart" {
		return false
	}
	switch req.HookSource {
	case "startup", "resume", "clear":
		return true
	default:
		return false
	}
}

func (c *agentCoordinator) deferCodexSessionStart(ref provider.RootRef, req rpc.Request, rootID string, now time.Time) {
	key := ref.Key()
	pending := &pendingCodexStart{sessionID: rootID, ref: ref, req: req, now: now}
	c.codexHookMu.Lock()
	if prior := c.codexStarts[key]; prior != nil {
		if prior.timer.Stop() {
			prior.finish(&c.codexTimerWG)
		}
	}
	delay := c.codexStartSettle
	if delay <= 0 {
		delay = codexHookStartSettle
	}
	c.codexTimerWG.Add(1)
	pending.timer = time.AfterFunc(delay, func() {
		defer pending.finish(&c.codexTimerWG)
		c.codexHookMu.Lock()
		if c.codexStarts[key] != pending {
			c.codexHookMu.Unlock()
			return
		}
		delete(c.codexStarts, key)
		c.codexHookMu.Unlock()
		c.handleCodexHookNow(pending.ref, pending.req, pending.sessionID, pending.now, false)
	})
	c.codexStarts[key] = pending
	c.codexHookMu.Unlock()
}

// consumeCodexSessionStart coalesces SessionStart(clear/startup/resume) with
// the first event for the same thread. An event for the retired thread cannot
// cancel the pending transition and rotate the stable PID back to stale state.
func (c *agentCoordinator) consumeCodexSessionStart(key provider.RootKey, rootID string) (accepted, introduced bool) {
	c.codexHookMu.Lock()
	defer c.codexHookMu.Unlock()
	pending := c.codexStarts[key]
	if pending == nil {
		return true, false
	}
	if pending.sessionID != rootID {
		return false, false
	}
	if pending.timer.Stop() {
		pending.finish(&c.codexTimerWG)
	}
	delete(c.codexStarts, key)
	return true, true
}

func (c *agentCoordinator) handleCodexHookNow(ref provider.RootRef, req rpc.Request, rootID string, now time.Time, introduced bool) {
	currentID, currentAt := ref.ProviderSessionID, time.Time{}
	if current, ok := sessionForKey(c.store.Snapshot(), ref.Key()); ok && current.AgentGraph != nil {
		if currentID == "" {
			currentID = current.AgentGraph.RootID
		}
		// Only hook time orders the hook reducer. A generic app-server snapshot
		// does not know whether standard Codex is inside request_user_input and
		// therefore cannot fence that exact onset edge merely by polling later.
		if current.AgentGraph.Source == agentgraph.SourceHook {
			currentAt = current.AgentGraph.ObservedAt
		}
	}

	c.codexHookMu.Lock()
	rootState := c.codexHookRoots[ref.Key()]
	if rootState == nil {
		rootState = newCodexHookRootState(currentID, currentAt)
		c.codexHookRoots[ref.Key()] = rootState
	}
	if !codexHookSessionAllowed(rootState, rootID, req, now, introduced) {
		c.codexHookMu.Unlock()
		c.recordDiagnostic(ref.Provider, "stale_observation_rejected", now)
		return
	}
	var accepted bool
	ref, accepted = c.reconcileCodexBinding(ref, rootID, now)
	if !accepted {
		c.codexHookMu.Unlock()
		c.recordDiagnostic(ref.Provider, "stale_observation_rejected", now)
		return
	}
	// SessionStart normally establishes exact identity, but a later trusted hook
	// must also self-heal a startup race. The registry accepts forward rotation
	// and fences identities it has retired for this process lifetime.
	if c.codex != nil {
		if reconciler, ok := c.codex.(codexBindingReconciler); ok {
			update, err := reconciler.ReconcileHookBinding(ref.Key(), rootID)
			if err != nil {
				c.codexHookMu.Unlock()
				c.recordDiagnostic(ref.Provider, "binding_error", now)
				return
			}
			if update.Stale {
				c.codexHookMu.Unlock()
				c.recordDiagnostic(ref.Provider, "stale_observation_rejected", now)
				return
			}
		} else if err := c.codex.RegisterHookBinding(ref.Key(), rootID); err != nil {
			c.codexHookMu.Unlock()
			c.recordDiagnostic(ref.Provider, "binding_error", now)
			return
		}
	}
	if rootState.sessionID != "" && rootState.sessionID != rootID {
		c.clearCodexApprovalsLocked(rootState)
	}
	commitCodexHookSession(rootState, rootID, now)
	pendingAttention, hookOwnsTransition := reduceCodexPendingInput(rootState, req)
	approvalDeferred, approvalOwnsTransition := c.reduceCodexPendingApprovalLocked(rootState, ref, rootID, req, now)
	hookOwnsTransition = hookOwnsTransition || approvalOwnsTransition
	c.codexHookMu.Unlock()
	ref.ProviderSessionID = rootID
	observation, mapped := codexHookObservation(rootID, req, ref.StartedAt, now)
	if mapped {
		observation = applyCodexPendingAttention(observation, pendingAttention, now)
		if current, ok := sessionForKey(c.store.Snapshot(), ref.Key()); ok {
			if approvalDeferred && pendingAttention == agentgraph.AttentionNone && current.AgentGraph != nil &&
				current.AgentGraph.Summary.Attention != agentgraph.AttentionNone {
				// An ambiguous permission hook may announce internal review, but it
				// cannot clear an independently proven human-owned request.
				mapped = false
			}
			hookOwnsTransition = hookOwnsTransition || codexAppServerRootUnavailable(current.AgentGraph)
			observation = overlayCodexHookObservation(observation, current.AgentGraph)
		}
	}
	if mapped {
		c.rememberCodexHookRootObservation(ref.Key(), observation)
		generation := c.begin(ref.Key())
		c.applyObservationWithHookOwnership(ref, generation, observation, claudeprovider.Compatibility{}, now, hookOwnsTransition)
	}
	if approvalDeferred {
		c.recordDiagnostic(agentgraph.ProviderCodex, "hook_approval_grace_started", now)
	}
	switch req.Event {
	case "UserPromptSubmit":
		c.retainCodexNamingCandidate(ref, rootID, req.TurnID, req.Prompt, now)
	case "Stop":
		c.completeCodexNaming(ref, rootID, req.TurnID, req.LastAssistantMessage, now)
	}
	if c.codex != nil {
		c.Request(ref.Key())
	}
}

func currentCodexGraph(snapshot state.Snapshot, key provider.RootKey) *state.AgentGraph {
	session, ok := sessionForKey(snapshot, key)
	if !ok || session.Agent != state.AgentKindCodex {
		return nil
	}
	return session.AgentGraph
}

const codexChildHookOverlayDiagnostic = "codex child hook overlay"

// enqueueCodexChildHook retains an exact child lifecycle edge until the
// coordinator can validate it against a fresh app-server graph. Child ordering
// is intentionally independent of root-hook ordering: a sibling edge is not
// stale merely because another hook for the root arrived later.
func (c *agentCoordinator) enqueueCodexChildHook(ref provider.RootRef, req rpc.Request, rootID string, now time.Time) {
	if req.AgentID == "" {
		c.recordDiagnostic(agentgraph.ProviderCodex, "subagent_hook_missing_id", now)
		return
	}
	currentID := ref.ProviderSessionID
	if graph := currentCodexGraph(c.store.Snapshot(), ref.Key()); graph != nil && currentID == "" {
		currentID = graph.RootID
	}

	c.codexHookMu.Lock()
	rootState := c.codexHookRoots[ref.Key()]
	if rootState == nil {
		rootState = newCodexHookRootState(currentID, time.Time{})
		c.codexHookRoots[ref.Key()] = rootState
	}
	wrongRoot := (currentID != "" && currentID != rootID) ||
		(rootState.sessionID != "" && rootState.sessionID != rootID)
	_, retired := rootState.retired[rootID]
	if wrongRoot || retired {
		c.codexHookMu.Unlock()
		c.recordDiagnostic(agentgraph.ProviderCodex, "stale_observation_rejected", now)
		return
	}
	if rootState.sessionID == "" {
		rootState.sessionID = rootID
	}
	if len(rootState.childQueue) >= codexChildHookQueueLimit {
		c.codexHookMu.Unlock()
		c.recordDiagnostic(agentgraph.ProviderCodex, "subagent_hook_queue_full", now)
		return
	}
	rootState.receiveSequence++
	receiveSequence := rootState.receiveSequence
	rootState.childQueue = append(rootState.childQueue, codexChildHookEdge{
		event: req.Event, agentID: req.AgentID, at: now, receiveSequence: receiveSequence,
	})
	c.codexHookMu.Unlock()

	if c.codex != nil {
		if err := c.codex.RegisterHookBinding(ref.Key(), rootID); err != nil {
			c.codexHookMu.Lock()
			if state := c.codexHookRoots[ref.Key()]; state != nil && state.sessionID == rootID {
				for i := range state.childQueue {
					if state.childQueue[i].receiveSequence == receiveSequence {
						state.childQueue = append(state.childQueue[:i], state.childQueue[i+1:]...)
						break
					}
				}
			}
			c.codexHookMu.Unlock()
			c.recordDiagnostic(agentgraph.ProviderCodex, "binding_conflict", now)
			return
		}
	}
	c.recordDiagnostic(agentgraph.ProviderCodex, "subagent_hook_queued", now)
	c.Request(ref.Key())
}

// reconcileCodexChildHooks runs only from the coordinator's serialized
// reconcile path. It never creates topology: an edge remains queued until its
// exact agent ID names a non-root node in a fresh codex_app_server graph.
func (c *agentCoordinator) reconcileCodexChildHooks(ref provider.RootRef, now time.Time) {
	c.expireCodexChildHookState(ref, now)
	for {
		graph := currentCodexGraph(c.store.Snapshot(), ref.Key())
		if graph == nil || graph.Source != agentgraph.SourceCodexAppServer || !graph.Fresh(now) {
			return
		}
		rootID := graph.RootID
		if rootID == "" {
			return
		}

		c.codexHookMu.Lock()
		rootState := c.codexHookRoots[ref.Key()]
		if rootState == nil || rootState.sessionID != rootID || len(rootState.childQueue) == 0 {
			c.codexHookMu.Unlock()
			return
		}
		sort.SliceStable(rootState.childQueue, func(i, j int) bool {
			left, right := rootState.childQueue[i], rootState.childQueue[j]
			if left.at.Equal(right.at) {
				return left.receiveSequence < right.receiveSequence
			}
			return left.at.Before(right.at)
		})
		matchedIndex := -1
		for i := range rootState.childQueue {
			if codexGraphHasChild(graph, rootState.childQueue[i].agentID) {
				matchedIndex = i
				break
			}
			if !rootState.childQueue[i].unmatchedDiagnosed {
				rootState.childQueue[i].unmatchedDiagnosed = true
				c.codexHookMu.Unlock()
				c.recordDiagnostic(agentgraph.ProviderCodex, "subagent_hook_unmatched_topology", now)
				c.codexHookMu.Lock()
				rootState = c.codexHookRoots[ref.Key()]
				if rootState == nil || rootState.sessionID != rootID {
					c.codexHookMu.Unlock()
					return
				}
			}
		}
		if matchedIndex < 0 {
			c.codexHookMu.Unlock()
			return
		}
		edge := rootState.childQueue[matchedIndex]
		rootState.childQueue = append(rootState.childQueue[:matchedIndex], rootState.childQueue[matchedIndex+1:]...)
		c.codexHookMu.Unlock()

		c.applyCodexChildHookEdge(ref, rootID, edge, now)
	}
}

func codexGraphHasChild(graph *state.AgentGraph, id string) bool {
	if graph == nil || id == "" || id == graph.RootID {
		return false
	}
	for _, node := range graph.Nodes {
		if node.ID == id && node.ParentID != "" {
			return true
		}
	}
	return false
}

func childHookTargets(event string) (agentgraph.RuntimeState, agentgraph.LifecycleState, bool) {
	switch event {
	case "SubagentStart":
		return agentgraph.RuntimeActive, agentgraph.LifecycleRunning, true
	case "SubagentStop":
		return agentgraph.RuntimeIdle, agentgraph.LifecycleCompleted, true
	default:
		return agentgraph.RuntimeUnknown, agentgraph.LifecycleUnknown, false
	}
}

func childHookCursorBefore(left codexChildHookEdge, right codexChildHookCursor) bool {
	return left.at.Before(right.at) || (left.at.Equal(right.at) && left.receiveSequence <= right.receiveSequence)
}

func (c *agentCoordinator) applyCodexChildHookEdge(ref provider.RootRef, rootID string, edge codexChildHookEdge, now time.Time) {
	runtime, lifecycle, ok := childHookTargets(edge.event)
	if !ok {
		return
	}
	graph := currentCodexGraph(c.store.Snapshot(), ref.Key())
	if graph == nil || graph.RootID != rootID || graph.Source != agentgraph.SourceCodexAppServer ||
		!graph.Fresh(now) || !codexGraphHasChild(graph, edge.agentID) {
		c.requeueCodexChildHook(ref.Key(), rootID, edge)
		return
	}

	var graphNode agentgraph.Node
	for _, node := range observationFromState(agentgraph.ProviderCodex, graph).Nodes {
		if node.ID == edge.agentID {
			graphNode = node
			break
		}
	}
	if graphNode.ID == "" {
		c.requeueCodexChildHook(ref.Key(), rootID, edge)
		return
	}

	c.codexHookMu.Lock()
	rootState := c.codexHookRoots[ref.Key()]
	if rootState == nil || rootState.sessionID != rootID {
		c.codexHookMu.Unlock()
		return
	}
	if last, exists := rootState.childLastApplied[edge.agentID]; exists {
		if childHookCursorBefore(edge, last) {
			c.codexHookMu.Unlock()
			return
		}
		if edge.event == last.event && graphNode.Runtime == runtime && graphNode.Lifecycle == lifecycle {
			// A replay emits no state/history edge, but its newer cursor still
			// fences an older hook that arrives afterward.
			rootState.childLastApplied[edge.agentID] = codexChildHookCursor{
				event: edge.event, at: edge.at, receiveSequence: edge.receiveSequence,
			}
			c.codexHookMu.Unlock()
			return
		}
	}
	providerNode, exists := rootState.childProvider[edge.agentID]
	if !exists {
		providerNode = graphNode
		rootState.childProvider[edge.agentID] = graphNode
	}
	runtimeOwned := childHookOwnsRuntime(providerNode, edge.at)
	lifecycleOwned := childHookOwnsLifecycle(providerNode, edge.at)
	previousOverlay, hadOverlay := rootState.childOverlays[edge.agentID]
	previousLast, hadLast := rootState.childLastApplied[edge.agentID]
	rootState.childLastApplied[edge.agentID] = codexChildHookCursor{
		event: edge.event, at: edge.at, receiveSequence: edge.receiveSequence,
	}
	if !runtimeOwned && !lifecycleOwned {
		c.codexHookMu.Unlock()
		if providerNode.Runtime == runtime && providerNode.Lifecycle == lifecycle {
			return
		}
		c.recordDiagnostic(agentgraph.ProviderCodex, "subagent_hook_provider_superseded", now)
		return
	}
	overlay := codexChildHookOverlay{
		event: edge.event, at: edge.at, receiveSequence: edge.receiveSequence,
		runtime: runtime, lifecycle: lifecycle,
		runtimeOwned: runtimeOwned, lifecycleOwned: lifecycleOwned,
	}
	if edge.event == "SubagentStart" {
		overlay.expiresAt = edge.at.Add(codexChildHookRetention)
	}
	rootState.childOverlays[edge.agentID] = overlay
	c.codexHookMu.Unlock()

	observation := observationFromState(agentgraph.ProviderCodex, graph)
	observation.Diagnostic = codexChildHookOverlayDiagnostic
	if !applyCodexChildOverlay(&observation, edge.agentID, overlay) {
		c.rollbackCodexChildHook(ref.Key(), rootID, edge, previousOverlay, hadOverlay, previousLast, hadLast)
		return
	}
	generation := c.begin(ref.Key())
	if !c.applyObservationWithHookOwnership(ref, generation, observation, claudeprovider.Compatibility{}, now, true) {
		c.rollbackCodexChildHook(ref.Key(), rootID, edge, previousOverlay, hadOverlay, previousLast, hadLast)
		return
	}
	c.recordDiagnostic(agentgraph.ProviderCodex, "subagent_hook_applied", edge.at)
}

func childHookOwnsRuntime(providerNode agentgraph.Node, hookAt time.Time) bool {
	return providerNode.Runtime == agentgraph.RuntimeUnknown || providerNode.Runtime == agentgraph.RuntimeNotLoaded ||
		providerNode.UpdatedAt.IsZero() || hookAt.After(providerNode.UpdatedAt)
}

func childHookOwnsLifecycle(providerNode agentgraph.Node, hookAt time.Time) bool {
	providerAt := providerNode.UpdatedAt
	if providerNode.Lifecycle.Terminal() && !providerNode.CompletedAt.IsZero() {
		providerAt = providerNode.CompletedAt
	}
	return providerNode.Lifecycle == agentgraph.LifecycleUnknown || providerAt.IsZero() || hookAt.After(providerAt)
}

func applyCodexChildOverlay(observation *agentgraph.Observation, agentID string, overlay codexChildHookOverlay) bool {
	for i := range observation.Nodes {
		node := &observation.Nodes[i]
		if node.ID != agentID || node.ParentID == "" {
			continue
		}
		if overlay.runtimeOwned {
			node.Runtime = overlay.runtime
		}
		if overlay.lifecycleOwned {
			node.Lifecycle = overlay.lifecycle
			if overlay.lifecycle == agentgraph.LifecycleCompleted {
				node.CompletedAt = overlay.at
			} else {
				node.CompletedAt = time.Time{}
			}
		}
		node.UpdatedAt = overlay.at
		return true
	}
	return false
}

func (c *agentCoordinator) requeueCodexChildHook(key provider.RootKey, rootID string, edge codexChildHookEdge) {
	c.codexHookMu.Lock()
	defer c.codexHookMu.Unlock()
	state := c.codexHookRoots[key]
	if state == nil || state.sessionID != rootID || len(state.childQueue) >= codexChildHookQueueLimit {
		return
	}
	state.childQueue = append(state.childQueue, edge)
}

func (c *agentCoordinator) rollbackCodexChildHook(key provider.RootKey, rootID string, edge codexChildHookEdge,
	previousOverlay codexChildHookOverlay, hadOverlay bool, previousLast codexChildHookCursor, hadLast bool) {
	c.codexHookMu.Lock()
	defer c.codexHookMu.Unlock()
	state := c.codexHookRoots[key]
	if state == nil || state.sessionID != rootID {
		return
	}
	if current, ok := state.childOverlays[edge.agentID]; ok && current.receiveSequence == edge.receiveSequence {
		if hadOverlay {
			state.childOverlays[edge.agentID] = previousOverlay
		} else {
			delete(state.childOverlays, edge.agentID)
		}
	}
	if current, ok := state.childLastApplied[edge.agentID]; ok && current.receiveSequence == edge.receiveSequence {
		if hadLast {
			state.childLastApplied[edge.agentID] = previousLast
		} else {
			delete(state.childLastApplied, edge.agentID)
		}
	}
	if len(state.childQueue) < codexChildHookQueueLimit {
		state.childQueue = append(state.childQueue, edge)
	}
}

// overlayCodexChildObservation fuses retained exact hook edges into a fresh
// structural snapshot. It never changes attention, parentage, or graph source.
// Concrete provider transitions that are at least as new retire the matching
// field; a complete omission retires the whole child overlay.
func (c *agentCoordinator) overlayCodexChildObservation(key provider.RootKey, observation agentgraph.Observation, now time.Time) agentgraph.Observation {
	if observation.Source != agentgraph.SourceCodexAppServer || observation.RootID == "" ||
		observation.Diagnostic == codexChildHookOverlayDiagnostic {
		return observation
	}
	c.codexHookMu.Lock()
	state := c.codexHookRoots[key]
	if state == nil || state.sessionID != observation.RootID {
		c.codexHookMu.Unlock()
		return observation
	}
	present := make(map[string]struct{}, len(observation.Nodes))
	superseded, expired := 0, 0
	for i := range observation.Nodes {
		node := &observation.Nodes[i]
		present[node.ID] = struct{}{}
		if node.ID == observation.RootID || node.ParentID == "" {
			continue
		}
		providerNode := *node
		state.childProvider[node.ID] = providerNode
		overlay, exists := state.childOverlays[node.ID]
		if !exists {
			continue
		}
		if overlay.event == "SubagentStart" && !overlay.expiresAt.IsZero() && !now.Before(overlay.expiresAt) {
			delete(state.childOverlays, node.ID)
			expired++
			continue
		}
		providerSuperseded := false
		if overlay.runtimeOwned && !childHookOwnsRuntime(providerNode, overlay.at) {
			overlay.runtimeOwned = false
			providerSuperseded = true
		}
		if overlay.lifecycleOwned && !childHookOwnsLifecycle(providerNode, overlay.at) {
			overlay.lifecycleOwned = false
			providerSuperseded = true
		}
		if providerSuperseded {
			superseded++
		}
		if !overlay.runtimeOwned && !overlay.lifecycleOwned {
			delete(state.childOverlays, node.ID)
			continue
		}
		state.childOverlays[node.ID] = overlay
		applyCodexChildOverlay(&observation, node.ID, overlay)
	}
	if observation.Complete {
		for id := range state.childOverlays {
			if _, exists := present[id]; !exists {
				delete(state.childOverlays, id)
				superseded++
			}
		}
		for id := range state.childProvider {
			if _, exists := present[id]; !exists {
				delete(state.childProvider, id)
			}
		}
	}
	c.codexHookMu.Unlock()
	for range superseded {
		c.recordDiagnostic(agentgraph.ProviderCodex, "subagent_hook_provider_superseded", now)
	}
	for range expired {
		c.recordDiagnostic(agentgraph.ProviderCodex, "subagent_hook_expired", now)
	}
	return observation
}

func (c *agentCoordinator) expireCodexChildHookState(ref provider.RootRef, now time.Time) {
	type expiredOverlay struct {
		id   string
		base agentgraph.Node
	}
	var expired []expiredOverlay
	expiredEdges := 0
	c.codexHookMu.Lock()
	state := c.codexHookRoots[ref.Key()]
	if state == nil {
		c.codexHookMu.Unlock()
		return
	}
	retained := state.childQueue[:0]
	for _, edge := range state.childQueue {
		if !now.Before(edge.at.Add(codexChildHookRetention)) {
			expiredEdges++
			continue
		}
		retained = append(retained, edge)
	}
	state.childQueue = retained
	for id, overlay := range state.childOverlays {
		if overlay.event != "SubagentStart" || overlay.expiresAt.IsZero() || now.Before(overlay.expiresAt) {
			continue
		}
		delete(state.childOverlays, id)
		if base, ok := state.childProvider[id]; ok {
			expired = append(expired, expiredOverlay{id: id, base: base})
		}
	}
	rootID := state.sessionID
	c.codexHookMu.Unlock()

	for range expiredEdges + len(expired) {
		c.recordDiagnostic(agentgraph.ProviderCodex, "subagent_hook_expired", now)
	}
	if len(expired) == 0 {
		return
	}
	graph := currentCodexGraph(c.store.Snapshot(), ref.Key())
	if graph == nil || graph.RootID != rootID || graph.Source != agentgraph.SourceCodexAppServer || !graph.Fresh(now) {
		return
	}
	observation := observationFromState(agentgraph.ProviderCodex, graph)
	observation.Diagnostic = codexChildHookOverlayDiagnostic
	changed := false
	for _, item := range expired {
		for i := range observation.Nodes {
			if observation.Nodes[i].ID == item.id && observation.Nodes[i].ParentID != "" {
				observation.Nodes[i] = item.base
				changed = true
				break
			}
		}
	}
	if changed {
		c.applyObservationWithHookOwnership(ref, c.begin(ref.Key()), observation, claudeprovider.Compatibility{}, now, true)
	}
}

func codexHookSessionAllowed(state *codexHookRootState, rootID string, req rpc.Request, now time.Time, introduced bool) bool {
	if rootID == "" || (!state.latestAt.IsZero() && now.Before(state.latestAt)) {
		return false
	}
	if state.sessionID == "" || state.sessionID == rootID {
		return true
	}
	if _, stale := state.retired[rootID]; stale {
		return false
	}
	// SessionStart is canonical. UserPromptSubmit is the first substantive hook
	// after a coalesced or missed start; generic tool/stop hooks may not rotate.
	return introduced || req.Event == "SessionStart" || req.Event == "UserPromptSubmit"
}

func commitCodexHookSession(state *codexHookRootState, rootID string, now time.Time) {
	if state.sessionID != "" && state.sessionID != rootID {
		state.retired[state.sessionID] = struct{}{}
		clear(state.pending)
		state.rootNode = agentgraph.Node{}
		state.rootObservedAt = time.Time{}
		state.rootFreshUntil = time.Time{}
		state.childQueue = nil
		clear(state.childOverlays)
		clear(state.childLastApplied)
		clear(state.childProvider)
		state.receiveSequence = 0
	}
	state.sessionID = rootID
	if now.After(state.latestAt) {
		state.latestAt = now
	}
}

func (c *agentCoordinator) rememberCodexHookRootObservation(key provider.RootKey, observation agentgraph.Observation) {
	var root agentgraph.Node
	for _, node := range observation.Nodes {
		if node.ID == observation.RootID {
			root = node
			break
		}
	}
	if root.ID == "" {
		return
	}
	c.codexHookMu.Lock()
	defer c.codexHookMu.Unlock()
	state := c.codexHookRoots[key]
	if state == nil || state.sessionID != observation.RootID ||
		(!state.rootObservedAt.IsZero() && observation.ObservedAt.Before(state.rootObservedAt)) {
		return
	}
	state.rootNode = root
	state.rootObservedAt = observation.ObservedAt
	state.rootFreshUntil = observation.FreshUntil
}

// overlayCodexHookRootObservation composes exact hook-owned root status with a
// structurally complete standalone app-server graph. It applies only when the
// app-server explicitly lacks a live runtime and never survives the hook's
// original freshness deadline.
func (c *agentCoordinator) overlayCodexHookRootObservation(key provider.RootKey, observation agentgraph.Observation, now time.Time) agentgraph.Observation {
	if observation.Source != agentgraph.SourceCodexAppServer || observation.RootID == "" {
		return observation
	}
	c.codexHookMu.Lock()
	state := c.codexHookRoots[key]
	if state == nil || state.sessionID != observation.RootID || state.rootNode.ID != observation.RootID ||
		state.rootFreshUntil.IsZero() || !now.Before(state.rootFreshUntil) {
		c.codexHookMu.Unlock()
		return observation
	}
	root, freshUntil := state.rootNode, state.rootFreshUntil
	c.codexHookMu.Unlock()

	for i := range observation.Nodes {
		if observation.Nodes[i].ID != observation.RootID {
			continue
		}
		if !codexRootStateUnavailable(observation.Nodes[i].Runtime, observation.Nodes[i].Attention) {
			return observation
		}
		observation.Nodes[i].Runtime = root.Runtime
		observation.Nodes[i].Attention = root.Attention
		observation.Nodes[i].Lifecycle = root.Lifecycle
		observation.Nodes[i].UpdatedAt = root.UpdatedAt
		if observation.FreshUntil.IsZero() || freshUntil.Before(observation.FreshUntil) {
			observation.FreshUntil = freshUntil
		}
		return observation
	}
	return observation
}

func codexAppServerRootUnavailable(graph *state.AgentGraph) bool {
	if graph == nil || graph.Source != agentgraph.SourceCodexAppServer {
		return false
	}
	for _, node := range graph.Nodes {
		if node.ID == graph.RootID {
			return codexRootStateUnavailable(node.Runtime, node.Attention)
		}
	}
	return false
}

func codexRootStateUnavailable(runtime agentgraph.RuntimeState, attention agentgraph.AttentionState) bool {
	return attention == agentgraph.AttentionNone &&
		(runtime == agentgraph.RuntimeUnknown || runtime == agentgraph.RuntimeNotLoaded)
}

func reduceCodexPendingInput(state *codexHookRootState, req rpc.Request) (agentgraph.AttentionState, bool) {
	ownedTransition := false
	if req.Event == "PreToolUse" && isCodexUserInputTool(req.ToolName) {
		pending := codexPendingInput{
			turnID: req.TurnID, toolUseID: req.ToolUseID, writer: req.AgentID,
			toolName: req.ToolName, inputHash: req.ToolInputHash,
		}
		state.pending[codexPendingInputKey(pending)] = pending
		ownedTransition = true
	}
	if req.Event == "PostToolUse" && isCodexUserInputTool(req.ToolName) {
		for key, pending := range state.pending {
			if codexPendingInputMatches(pending, req) {
				delete(state.pending, key)
				ownedTransition = true
			}
		}
	}
	if req.Event == "Stop" {
		for key, pending := range state.pending {
			if req.TurnID == "" || pending.turnID == "" || (pending.turnID == req.TurnID && pending.writer == req.AgentID) {
				delete(state.pending, key)
				ownedTransition = true
			}
		}
	}
	if len(state.pending) > 0 {
		return agentgraph.AttentionUserInput, ownedTransition
	}
	return agentgraph.AttentionNone, ownedTransition
}

// reduceCodexPendingApprovalLocked gives a generic PermissionRequest the same
// bounded ownership grace as the app-server observer. The hook says that Codex
// reached a permission boundary, but it does not say whether a person or the
// Auto-review agent owns the decision. Progress from the exact same tool call
// cancels the grace; only an unresolved boundary reaches red at the deadline.
// c.codexHookMu must be held.
func (c *agentCoordinator) reduceCodexPendingApprovalLocked(
	state *codexHookRootState,
	ref provider.RootRef,
	rootID string,
	req rpc.Request,
	now time.Time,
) (deferred, ownsTransition bool) {
	if state.approvals == nil {
		state.approvals = make(map[string]*codexPendingApproval)
	}
	if req.Event == "PermissionRequest" && !isCodexHumanInputPermission(req.ToolName) {
		c.codexWaitEpisode++
		pending := &codexPendingApproval{
			turnID: req.TurnID, toolUseID: req.ToolUseID, writer: req.AgentID,
			toolName: req.ToolName, inputHash: req.ToolInputHash,
			episode: c.codexWaitEpisode, startedAt: now, ref: ref, sessionID: rootID,
		}
		key := codexPendingApprovalKey(pending)
		if prior := state.approvals[key]; prior != nil {
			c.stopCodexApprovalLocked(prior)
		}
		state.approvals[key] = pending
		c.startCodexApprovalTimerLocked(state, key, pending)
		logCodexHookWait("started", pending, "hook_permission", now)
		return true, true
	}

	resolved := make([]string, 0, 1)
	switch req.Event {
	case "PreToolUse", "PostToolUse":
		for key, pending := range state.approvals {
			if codexPendingApprovalMatches(pending, req) {
				resolved = append(resolved, key)
			}
		}
	case "Stop":
		for key, pending := range state.approvals {
			if req.TurnID == "" || pending.turnID == "" ||
				(pending.turnID == req.TurnID && pending.writer == req.AgentID) {
				resolved = append(resolved, key)
			}
		}
	case "SessionStart", "UserPromptSubmit":
		for key := range state.approvals {
			resolved = append(resolved, key)
		}
	}

	for _, key := range resolved {
		pending := state.approvals[key]
		if pending == nil {
			continue
		}
		published := !pending.redPublishedAt.IsZero()
		c.stopCodexApprovalLocked(pending)
		delete(state.approvals, key)
		logCodexHookWait("resolved", pending, "hook_progress", now)
		ownsTransition = ownsTransition || published
	}
	return false, ownsTransition
}

// startCodexApprovalTimerLocked schedules the Phase-1 timeout-to-human fallback.
// c.codexHookMu must be held.
func (c *agentCoordinator) startCodexApprovalTimerLocked(state *codexHookRootState, key string, pending *codexPendingApproval) {
	delay := c.codexApprovalGrace
	if delay <= 0 {
		delay = codexHookApprovalGrace
	}
	c.codexTimerWG.Add(1)
	pending.timer = time.AfterFunc(delay, func() {
		defer pending.finish(&c.codexTimerWG)
		appServerSettled := false
		existingAttention := false
		if currentSession, ok := sessionForKey(c.store.Snapshot(), pending.ref.Key()); ok && currentSession.AgentGraph != nil {
			graph := currentSession.AgentGraph
			existingAttention = graph.Summary.Attention != agentgraph.AttentionNone
			appServerSettled = graph.Source == agentgraph.SourceCodexAppServer &&
				!graph.ObservedAt.Before(pending.startedAt) && graph.Summary.Runtime == agentgraph.RuntimeActive &&
				graph.Summary.Attention == agentgraph.AttentionNone
		}
		c.codexHookMu.Lock()
		current := c.codexHookRoots[pending.ref.Key()]
		if current != state || current.sessionID != pending.sessionID || current.approvals[key] != pending {
			c.codexHookMu.Unlock()
			return
		}
		now := time.Now()
		if existingAttention {
			delete(current.approvals, key)
			c.codexHookMu.Unlock()
			logCodexHookWait("resolved", pending, "existing_human_attention", now)
			c.recordDiagnostic(agentgraph.ProviderCodex, "hook_approval_suppressed_existing_attention", now)
			return
		}
		if appServerSettled {
			delete(current.approvals, key)
			c.codexHookMu.Unlock()
			logCodexHookWait("resolved", pending, "app_server_non_attention", now)
			c.recordDiagnostic(agentgraph.ProviderCodex, "hook_approval_resolved_by_app_server", now)
			return
		}
		pending.redPublishedAt = now
		// Reserve the generation while the pending entry is still protected by
		// codexHookMu. A concurrent resolving hook will reserve a later generation
		// and fence this timer out if it wins the race to publication.
		generation := c.begin(pending.ref.Key())
		c.codexHookMu.Unlock()

		logCodexHookWait("red_published", pending, "timeout_fallback", now)
		c.recordDiagnostic(agentgraph.ProviderCodex, "hook_approval_timeout_red", now)
		observation := codexHookApprovalObservation(pending.sessionID, pending.ref.StartedAt, now)
		if currentSession, ok := sessionForKey(c.store.Snapshot(), pending.ref.Key()); ok {
			observation = overlayCodexHookObservation(observation, currentSession.AgentGraph)
		}
		c.applyObservationWithHookOwnership(
			pending.ref, generation, observation, claudeprovider.Compatibility{}, now, true,
		)
	})
}

// clearCodexApprovalsLocked is used for conversation rotation, root removal,
// and shutdown. c.codexHookMu must be held.
func (c *agentCoordinator) clearCodexApprovalsLocked(state *codexHookRootState) {
	if state == nil {
		return
	}
	for key, pending := range state.approvals {
		c.stopCodexApprovalLocked(pending)
		delete(state.approvals, key)
	}
}

// stopCodexApprovalLocked balances codexTimerWG whether the timer has not fired
// or its callback is already responsible for completion.
func (c *agentCoordinator) stopCodexApprovalLocked(pending *codexPendingApproval) {
	if pending != nil && pending.timer != nil && pending.timer.Stop() {
		pending.finish(&c.codexTimerWG)
	}
}

func codexPendingApprovalKey(pending *codexPendingApproval) string {
	if pending.toolUseID != "" {
		return "id:" + pending.toolUseID
	}
	if pending.inputHash != "" {
		return "hash:" + pending.writer + "\x00" + pending.turnID + "\x00" + pending.toolName + "\x00" + pending.inputHash
	}
	return "episode:" + strconv.FormatUint(pending.episode, 10)
}

func codexPendingApprovalMatches(pending *codexPendingApproval, req rpc.Request) bool {
	if pending.toolUseID != "" || req.ToolUseID != "" {
		return pending.toolUseID != "" && pending.toolUseID == req.ToolUseID
	}
	return pending.inputHash != "" && pending.inputHash == req.ToolInputHash &&
		pending.writer == req.AgentID && pending.turnID == req.TurnID && pending.toolName == req.ToolName
}

func isCodexHumanInputPermission(tool string) bool {
	return isCodexUserInputTool(tool) || strings.EqualFold(tool, "AskUserQuestion")
}

func logCodexHookWait(event string, pending *codexPendingApproval, evidence string, now time.Time) {
	redPublished := !pending.redPublishedAt.IsZero()
	redDuration := elapsedHookWait(pending.redPublishedAt, now)
	duration := elapsedHookWait(pending.startedAt, now)
	log.Printf("agent-observer: provider=codex category=wait_episode event=%s episode=%d request_kind=hook_permission ownership=%s evidence=%s source=hook duration_ms=%d red_duration_ms=%d red_published=%t human_evidence=false cleared_without_human_evidence=%t suppressed_false_red=false old_would_publish_red=%t count=1",
		event, pending.episode, hookWaitOwnership(redPublished), evidence,
		duration.Milliseconds(), redDuration.Milliseconds(), redPublished,
		event == "resolved" && redPublished, duration >= codexHookLegacyGrace)
}

func hookWaitOwnership(redPublished bool) string {
	if redPublished {
		return "human"
	}
	return "unknown"
}

func elapsedHookWait(started, now time.Time) time.Duration {
	if started.IsZero() || now.Before(started) {
		return 0
	}
	return now.Sub(started)
}

func codexPendingInputKey(pending codexPendingInput) string {
	if pending.toolUseID != "" {
		return "id:" + pending.toolUseID
	}
	return "fallback:" + pending.writer + "\x00" + pending.turnID + "\x00" + pending.toolName + "\x00" + pending.inputHash
}

func codexPendingInputMatches(pending codexPendingInput, req rpc.Request) bool {
	if pending.toolUseID != "" || req.ToolUseID != "" {
		return pending.toolUseID != "" && pending.toolUseID == req.ToolUseID
	}
	return pending.inputHash != "" && pending.inputHash == req.ToolInputHash &&
		pending.writer == req.AgentID && pending.turnID == req.TurnID && pending.toolName == req.ToolName
}

func isCodexUserInputTool(tool string) bool {
	if dot := strings.LastIndex(tool, "."); dot >= 0 {
		tool = tool[dot+1:]
	}
	normalized := strings.ReplaceAll(strings.ToLower(tool), "_", "")
	return normalized == "requestuserinput"
}

func applyCodexPendingAttention(observation agentgraph.Observation, attention agentgraph.AttentionState, now time.Time) agentgraph.Observation {
	if attention == agentgraph.AttentionNone {
		return observation
	}
	for i := range observation.Nodes {
		if observation.Nodes[i].ID != observation.RootID {
			continue
		}
		observation.Nodes[i].Runtime = agentgraph.RuntimeIdle
		observation.Nodes[i].Attention = attention
		observation.Nodes[i].UpdatedAt = now
		observation.FreshUntil = now.Add(codexHookAttentionFreshness)
		break
	}
	return observation
}

// overlayCodexPendingObservation keeps an observed standard-CLI question red
// across generic app-server snapshots. The app-server item latch is not
// reachable on that launch path, so only the exact PostToolUse/Stop hook may
// release this independently owned evidence.
func (c *agentCoordinator) overlayCodexPendingObservation(key provider.RootKey, observation agentgraph.Observation, now time.Time) agentgraph.Observation {
	c.codexHookMu.Lock()
	defer c.codexHookMu.Unlock()
	state := c.codexHookRoots[key]
	if state == nil || state.sessionID != observation.RootID || len(state.pending) == 0 {
		return observation
	}
	return applyCodexPendingAttention(observation, agentgraph.AttentionUserInput, now)
}

func (c *agentCoordinator) forgetCodexHookState(key provider.RootKey) {
	c.codexHookMu.Lock()
	if pending := c.codexStarts[key]; pending != nil {
		if pending.timer.Stop() {
			pending.finish(&c.codexTimerWG)
		}
		delete(c.codexStarts, key)
	}
	c.clearCodexApprovalsLocked(c.codexHookRoots[key])
	delete(c.codexHookRoots, key)
	c.codexHookMu.Unlock()
	c.cancelCodexNaming(key, true)
}

func codexHookObservation(rootID string, req rpc.Request, startedAt, now time.Time) (agentgraph.Observation, bool) {
	if rootID == "" {
		return agentgraph.Observation{}, false
	}
	runtime, attention := agentgraph.RuntimeUnknown, agentgraph.AttentionNone
	switch req.Event {
	case "SessionStart":
		if req.HookSource == "compact" {
			runtime = agentgraph.RuntimeActive
		} else {
			runtime = agentgraph.RuntimeIdle
		}
	case "Stop":
		runtime = agentgraph.RuntimeIdle
	case "UserPromptSubmit", "PreToolUse", "PostToolUse":
		runtime = agentgraph.RuntimeActive
		if req.Event == "PreToolUse" && isCodexUserInputTool(req.ToolName) {
			runtime = agentgraph.RuntimeIdle
			attention = agentgraph.AttentionUserInput
		}
	case "PermissionRequest":
		// A generic permission hook does not identify whether the decision is
		// owned by the user or Auto-review. Keep it active during the grace; the
		// coordinator publishes approval only if the exact gate remains unresolved
		// through codexHookApprovalGrace. Structured questions are human-owned.
		runtime = agentgraph.RuntimeActive
		if isCodexHumanInputPermission(req.ToolName) {
			runtime = agentgraph.RuntimeIdle
			attention = agentgraph.AttentionUserInput
		}
	default:
		return agentgraph.Observation{}, false
	}
	freshness := codexHookIdleFreshness
	if attention != agentgraph.AttentionNone {
		freshness = codexHookAttentionFreshness
	} else if runtime == agentgraph.RuntimeActive {
		freshness = codexHookActiveFreshness
	}
	return agentgraph.Observation{
		Provider: agentgraph.ProviderCodex, RootID: rootID, Source: agentgraph.SourceHook,
		ObservedAt: now, FreshUntil: now.Add(freshness), Complete: false,
		Nodes: []agentgraph.Node{{
			ID: rootID, Runtime: runtime, Attention: attention,
			Lifecycle: agentgraph.LifecycleRunning, StartedAt: startedAt, UpdatedAt: now,
		}},
	}, true
}

func codexHookApprovalObservation(rootID string, startedAt, now time.Time) agentgraph.Observation {
	return agentgraph.Observation{
		Provider: agentgraph.ProviderCodex, RootID: rootID, Source: agentgraph.SourceHook,
		ObservedAt: now, FreshUntil: now.Add(codexHookAttentionFreshness), Complete: false,
		Nodes: []agentgraph.Node{{
			ID: rootID, Runtime: agentgraph.RuntimeIdle, Attention: agentgraph.AttentionApproval,
			Lifecycle: agentgraph.LifecycleRunning, StartedAt: startedAt, UpdatedAt: now,
		}},
	}
}
