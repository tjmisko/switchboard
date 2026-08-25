package codex

import (
	"context"
	"errors"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/history"
	"github.com/tjmisko/switchboard/internal/pricing"
)

// HistoryUsageRecorder maps Codex telemetry into the provider-neutral history
// schema. It deliberately owns no pricing logic: the pricing manager selects a
// live/LKG catalog when readers estimate each canonical token delta.
type HistoryUsageRecorder struct {
	sink *history.Sink
}

func NewHistoryUsageRecorder(sink *history.Sink) *HistoryUsageRecorder {
	return &HistoryUsageRecorder{sink: sink}
}

func (r *HistoryUsageRecorder) PersistUsage(ctx context.Context, update UsageUpdate) error {
	if r == nil || r.sink == nil {
		return errRolloutRecorderUnavailable
	}
	coverage := update.Coverage
	// Rollout token counters do not expose potentially billable OpenAI tool
	// units (for example web-search calls). Until an exact provider field is
	// available, API-route estimates must disclose that they are token-only.
	if coverage == "" && update.Identity.BillingRoute == "api" {
		coverage = "tokens_only"
	}
	delta := historyUsage(update.Delta)
	total := historyUsage(update.Total)
	eventType := history.EventUsageSample
	usage := &delta
	if coverage != "" && update.Delta.IsZero() {
		eventType = history.EventUsageCutover
		usage = nil
	}
	event := history.Event{
		SchemaVersion: history.HistorySchemaVersion, Ts: update.ObservedAt, Type: eventType,
		SessionID: update.RootSessionID, PID: update.RootKey.PID, Agent: "codex",
		ThreadID: update.ThreadID, ParentThreadID: update.ParentThreadID, TurnID: update.TurnID,
		UsageEventID: update.UpdateID, UsageSnapshot: true, UsageRevision: update.Revision,
		UsageSourceID: string(update.Source), UsageCoverage: coverage, Usage: usage, UsageTotal: &total,
		ExecutionProvider: update.Identity.ExecutionProvider, BillingRoute: update.Identity.BillingRoute,
		AccountKind: update.Identity.AccountKind, AuthMode: update.Identity.AuthMode,
		Model: update.Identity.Model, ServiceTier: update.Identity.ServiceTier,
		Speed: update.Identity.Speed, InferenceGeo: update.Identity.InferenceGeo,
		ReasoningEffort: update.Identity.ReasoningEffort,
		// Legacy fields remain populated for old CLI readers. Canonical readers use
		// Usage when present and therefore never add both representations.
		TokIn: update.Delta.InputTokens, TokOut: update.Delta.OutputTokens,
		TokCacheRead: update.Delta.CachedInputTokens, TokCacheCreate: update.Delta.CacheWriteInputTokens,
		TokCacheWrite: update.Delta.CacheWriteInputTokens, TokReasoningOut: update.Delta.ReasoningOutputTokens,
		TokTotal: update.Delta.TotalTokens, ModelContextWindow: update.Delta.ModelContextWindow,
	}
	_, err := r.sink.RecordDurable(ctx, event)
	return err
}

func (r *HistoryUsageRecorder) PersistVendorUsage(ctx context.Context, update UsageUpdate) error {
	if r == nil || r.sink == nil {
		return errRolloutRecorderUnavailable
	}
	if update.VendorEstimate == nil {
		return errors.New("vendor usage snapshot missing")
	}
	snapshot := historyVendorUsage(*update.VendorEstimate, update.Revision)
	event := history.Event{
		SchemaVersion: history.HistorySchemaVersion, Ts: update.ObservedAt, Type: history.EventVendorUsageSnapshot,
		SessionID: update.RootSessionID, PID: update.RootKey.PID, Agent: "codex",
		ThreadID: update.ThreadID, ParentThreadID: update.ParentThreadID, TurnID: update.TurnID,
		UsageEventID: update.UpdateID, UsageSnapshot: true, UsageRevision: update.Revision,
		UsageSourceID: string(update.Source), VendorUsage: &snapshot,
		ExecutionProvider: update.Identity.ExecutionProvider, BillingRoute: update.Identity.BillingRoute,
		AccountKind: update.Identity.AccountKind, AuthMode: update.Identity.AuthMode,
		Model: update.Identity.Model, ServiceTier: update.Identity.ServiceTier,
		Speed: update.Identity.Speed, InferenceGeo: update.Identity.InferenceGeo,
		ReasoningEffort: update.Identity.ReasoningEffort,
	}
	_, err := r.sink.RecordDurable(ctx, event)
	return err
}

func historyUsage(usage agentgraph.Usage) history.UsageDelta {
	return history.UsageDelta{
		InputTokens: usage.InputTokens, CachedInputTokens: usage.CachedInputTokens,
		CacheWriteInputTokens: usage.CacheWriteInputTokens, OutputTokens: usage.OutputTokens,
		ReasoningOutputTokens: usage.ReasoningOutputTokens, TotalTokens: usage.TotalTokens,
		ModelContextWindow: usage.ModelContextWindow,
	}
}

func historyVendorUsage(estimate ThreadUsageEstimate, revision int64) history.VendorUsageSnapshot {
	groups := make([]pricing.VendorUsageGroup, len(estimate.Groups))
	for i, group := range estimate.Groups {
		groups[i] = pricing.VendorUsageGroup{
			Model: cloneString(group.Model), ReasoningEffort: cloneString(group.ReasoningEffort), Speed: cloneString(group.Speed),
			InputTokens: cloneInt64(group.InputTokens), CachedInputTokens: cloneInt64(group.CachedInputTokens),
			NetNewInputTokens: cloneInt64(group.NetNewInputTokens), OutputTokens: cloneInt64(group.OutputTokens),
			TotalTokens:           cloneInt64(group.TotalTokens),
			EstimatedUsageCredits: pricing.CreditsFromMicros(group.EstimatedUsageCreditsMicros),
		}
	}
	var usd *pricing.USD
	if estimate.EstimatedUsageUSDMicros != nil {
		value := pricing.USDFromMicros(*estimate.EstimatedUsageUSDMicros)
		usd = &value
	}
	return history.VendorUsageSnapshot{
		ThreadID: estimate.ThreadID, EstimatedUsageCredits: pricing.CreditsFromMicros(estimate.EstimatedUsageCreditsMicros),
		EstimatedUsageUSD: usd, Groups: groups, ObservedAt: estimate.ObservedAt,
		Revision: revision, Stale: estimate.Stale,
	}
}
