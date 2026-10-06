package main

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"strconv"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/pricing"
	"github.com/tjmisko/switchboard/internal/provider"
	"github.com/tjmisko/switchboard/internal/rpc"
	"github.com/tjmisko/switchboard/internal/state"
)

// piUsageSeenCap bounds the per-root dedupe memory. Pi delivers each
// message_end once; the set only has to outlast a redelivery, not a session.
const piUsageSeenCap = 1024

// piMaxReportedCostUSD rejects a per-message cost no request could run up,
// which also keeps the micro-dollar conversion far from overflow.
const piMaxReportedCostUSD = 1_000_000

// piUsageSeen is the bounded set of usage keys a root has already counted,
// evicted oldest first.
type piUsageSeen struct {
	keys  map[string]struct{}
	order []string
}

// add records key and reports whether it was new.
func (s *piUsageSeen) add(key string) bool {
	if _, ok := s.keys[key]; ok {
		return false
	}
	if s.keys == nil {
		s.keys = make(map[string]struct{})
	}
	if len(s.order) == piUsageSeenCap {
		delete(s.keys, s.order[0])
		s.order = s.order[1:]
	}
	s.keys[key] = struct{}{}
	s.order = append(s.order, key)
	return true
}

// piUsageEventID is the stable identity of one Pi message's usage: Pi's
// message id when it supplies one, else the hook's own instant and counts,
// which a redelivered hook repeats. History readers upsert by it, so even a
// copy that escapes the in-memory set is counted once.
func piUsageEventID(sessionID string, at time.Time, usage rpc.HookUsage) string {
	key := "message\x00" + sessionID + "\x00" + usage.MessageID
	if usage.MessageID == "" {
		cost := "none"
		if usage.CostTotal != nil {
			cost = strconv.FormatFloat(*usage.CostTotal, 'g', -1, 64)
		}
		key = "hook\x00" + sessionID + "\x00" + strconv.FormatInt(at.UnixNano(), 10) + "\x00" +
			usage.Provider + "\x00" + usage.Model + "\x00" +
			strconv.FormatInt(usage.InputTokens, 10) + "\x00" + strconv.FormatInt(usage.OutputTokens, 10) + "\x00" +
			strconv.FormatInt(usage.CacheReadTokens, 10) + "\x00" + strconv.FormatInt(usage.CacheWriteTokens, 10) + "\x00" +
			strconv.FormatInt(usage.TotalTokens, 10) + "\x00" + cost
	}
	sum := sha256.Sum256([]byte(key))
	return "pi:" + hex.EncodeToString(sum[:16])
}

// piReportedCost is Pi's own cost for one message as a client-reported
// estimate, which history never reprices (decision 5). A message Pi priced at
// nothing it could report stays visibly unpriced rather than being priced
// from the catalog instead.
func piReportedCost(usage rpc.HookUsage) *history.CostEstimate {
	tokens := usage.InputTokens + usage.OutputTokens + usage.CacheReadTokens + usage.CacheWriteTokens
	estimate := &history.CostEstimate{PricingKind: pricing.PricingKindClientReported, PricingProvider: state.AgentKindPi}
	if usage.CostTotal == nil || *usage.CostTotal < 0 || *usage.CostTotal > piMaxReportedCostUSD {
		estimate.Status = pricing.CostUnknown
		estimate.UnpricedTokens, estimate.UnpricedEvents = tokens, 1
		estimate.UnpricedReasons = []string{"pi reported no cost for this message"}
		return estimate
	}
	usd := pricing.USDFromMicros(int64(math.Round(*usage.CostTotal * 1_000_000)))
	estimate.APIEquivalentUSD = &usd
	estimate.Status, estimate.Coverage, estimate.PricedTokens = pricing.CostEstimated, 1, tokens
	return estimate
}

// applyPiUsage accounts one Pi assistant message (phase-1 plan, 1.9). The
// tokens and Pi's own cost go to history as a usage_sample under a Pi billing
// identity (agent client pi, provider and model as Pi reported them), and the
// bound session's running totals go onto its graph root. A message is counted
// once: by Pi's message id when it supplies one, else once per message_end
// hook. Usage is no status evidence, so it neither moves the status nor
// renews the hook lease, and it is not ordered against status hooks.
func (c *agentCoordinator) applyPiUsage(key provider.RootKey, root *piHookRoot, req rpc.Request, sess state.Session, now time.Time) {
	if req.Usage == nil {
		return
	}
	usage := *req.Usage
	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = root.sessionID
	}
	if sessionID == "" {
		c.recordDiagnostic(agentgraph.ProviderPi, "exact_binding_unavailable", now)
		return
	}
	eventID := piUsageEventID(sessionID, now, usage)
	if !root.usageSeen.add(eventID) {
		c.recordDiagnostic(agentgraph.ProviderPi, "duplicate_usage_ignored", now)
		return
	}
	delta := agentgraph.Usage{
		InputTokens: usage.InputTokens, CachedInputTokens: usage.CacheReadTokens,
		CacheWriteInputTokens: usage.CacheWriteTokens, OutputTokens: usage.OutputTokens,
		TotalTokens: usage.TotalTokens,
	}
	identity := agentgraph.BillingIdentity{AgentClient: state.AgentKindPi, ExecutionProvider: usage.Provider, Model: usage.Model}

	// Only the bound session's totals live on the graph. Usage for another
	// session (a rotation whose SessionStart has not landed) still reaches
	// history under its own session id.
	if sessionID == root.sessionID {
		root.usage = addPiUsage(root.usage, delta)
		root.billing = identity
		total, billing := root.usage, root.billing
		c.store.Apply(func(sessions map[int]*state.Session) {
			s := sessions[key.PID]
			if !sessionHoldsRoot(s, key) || s.Agent != state.AgentKindPi {
				return
			}
			s.SetPiUsage(sessionID, total, billing)
		})
	}

	canonical := history.UsageDelta{
		InputTokens: delta.InputTokens, CachedInputTokens: delta.CachedInputTokens,
		CacheWriteInputTokens: delta.CacheWriteInputTokens, OutputTokens: delta.OutputTokens,
		TotalTokens: delta.TotalTokens,
	}
	c.sink.Record(history.Event{
		SchemaVersion: history.HistorySchemaVersion, Ts: now, Type: history.EventUsageSample,
		SessionID: sessionID, PID: key.PID, Agent: state.AgentKindPi, CWD: sess.CWD,
		Source:       agentgraph.SourceHook,
		UsageEventID: eventID, UsageSnapshot: true, UsageRevision: 1,
		ProviderMessageID: usage.MessageID,
		ExecutionProvider: usage.Provider, Model: usage.Model,
		Usage: &canonical, Cost: piReportedCost(usage),
		// Legacy fields remain populated for old CLI readers, as for Codex.
		TokIn: delta.InputTokens, TokOut: delta.OutputTokens,
		TokCacheRead: delta.CachedInputTokens, TokCacheCreate: delta.CacheWriteInputTokens,
		TokCacheWrite: delta.CacheWriteInputTokens, TokTotal: delta.TotalTokens,
	})
}

func addPiUsage(a, b agentgraph.Usage) agentgraph.Usage {
	return agentgraph.Usage{
		InputTokens: a.InputTokens + b.InputTokens, CachedInputTokens: a.CachedInputTokens + b.CachedInputTokens,
		CacheWriteInputTokens: a.CacheWriteInputTokens + b.CacheWriteInputTokens,
		OutputTokens:          a.OutputTokens + b.OutputTokens, ReasoningOutputTokens: a.ReasoningOutputTokens + b.ReasoningOutputTokens,
		TotalTokens: a.TotalTokens + b.TotalTokens, ModelContextWindow: max(a.ModelContextWindow, b.ModelContextWindow),
	}
}
