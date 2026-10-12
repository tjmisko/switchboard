package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/provider"
	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/state"
	"github.com/tjmisko/switchboard/internal/statusexplain"
	"github.com/tjmisko/switchboard/internal/statustune"
	"github.com/tjmisko/switchboard/internal/tailcache"
)

// Pi hook evidence leases. Pi has no adapter that re-observes it between
// hooks, so its hook graph is edge-triggered evidence of the same kind as the
// Codex hook fallback, and takes that path's state-specific windows rather
// than a lease of its own. When one runs out with no newer hook, the status
// falls back to herdr, else to unknown (the status resolver, through
// state.Session.ReprojectPi).
const (
	piHookActiveLease    = codexHookActiveFreshness
	piHookAttentionLease = codexHookAttentionFreshness
	piHookIdleLease      = codexHookIdleFreshness
)

// piHookRoot is the reducer's state for one Pi process lifetime. A run is open
// from UserPromptSubmit (agent_start) until Stop or StopFailure
// (agent_settled); openDialogs is the extension's latest count of open
// dialogs. lastAt is the newest hook instant applied, which orders the
// spawn-and-forget hooks: one older than it is dropped. nameAt orders Pi's
// /name the same way, apart from status, because a rename carries none.
// usage and billing are the bound session's running totals (pi_usage.go).
type piHookRoot struct {
	sessionID   string
	runOpen     bool
	openDialogs int
	lastAt      time.Time
	nameAt      time.Time
	usage       agentgraph.Usage
	billing     agentgraph.BillingIdentity
	usageSeen   piUsageSeen
}

// seedPiHookRoot rebuilds a root's reducer state from the session's published
// state, for the first hook this daemon sees from a Pi process.
func seedPiHookRoot(sess state.Session) *piHookRoot {
	root := &piHookRoot{}
	if sess.Pi == nil || sess.Pi.SessionID == "" {
		return root
	}
	root.sessionID = sess.Pi.SessionID
	if g := sess.AgentGraph; g != nil && g.RootID == root.sessionID {
		root.runOpen = g.Summary.Runtime == agentgraph.RuntimeActive
		if g.Source == agentgraph.SourceHook {
			root.lastAt = g.ObservedAt
		}
		// A restart keeps the totals a Pi-owned graph carried; herdr's node
		// never holds any.
		seed := agentgraph.Observation{Nodes: []agentgraph.Node{{ID: root.sessionID}}, RootID: root.sessionID}
		carryPiRootDetail(&seed, g)
		root.usage, root.billing = seed.Nodes[0].Usage, seed.Nodes[0].Billing
	}
	return root
}

// reducePiHook applies one hook event to a root and returns the rule naming
// the edge, or "" when the event carries no status evidence (Usage,
// SessionEnd, anything unrecognized). SessionEnd holds the status until the
// following SessionStart; if the process dies first, death ends the session.
func reducePiHook(root *piHookRoot, req rpc.Request, rebinding bool) string {
	switch req.Event {
	case "SessionStart":
		root.runOpen, root.openDialogs = req.Busy, 0
		if !rebinding && req.HookSource == "reload" {
			return statustune.RulePiReloaded
		}
		return statustune.RulePiSessionStarted
	case "UserPromptSubmit":
		root.runOpen = true
		return statustune.RulePiRunStarted
	case "PreToolUse", "PostToolUse":
		root.runOpen = true
		return statustune.RulePiToolActivity
	case "PermissionRequest", "PermissionResolved":
		switch {
		case req.OpenDialogs != nil:
			root.openDialogs = max(*req.OpenDialogs, 0)
		case req.Event == "PermissionRequest":
			root.openDialogs = max(root.openDialogs, 1)
		default:
			root.openDialogs = max(root.openDialogs-1, 0)
		}
		if root.openDialogs > 0 {
			return statustune.RulePiDialogOpen
		}
		return statustune.RulePiDialogClosed
	case "Stop", "StopFailure":
		// A settled run waits in no dialog of its own; a dialog opened later
		// reports a fresh count.
		root.runOpen, root.openDialogs = false, 0
		if req.Event == "StopFailure" {
			return statustune.RulePiRunFailed
		}
		return statustune.RulePiRunSettled
	}
	return ""
}

// piRootObservation is a root's one-node hook graph at at: red while any
// dialog is open, else working while a run is open, else idle.
func piRootObservation(root *piHookRoot, at time.Time) agentgraph.Observation {
	runtime, attention, lease := agentgraph.RuntimeIdle, agentgraph.AttentionNone, piHookIdleLease
	if root.runOpen {
		runtime, lease = agentgraph.RuntimeActive, piHookActiveLease
	}
	if root.openDialogs > 0 {
		attention, lease = agentgraph.AttentionUserInput, piHookAttentionLease
	}
	return agentgraph.Observation{
		Provider: agentgraph.ProviderPi, RootID: root.sessionID, Source: agentgraph.SourceHook,
		ObservedAt: at, FreshUntil: at.Add(lease),
		Nodes: []agentgraph.Node{{
			ID: root.sessionID, Runtime: runtime, Attention: attention, UpdatedAt: at,
			Usage: root.usage, Billing: root.billing,
		}},
	}
}

// piSessionIDFromFile reads Pi's session id from a session file path,
// <dir>/<timestamp>_<session-id>.jsonl, or "" when the name is not that shape.
func piSessionIDFromFile(path string) string {
	name, ok := strings.CutSuffix(filepath.Base(path), ".jsonl")
	if !ok {
		return ""
	}
	i := strings.LastIndexByte(name, '_')
	id := name[i+1:]
	if i < 0 || id == "" || len(id) > 128 {
		return ""
	}
	for _, r := range id {
		if !(r == '-' || r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return ""
		}
	}
	return id
}

// handlePiHook is the Pi hook reducer: a daemon-side FSM over the extension's
// lifecycle events (integrations/pi/switchboard.ts), dispatched from HandleHook
// once rpc has applied the hook's usage-limit evidence. Its clock is the
// hook's own ObservedAt, the instant the Pi event fired.
//
// Every edge lands as a one-node hook graph rooted at Pi's session id, and
// every published status change becomes a history transition with a Pi rule
// code and a statustune decision line. The published status itself comes from
// the status resolver, which weighs this evidence against herdr's.
func (c *agentCoordinator) handlePiHook(req rpc.Request, sess state.Session) {
	key := providerRootKey(sess)
	if key.PID <= 0 || key.StartedAt.IsZero() {
		return
	}
	now := req.ObservedAt
	if now.IsZero() {
		now = c.now()
	}

	c.piMu.Lock()
	defer c.piMu.Unlock()
	root := c.piRoots[key]
	if root == nil {
		root = seedPiHookRoot(sess)
		c.piRoots[key] = root
	}
	switch req.Event {
	case "SessionEnd":
		return // no status evidence
	case "Usage":
		c.applyPiUsage(key, root, req, sess, now)
		return
	case "SessionName":
		c.applyPiSessionName(key, root, req, now)
		return
	}
	if now.Before(root.lastAt) {
		// Hooks are sent spawn-and-forget. An older one arriving late, a
		// stale dialog count above all, must not repaint newer evidence.
		c.recordDiagnostic(agentgraph.ProviderPi, "stale_observation_rejected", now)
		return
	}
	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = root.sessionID // herdr:blocked carries no session context
	}
	if sessionID == "" {
		c.recordDiagnostic(agentgraph.ProviderPi, "exact_binding_unavailable", now)
		return
	}
	prevID := root.sessionID
	rotated := prevID != "" && sessionID != prevID

	next := *root
	next.sessionID = sessionID
	if rotated {
		// Name and usage belong to the conversation left, not the new one.
		next.runOpen, next.openDialogs, next.nameAt = false, 0, time.Time{}
		next.usage, next.billing, next.usageSeen = agentgraph.Usage{}, agentgraph.BillingIdentity{}, piUsageSeen{}
	}
	// SessionStart carries Pi's current /name for the session it binds.
	naming := req.Event == "SessionStart" && !now.Before(next.nameAt)
	rule := reducePiHook(&next, req, rotated)
	if rule == "" {
		return
	}
	if rotated {
		rule = statustune.RulePiSessionRotated
	}
	observation := piRootObservation(&next, now)
	// The hook announces that the session file is moving: drop its cached
	// tail so any later read re-extracts it (#98).
	if req.Transcript != "" {
		tailcache.Default().Invalidate(req.Transcript)
	}

	applied := false
	before, after, cwd := "", "", ""
	var beforeSince time.Time
	c.store.Apply(func(sessions map[int]*state.Session) {
		s := sessions[key.PID]
		if !sessionHoldsRoot(s, key) || s.Agent != state.AgentKindPi {
			return
		}
		projected, err := state.ProjectAgentGraph(observation, s.AgentGraph, now)
		if err != nil {
			return
		}
		if s.Pi != nil {
			beforeSince = s.Pi.StatusSince
		}
		if rotated {
			s.RotatePiSession(sessionID, req.Transcript)
		}
		before, after = s.SetPiHookGraph(projected, now)
		if req.Transcript != "" {
			s.Pi.Transcript = req.Transcript
		}
		if naming {
			s.SetPiNativeName(sessionID, req.SessionName)
		}
		cwd, applied = s.CWD, true
	})
	if !applied {
		return
	}
	next.lastAt = now
	if naming {
		next.nameAt = now
	}
	*root = next

	if rotated {
		c.recordPiRotation(req, key.PID, cwd, prevID, sessionID, now)
	}
	canonical, err := c.history.Project(history.AgentStateContext{PID: key.PID, CWD: cwd}, observation, now)
	if err != nil {
		c.recordDiagnostic(agentgraph.ProviderPi, "history_projection_error", now)
	}
	for _, event := range canonical {
		c.sink.Record(event)
	}
	if before != after {
		c.sink.Record(history.Event{
			Ts: now, Type: history.EventTransition, SessionID: sessionID,
			PID: key.PID, Agent: state.AgentKindPi, CWD: cwd,
			From: before, To: after, Rule: rule, DurPrevMs: history.HeldMs(beforeSince, now),
		})
	}
	if before != after || before == state.StatusPermission || after == state.StatusPermission {
		logPiDecision(key.PID, sessionID, before, after, rule,
			fmt.Sprintf("pi hook event=%s run_open=%t open_dialogs=%d", req.Event, next.runOpen, next.openDialogs),
			beforeSince, now)
	}
}

// applyPiSessionName lands Pi's /name, set or cleared mid-session
// (session_info_changed), as the bound session's native display name. It is
// ordered by nameAt rather than lastAt: a rename is no status evidence, so it
// neither renews the hook lease nor fences a status hook. A rename for a
// session the root is not bound to is dropped; the SessionStart that binds
// that session carries its name.
func (c *agentCoordinator) applyPiSessionName(key provider.RootKey, root *piHookRoot, req rpc.Request, now time.Time) {
	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = root.sessionID
	}
	if sessionID == "" || sessionID != root.sessionID || now.Before(root.nameAt) {
		return
	}
	applied := false
	c.store.Apply(func(sessions map[int]*state.Session) {
		s := sessions[key.PID]
		if !sessionHoldsRoot(s, key) || s.Agent != state.AgentKindPi {
			return
		}
		s.SetPiNativeName(sessionID, req.SessionName)
		applied = true
	})
	if applied {
		root.nameAt = now
	}
}

// recordPiRotation opens the new session's history lane, paired with the
// session it replaced: by Pi's previous_session_file when that names the
// session left, else by the session the daemon had bound. A new session id on
// a live pid is what ends the old lane (docs/history-schema.md); no process
// died, so no session_end is written for it.
func (c *agentCoordinator) recordPiRotation(req rpc.Request, pid int, cwd, prevID, sessionID string, now time.Time) {
	c.history.Forget(agentgraph.ProviderPi, prevID)
	pairedID := piSessionIDFromFile(req.PreviousSessionFile)
	if pairedID == "" || pairedID == sessionID {
		pairedID = prevID
	}
	c.sink.Record(history.Event{Ts: now, Type: history.EventSessionStart,
		SessionID: sessionID, PrevSessionID: pairedID, PID: pid, Agent: state.AgentKindPi, CWD: cwd})
	c.recordDiagnostic(agentgraph.ProviderPi, "conversation_rotated", now)
}

// piDecisionMoved reports whether a re-projection changed which source decides
// a Pi status or why, even with the status itself unchanged: a hook lease that
// lapses onto a herdr reading of the same colour. Such a move is applied to the
// store so explain does not keep attributing the status to the lapsed hook; it
// publishes nothing, since no published field changes.
func piDecisionMoved(before, after statusexplain.Decision) bool {
	return before.Reason != after.Reason || before.Source != after.Source
}

// reconcilePiRoots runs on the coordinator's periodic pass. It reads the
// session file of each restored root no hook has reached yet, re-projects each
// bound Pi session at now, so a lease that ran out with nothing newer hands the
// status to herdr or to unknown, and it drops the reducer state of Pi
// processes no longer tracked.
func (c *agentCoordinator) reconcilePiRoots(now time.Time) {
	c.seedPiFromSessionFiles(now)
	type lapse struct {
		pid            int
		sessionID, cwd string
		before, after  string
		beforeSince    time.Time
	}
	snap := c.store.Snapshot()
	live := make(map[provider.RootKey]bool)
	pending := false
	for i := range snap.Sessions {
		sess := snap.Sessions[i]
		if sess.Agent != state.AgentKindPi {
			continue
		}
		live[providerRootKey(sess)] = true
		prior := sess.StatusDecision()
		if before, after := sess.ReprojectPi(now); before != after || piDecisionMoved(prior, sess.StatusDecision()) {
			pending = true
		}
	}

	c.piMu.Lock()
	defer c.piMu.Unlock()
	for key := range c.piTails {
		if !live[key] {
			delete(c.piTails, key)
		}
	}
	for key, root := range c.piRoots {
		if live[key] {
			continue
		}
		delete(c.piRoots, key)
		if root.sessionID != "" {
			c.history.Forget(agentgraph.ProviderPi, root.sessionID)
		}
	}
	if !pending {
		return
	}
	var lapses []lapse
	c.store.Apply(func(sessions map[int]*state.Session) {
		for _, s := range sessions {
			if s.Agent != state.AgentKindPi || s.Pi == nil {
				continue
			}
			beforeSince := s.Pi.StatusSince
			if before, after := s.ReprojectPi(now); before != after {
				lapses = append(lapses, lapse{pid: s.PID, sessionID: s.Pi.SessionID, cwd: s.CWD,
					before: before, after: after, beforeSince: beforeSince})
			}
		}
	})
	for _, l := range lapses {
		c.sink.Record(history.Event{
			Ts: now, Type: history.EventTransition, SessionID: l.sessionID,
			PID: l.pid, Agent: state.AgentKindPi, CWD: l.cwd,
			From: l.before, To: l.after, Rule: statustune.RulePiHookLapsed,
			DurPrevMs: history.HeldMs(l.beforeSince, now),
		})
		logPiDecision(l.pid, l.sessionID, l.before, l.after, statustune.RulePiHookLapsed,
			"pi hook lease ran out", l.beforeSince, now)
	}
}

// logPiDecision writes the statustune decision line `diagnose` reads.
func logPiDecision(pid int, sessionID, from, to, rule, reason string, since, now time.Time) {
	age := time.Duration(0)
	if !since.IsZero() && now.After(since) {
		age = now.Sub(since)
	}
	if from == "" {
		from = "unknown"
	}
	if to == "" {
		to = "unknown"
	}
	statustune.Decision{
		PID: pid, Session: shortSessionID(sessionID),
		From: from, To: to, Rule: rule, Reason: reason, Age: age,
	}.Log()
}
