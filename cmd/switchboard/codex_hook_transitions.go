package main

import (
	"strings"
	"sync"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/provider"
	claudeprovider "github.com/tjmisko/switchboard/internal/provider/claude"
	"github.com/tjmisko/switchboard/internal/rpc"
)

const codexHookStartSettle = 250 * time.Millisecond

type codexPendingInput struct {
	turnID, toolUseID, writer, toolName, inputHash string
}

type codexHookRootState struct {
	sessionID string
	latestAt  time.Time
	retired   map[string]struct{}
	pending   map[string]codexPendingInput
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
		rootState = &codexHookRootState{
			sessionID: currentID, latestAt: currentAt,
			retired: make(map[string]struct{}), pending: make(map[string]codexPendingInput),
		}
		c.codexHookRoots[ref.Key()] = rootState
	}
	if !codexHookSessionAllowed(rootState, rootID, req, now, introduced) {
		c.codexHookMu.Unlock()
		c.recordDiagnostic(ref.Provider, "stale_observation_rejected", now)
		return
	}
	var accepted bool
	ref, accepted = c.reconcileCodexBinding(ref, rootID, req.Generation, now)
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
	commitCodexHookSession(rootState, rootID, now)
	pendingAttention, hookOwnsTransition := reduceCodexPendingInput(rootState, req)
	c.codexHookMu.Unlock()

	ref.ProviderSessionID = rootID
	observation, mapped := codexHookObservation(rootID, req, ref.StartedAt, now)
	if mapped {
		observation = applyCodexPendingAttention(observation, pendingAttention, now)
		if current, ok := sessionForKey(c.store.Snapshot(), ref.Key()); ok {
			observation = overlayCodexHookObservation(observation, current.AgentGraph)
		}
		generation := c.begin(ref.Key())
		c.applyObservationWithHookOwnership(ref, generation, observation, claudeprovider.Compatibility{}, now, hookOwnsTransition)
	}
	if req.Event == "UserPromptSubmit" && req.Prompt != "" && ref.SlotID != "" {
		c.startAutoname(ref, rootID, req.Prompt, false)
	}
	if c.codex != nil {
		c.Request(ref.Key())
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
	}
	state.sessionID = rootID
	if now.After(state.latestAt) {
		state.latestAt = now
	}
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
	delete(c.codexHookRoots, key)
	c.codexHookMu.Unlock()
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
		runtime = agentgraph.RuntimeIdle
		attention = agentgraph.AttentionApproval
		if isCodexUserInputTool(req.ToolName) || req.ToolName == "AskUserQuestion" {
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
