package codex

import (
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/provider"
)

func (o *Observer) runRolloutCollector() {
	defer o.wg.Done()
	ticker := time.NewTicker(o.config.RolloutPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-o.ctx.Done():
			return
		case <-ticker.C:
		case <-o.rolloutRefresh:
		}
		o.rollout.collectAll(o.ctx)
	}
}

// enrichRolloutIdentity snapshots account/auth classification at the point a
// turn's durable record is written. AuthMode never implies a ChatGPT billing
// route; only direct API/cloud credentials or provider-native usage evidence do.
func (o *Observer) enrichRolloutIdentity(identity agentgraph.BillingIdentity) agentgraph.BillingIdentity {
	o.mu.Lock()
	account := o.account
	o.mu.Unlock()
	identity.AgentClient = string(agentgraph.ProviderCodex)
	if identity.AuthMode == "" {
		identity.AuthMode = account.authMode
	}
	if identity.AccountKind == "" {
		identity.AccountKind = account.accountKind
	}
	if identity.BillingRoute == "" {
		identity.BillingRoute = account.billingRoute
	}
	if identity.ExecutionProvider == "" {
		identity.ExecutionProvider = account.executionProvider
	}
	return identity
}

func rolloutLatestKey(rootID, threadID string) string { return rootID + "\x00" + threadID }

// installRolloutUsage runs only after canonical history and its cursor are
// durable. It updates the current graph and optional notification stream; those
// downstream views can be dropped/rebuilt without affecting accounting.
func (o *Observer) installRolloutUsage(update UsageUpdate) {
	o.mu.Lock()
	o.rolloutLatest[rolloutLatestKey(update.RootSessionID, update.ThreadID)] = cloneUsageUpdate(update)
	record := o.roots[update.RootKey]
	changed := false
	if record != nil && record.threadID == update.RootSessionID && record.graph != nil {
		if node := record.graph.nodes[update.ThreadID]; node != nil {
			changed = node.node.Usage != update.Total || node.node.Billing != update.Identity
			node.node.Usage = update.Total
			node.node.Billing = mergeBilling(node.node.Billing, update.Identity)
			if changed {
				if observation, err := record.graph.observation(o.config.Now(), o.config.Freshness); err == nil {
					record.observation = observation
					o.scheduleExpiryLocked(update.RootKey, record)
				}
			}
		}
	}
	// This bounded stream is explicitly a convenience notification. It is not a
	// persistence boundary and may drop while canonical rollout ingestion remains
	// non-lossy.
	o.emitUsageUpdateLocked(update)
	o.mu.Unlock()
	if changed {
		o.queue.Signal(update.RootKey)
	}
}

func (o *Observer) mergeRolloutLatestLocked(key provider.RootKey, rootID string, state *graphState) {
	for threadID, node := range state.nodes {
		update, ok := o.rolloutLatest[rolloutLatestKey(rootID, threadID)]
		if !ok {
			continue
		}
		node.node.Usage = update.Total
		node.node.Billing = mergeBilling(node.node.Billing, update.Identity)
	}
}
