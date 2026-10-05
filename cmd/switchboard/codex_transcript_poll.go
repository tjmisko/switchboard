package main

import (
	"log"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/provider"
	claudeprovider "github.com/tjmisko/switchboard/internal/provider/claude"
	codexprovider "github.com/tjmisko/switchboard/internal/provider/codex"
	"github.com/tjmisko/switchboard/internal/state"
)

const codexTranscriptQuietWindow = 90 * time.Second

// Poll only the rollout supplied by an accepted hook. Silence triggers a read;
// an explicit terminal marker, newer than the hook, is what establishes idle.
func (c *agentCoordinator) pollCodexStoppedRoot(ref provider.RootRef, now time.Time) {
	c.codexHookMu.Lock()
	root := c.codexHookRoots[ref.Key()]
	if root == nil || root.transcript == "" || now.Sub(root.latestAt) < codexTranscriptQuietWindow {
		c.codexHookMu.Unlock()
		return
	}
	path, sessionID, hookAt := root.transcript, root.sessionID, root.latestAt
	c.codexHookMu.Unlock()
	rollout, err := codexprovider.ReadRolloutState(path)
	if err != nil {
		return
	}
	// A usage-limit turn end is recorded by scanCodexUsageLimit, which also
	// reads the subagents' rollouts; this read only corrects the root to idle.
	runtime, at := rollout.Runtime, rollout.At

	c.codexHookMu.Lock()
	if c.codexHookRoots[ref.Key()] != root || root.sessionID != sessionID || !root.latestAt.Equal(hookAt) || root.transcript != path {
		c.codexHookMu.Unlock()
		return
	}
	// A newer start marker revokes an earlier polling correction. Pending input
	// and approval records remain hook-owned and cannot be cleared by this read.
	root.transcriptStoppedAt = time.Time{}
	if runtime != agentgraph.RuntimeIdle || at.Before(hookAt) || len(root.pending) != 0 || len(root.approvals) != 0 {
		c.codexHookMu.Unlock()
		return
	}
	sess, ok := sessionForKey(c.store.Snapshot(), ref.Key())
	if !ok || sess.AgentGraph == nil || sess.AgentGraph.RootID != sessionID {
		c.codexHookMu.Unlock()
		return
	}
	observation := observationFromState(agentgraph.ProviderCodex, sess.AgentGraph)
	for i := range observation.Nodes {
		node := &observation.Nodes[i]
		if node.ID == sessionID {
			if node.Attention != agentgraph.AttentionNone {
				c.codexHookMu.Unlock()
				return
			}
			if observation.Source == agentgraph.SourceCodexAppServer && observation.Fresh(now) && !codexRootStateUnavailable(node.Runtime, node.Attention) {
				c.codexHookMu.Unlock()
				return
			}
			node.Runtime, node.UpdatedAt = agentgraph.RuntimeIdle, at
		}
	}
	root.transcriptStoppedAt = at
	root.rootObservedAt, root.rootFreshUntil = now, now.Add(codexTranscriptQuietWindow)
	observation.ObservedAt, observation.FreshUntil = now, root.rootFreshUntil
	// Reserve while holding the hook reducer lock so an arriving hook fences
	// this observation before publication rather than being overwritten by it.
	generation := c.begin(ref.Key())
	c.codexHookMu.Unlock()
	c.applyObservationWithHookOwnership(ref, generation, observation, claudeprovider.Compatibility{}, now, true)
}

// recordCodexUsageLimit records a usage-limit turn end, the root's or a
// subagent's, on the session still bound to rootID. Session.RecordUsageLimit
// refuses it if a newer activity hook has already arrived.
func (c *agentCoordinator) recordCodexUsageLimit(ref provider.RootRef, rootID string, at time.Time, limit *codexprovider.RolloutUsageLimit) {
	sess, ok := sessionForKey(c.store.Snapshot(), ref.Key())
	if !ok || sess.AgentGraph == nil || sess.AgentGraph.RootID != rootID {
		return
	}
	// The scan re-reads the same evidence until the rollout moves; only new
	// evidence is worth a store write and a log line.
	if sess.UsageLimit != nil && sess.UsageLimit.ObservedAt.Equal(at) {
		return
	}
	c.store.Apply(func(sessions map[int]*state.Session) {
		live := sessions[sess.PID]
		if live == nil || live.AgentGraph == nil || live.AgentGraph.RootID != rootID {
			return
		}
		if live.RecordUsageLimit(state.UsageLimit{ObservedAt: at, ResetsAt: limit.ResetsAt, Source: state.UsageLimitSourceCodexRollout}) {
			log.Printf("usage-limit: pid=%d agent=codex resets_at=%s", sess.PID, formatUsageLimitReset(limit.ResetsAt))
		}
	})
}

func formatUsageLimitReset(resetsAt *time.Time) string {
	if resetsAt == nil {
		return "unknown"
	}
	return resetsAt.Format(time.RFC3339)
}
