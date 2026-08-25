package codex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/provider"
)

const (
	billingRouteAPI                 = "api"
	billingRouteChatGPTSubscription = "chatgpt_subscription"
	billingRouteCredits             = "credits"
	billingRouteCloud               = "cloud"
)

// UsageUpdate is a bounded, content-free Codex usage event. Delta is the
// app-server's `last` usage for one update, or a guarded monotonic delta from
// `total` when `last` is absent. Total is the latest cumulative thread value.
// UpdateID is a stable digest of provider IDs and numeric usage only; it can be
// used as an idempotency key by a history sink.
type UsageUpdate struct {
	RootKey        provider.RootKey           `json:"-"`
	ThreadID       string                     `json:"thread_id"`
	TurnID         string                     `json:"turn_id,omitempty"`
	UpdateID       string                     `json:"update_id"`
	Source         agentgraph.SourceKind      `json:"source"`
	Identity       agentgraph.BillingIdentity `json:"identity"`
	Delta          agentgraph.Usage           `json:"delta"`
	Total          agentgraph.Usage           `json:"total"`
	VendorEstimate *ThreadUsageEstimate       `json:"vendor_estimate,omitempty"`
	ObservedAt     time.Time                  `json:"observed_at"`
}

// ThreadUsageEstimate is the optional provider-native estimate returned by
// account/usage/read for a requested thread. Dollar micros remain a pointer so
// a missing vendor estimate can never collapse to a believable zero.
type ThreadUsageEstimate struct {
	ThreadID                    string             `json:"threadId"`
	EstimatedUsageCreditsMicros int64              `json:"estimatedUsageCreditsMicros"`
	EstimatedUsageUSDMicros     *int64             `json:"estimatedUsageUsdMicros"`
	Groups                      []ThreadUsageGroup `json:"groups"`
}

// ThreadUsageGroup retains the provider's per-model/effort/speed breakdown.
// Nullable token fields remain pointers to distinguish unavailable from zero.
type ThreadUsageGroup struct {
	Model                       *string `json:"model"`
	ReasoningEffort             *string `json:"reasoningEffort"`
	Speed                       *string `json:"speed"`
	InputTokens                 *int64  `json:"inputTokens"`
	CachedInputTokens           *int64  `json:"cachedInputTokens"`
	NetNewInputTokens           *int64  `json:"netNewInputTokens"`
	OutputTokens                *int64  `json:"outputTokens"`
	TotalTokens                 *int64  `json:"totalTokens"`
	EstimatedUsageCreditsMicros int64   `json:"estimatedUsageCreditsMicros"`
}

type rpcTokenUsageBreakdown struct {
	InputTokens           int64 `json:"inputTokens"`
	CachedInputTokens     int64 `json:"cachedInputTokens"`
	CacheWriteInputTokens int64 `json:"cacheWriteInputTokens"`
	OutputTokens          int64 `json:"outputTokens"`
	ReasoningOutputTokens int64 `json:"reasoningOutputTokens"`
	TotalTokens           int64 `json:"totalTokens"`
}

type rpcThreadTokenUsage struct {
	Last               *rpcTokenUsageBreakdown `json:"last"`
	Total              *rpcTokenUsageBreakdown `json:"total"`
	ModelContextWindow *int64                  `json:"modelContextWindow"`
}

type accountReadResult struct {
	Account            *rpcAccount `json:"account"`
	RequiresOpenAIAuth bool        `json:"requiresOpenaiAuth"`
}

// rpcAccount deliberately omits personal account fields such as email. PlanType
// is used only to classify usage-based billing and is never retained verbatim.
type rpcAccount struct {
	Type     string `json:"type"`
	PlanType string `json:"planType"`
}

type accountMetadata struct {
	known             bool
	accountKind       string
	billingRoute      string
	executionProvider string
}

type accountUsageReadResult struct {
	ThreadUsage *ThreadUsageEstimate `json:"threadUsage"`
}

// UsageUpdates returns the bounded stream of per-turn usage deltas and
// provider-native estimate updates. Consumers must persist UpdateID
// idempotently and periodically reconcile against Observe's cumulative totals,
// because a deliberately bounded channel may discard an oldest unread event.
func (o *Observer) UsageUpdates() <-chan UsageUpdate {
	return o.usageUpdates
}

// LatestThreadUsage returns a detached copy of the newest provider-native
// estimate retained for a thread.
func (o *Observer) LatestThreadUsage(threadID string) (ThreadUsageEstimate, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, record := range o.roots {
		if record.vendorEstimate != nil && record.vendorEstimate.ThreadID == threadID {
			return cloneThreadUsageEstimate(*record.vendorEstimate), true
		}
	}
	return ThreadUsageEstimate{}, false
}

func accountMetadataFromType(accountType string) accountMetadata {
	switch accountType {
	case "apiKey", "apikey":
		return accountMetadata{known: true, accountKind: "api_key", billingRoute: billingRouteAPI}
	case "chatgpt":
		return accountMetadata{known: true, accountKind: "chatgpt", billingRoute: billingRouteChatGPTSubscription}
	case "chatgptAuthTokens":
		return accountMetadata{known: true, accountKind: "chatgpt_auth_tokens", billingRoute: billingRouteChatGPTSubscription}
	case "agentIdentity":
		return accountMetadata{known: true, accountKind: "agent_identity", billingRoute: billingRouteChatGPTSubscription}
	case "personalAccessToken":
		return accountMetadata{known: true, accountKind: "personal_access_token", billingRoute: billingRouteChatGPTSubscription}
	case "amazonBedrock", "bedrockApiKey":
		return accountMetadata{known: true, accountKind: "amazon_bedrock", billingRoute: billingRouteCloud, executionProvider: "aws-bedrock"}
	case "":
		return accountMetadata{known: true}
	default:
		return accountMetadata{known: true}
	}
}

func accountMetadataFromAccount(account rpcAccount) accountMetadata {
	metadata := accountMetadataFromType(account.Type)
	if metadata.billingRoute == billingRouteChatGPTSubscription && usageBasedPlan(account.PlanType) {
		metadata.billingRoute = billingRouteCredits
	}
	return metadata
}

func accountMetadataFromAuthMode(authMode, planType string) accountMetadata {
	metadata := accountMetadataFromType(authMode)
	if metadata.billingRoute == billingRouteChatGPTSubscription && usageBasedPlan(planType) {
		metadata.billingRoute = billingRouteCredits
	}
	return metadata
}

func usageBasedPlan(planType string) bool {
	switch planType {
	case "self_serve_business_usage_based", "enterprise_cbp_usage_based":
		return true
	default:
		return false
	}
}

func (s *graphState) applyAccountMetadata(account accountMetadata) bool {
	if !account.known {
		return false
	}
	changed := false
	for _, state := range s.nodes {
		identity := state.node.Billing
		identity.AgentClient = string(agentgraph.ProviderCodex)
		identity.AccountKind = account.accountKind
		identity.BillingRoute = account.billingRoute
		if identity.ExecutionProvider == "" && account.executionProvider != "" {
			identity.ExecutionProvider = account.executionProvider
		}
		if identity != state.node.Billing {
			state.node.Billing = identity
			changed = true
		}
	}
	return changed
}

func (s *graphState) applyThreadSettings(threadID string, settings rpcThreadSettings) bool {
	state := s.nodes[threadID]
	if state == nil {
		return false
	}
	identity := state.node.Billing
	identity.AgentClient = string(agentgraph.ProviderCodex)
	identity.ExecutionProvider = strings.TrimSpace(settings.ModelProvider)
	identity.Model = strings.TrimSpace(settings.Model)
	identity.ServiceTier = strings.TrimSpace(settings.ServiceTier)
	identity.ReasoningEffort = strings.TrimSpace(settings.Effort)
	if identity == state.node.Billing {
		return false
	}
	state.node.Billing = identity
	return true
}

func (s *graphState) mergeTelemetryFrom(previous *graphState) {
	if previous == nil {
		return
	}
	for id, state := range s.nodes {
		prior := previous.nodes[id]
		if prior == nil {
			continue
		}
		if state.node.Billing.Model == "" {
			state.node.Billing.Model = prior.node.Billing.Model
		}
		if state.node.Billing.ExecutionProvider == "" {
			state.node.Billing.ExecutionProvider = prior.node.Billing.ExecutionProvider
		}
		if state.node.Billing.ServiceTier == "" {
			state.node.Billing.ServiceTier = prior.node.Billing.ServiceTier
		}
		if state.node.Billing.ReasoningEffort == "" {
			state.node.Billing.ReasoningEffort = prior.node.Billing.ReasoningEffort
		}
		if state.node.Billing.BillingRoute == "" {
			state.node.Billing.BillingRoute = prior.node.Billing.BillingRoute
		}
		if state.node.Billing.AccountKind == "" {
			state.node.Billing.AccountKind = prior.node.Billing.AccountKind
		}
		if !prior.node.Usage.IsZero() {
			state.node.Usage = prior.node.Usage
		}
	}
}

func (o *Observer) applyTokenUsageLocked(key provider.RootKey, record *rootRecord, params notificationParams, eventAt time.Time) bool {
	if record == nil || record.graph == nil || params.ThreadID == "" || params.TurnID == "" {
		return false
	}
	state := record.graph.nodes[params.ThreadID]
	if state == nil || !validThreadTokenUsage(params.TokenUsage) {
		return false
	}
	if o.usageTotals == nil {
		o.usageTotals = make(map[string]agentgraph.Usage)
	}

	fingerprint := tokenUsageFingerprint(params.ThreadID, params.TurnID, params.TokenUsage)
	duplicate := o.seenUsageFingerprintLocked(fingerprint)
	previous, hadPrevious := o.usageTotals[params.ThreadID]
	total := state.node.Usage
	if params.TokenUsage.Total != nil {
		total = usageFromBreakdown(*params.TokenUsage.Total, params.TokenUsage.ModelContextWindow)
	} else if params.TokenUsage.Last != nil {
		var ok bool
		total, ok = addUsage(total, usageFromBreakdown(*params.TokenUsage.Last, params.TokenUsage.ModelContextWindow))
		if !ok {
			return false
		}
	}
	if params.TokenUsage.ModelContextWindow == nil && state.node.Usage.ModelContextWindow > 0 {
		total.ModelContextWindow = state.node.Usage.ModelContextWindow
	}
	changed := total != state.node.Usage
	state.node.Usage = total
	o.usageTotals[params.ThreadID] = total
	if duplicate {
		return changed
	}

	var delta agentgraph.Usage
	if params.TokenUsage.Last != nil {
		delta = usageFromBreakdown(*params.TokenUsage.Last, params.TokenUsage.ModelContextWindow)
	} else if params.TokenUsage.Total != nil {
		if !hadPrevious {
			delta = total
		} else if candidate, ok := subtractUsage(total, previous); ok {
			delta = candidate
		}
	}
	if usageTokensZero(delta) {
		return changed
	}
	if state.node.Billing.AgentClient == "" {
		state.node.Billing.AgentClient = string(agentgraph.ProviderCodex)
	}
	o.emitUsageUpdateLocked(UsageUpdate{
		RootKey: key, ThreadID: params.ThreadID, TurnID: params.TurnID,
		UpdateID: fingerprint, Source: agentgraph.SourceCodexAppServer,
		Identity: state.node.Billing, Delta: delta, Total: total, ObservedAt: eventAt,
	})
	return true
}

func (o *Observer) installVendorEstimateLocked(key provider.RootKey, record *rootRecord, estimate *ThreadUsageEstimate, eventAt time.Time) {
	if record == nil || estimate == nil || estimate.ThreadID != record.threadID || !validThreadUsageEstimate(*estimate) {
		return
	}
	copy := cloneThreadUsageEstimate(*estimate)
	record.vendorEstimate = &copy
	fingerprint := vendorUsageFingerprint(copy)
	if o.seenUsageFingerprintLocked(fingerprint) {
		return
	}
	identity := agentgraph.BillingIdentity{AgentClient: string(agentgraph.ProviderCodex)}
	total := agentgraph.Usage{}
	if state := record.graph.nodes[record.threadID]; state != nil {
		identity = state.node.Billing
		total = state.node.Usage
	}
	o.emitUsageUpdateLocked(UsageUpdate{
		RootKey: key, ThreadID: record.threadID, UpdateID: fingerprint,
		Source: agentgraph.SourceCodexAppServer, Identity: identity, Total: total,
		VendorEstimate: &copy, ObservedAt: eventAt,
	})
}

func (o *Observer) emitUsageUpdateLocked(update UsageUpdate) {
	if o.usageUpdates == nil {
		return
	}
	update = cloneUsageUpdate(update)
	select {
	case o.usageUpdates <- update:
		return
	default:
	}
	// The stream is intentionally bounded. Prefer the latest cumulative update;
	// consumers reconcile gaps against Node.Usage and stable UpdateID values.
	select {
	case <-o.usageUpdates:
	default:
	}
	select {
	case o.usageUpdates <- update:
	default:
	}
}

func (o *Observer) seenUsageFingerprintLocked(fingerprint string) bool {
	if fingerprint == "" {
		return true
	}
	if o.usageFingerprints == nil {
		o.usageFingerprints = make(map[string]struct{})
	}
	if _, exists := o.usageFingerprints[fingerprint]; exists {
		return true
	}
	limit := o.config.UsageDedupLimit
	if limit <= 0 {
		limit = DefaultUsageDedupLimit
	}
	if len(o.usageFingerprintOrder) >= limit {
		oldest := o.usageFingerprintOrder[0]
		delete(o.usageFingerprints, oldest)
		copy(o.usageFingerprintOrder, o.usageFingerprintOrder[1:])
		o.usageFingerprintOrder = o.usageFingerprintOrder[:len(o.usageFingerprintOrder)-1]
	}
	o.usageFingerprints[fingerprint] = struct{}{}
	o.usageFingerprintOrder = append(o.usageFingerprintOrder, fingerprint)
	return false
}

func (o *Observer) readAccount(client *rpcClient) accountMetadata {
	ctx, cancel := context.WithTimeout(o.ctx, o.config.UsageRequestTimeout)
	defer cancel()
	var result accountReadResult
	if err := client.request(ctx, "account/read", map[string]any{"refreshToken": false}, &result); err != nil {
		return accountMetadataFromType("")
	}
	if result.Account == nil {
		return accountMetadataFromType("")
	}
	return accountMetadataFromAccount(*result.Account)
}

func (o *Observer) readThreadUsage(client *rpcClient, threadID string) (*ThreadUsageEstimate, error) {
	ctx, cancel := context.WithTimeout(o.ctx, o.config.UsageRequestTimeout)
	defer cancel()
	var result accountUsageReadResult
	if err := client.request(ctx, "account/usage/read", map[string]any{"threadId": threadID}, &result); err != nil {
		return nil, err
	}
	if result.ThreadUsage == nil {
		return nil, nil
	}
	if result.ThreadUsage.ThreadID != threadID {
		return nil, errors.New("codex app-server returned usage for a different thread")
	}
	if !validThreadUsageEstimate(*result.ThreadUsage) {
		return nil, errors.New("codex app-server returned invalid thread usage")
	}
	copy := cloneThreadUsageEstimate(*result.ThreadUsage)
	return &copy, nil
}

func validThreadTokenUsage(usage rpcThreadTokenUsage) bool {
	if usage.Last == nil && usage.Total == nil {
		return false
	}
	if usage.ModelContextWindow != nil && *usage.ModelContextWindow < 0 {
		return false
	}
	return (usage.Last == nil || validBreakdown(*usage.Last)) && (usage.Total == nil || validBreakdown(*usage.Total))
}

func validBreakdown(usage rpcTokenUsageBreakdown) bool {
	return usage.InputTokens >= 0 && usage.CachedInputTokens >= 0 && usage.CacheWriteInputTokens >= 0 &&
		usage.OutputTokens >= 0 && usage.ReasoningOutputTokens >= 0 && usage.TotalTokens >= 0
}

func validThreadUsageEstimate(estimate ThreadUsageEstimate) bool {
	if estimate.ThreadID == "" || estimate.EstimatedUsageCreditsMicros < 0 || negativeInt64(estimate.EstimatedUsageUSDMicros) {
		return false
	}
	for _, group := range estimate.Groups {
		if group.EstimatedUsageCreditsMicros < 0 || negativeInt64(group.InputTokens) || negativeInt64(group.CachedInputTokens) ||
			negativeInt64(group.NetNewInputTokens) || negativeInt64(group.OutputTokens) || negativeInt64(group.TotalTokens) {
			return false
		}
	}
	return true
}

func negativeInt64(value *int64) bool { return value != nil && *value < 0 }

func usageFromBreakdown(usage rpcTokenUsageBreakdown, contextWindow *int64) agentgraph.Usage {
	out := agentgraph.Usage{
		InputTokens: usage.InputTokens, CachedInputTokens: usage.CachedInputTokens,
		CacheWriteInputTokens: usage.CacheWriteInputTokens, OutputTokens: usage.OutputTokens,
		ReasoningOutputTokens: usage.ReasoningOutputTokens, TotalTokens: usage.TotalTokens,
	}
	if contextWindow != nil {
		out.ModelContextWindow = *contextWindow
	}
	return out
}

func usageTokensZero(usage agentgraph.Usage) bool {
	usage.ModelContextWindow = 0
	return usage.IsZero()
}

func subtractUsage(current, previous agentgraph.Usage) (agentgraph.Usage, bool) {
	if current.InputTokens < previous.InputTokens || current.CachedInputTokens < previous.CachedInputTokens ||
		current.CacheWriteInputTokens < previous.CacheWriteInputTokens || current.OutputTokens < previous.OutputTokens ||
		current.ReasoningOutputTokens < previous.ReasoningOutputTokens || current.TotalTokens < previous.TotalTokens {
		return agentgraph.Usage{}, false
	}
	return agentgraph.Usage{
		InputTokens:           current.InputTokens - previous.InputTokens,
		CachedInputTokens:     current.CachedInputTokens - previous.CachedInputTokens,
		CacheWriteInputTokens: current.CacheWriteInputTokens - previous.CacheWriteInputTokens,
		OutputTokens:          current.OutputTokens - previous.OutputTokens,
		ReasoningOutputTokens: current.ReasoningOutputTokens - previous.ReasoningOutputTokens,
		TotalTokens:           current.TotalTokens - previous.TotalTokens,
		ModelContextWindow:    current.ModelContextWindow,
	}, true
}

func addUsage(left, right agentgraph.Usage) (agentgraph.Usage, bool) {
	values := [6][2]int64{
		{left.InputTokens, right.InputTokens}, {left.CachedInputTokens, right.CachedInputTokens},
		{left.CacheWriteInputTokens, right.CacheWriteInputTokens}, {left.OutputTokens, right.OutputTokens},
		{left.ReasoningOutputTokens, right.ReasoningOutputTokens}, {left.TotalTokens, right.TotalTokens},
	}
	for _, pair := range values {
		if pair[1] > math.MaxInt64-pair[0] {
			return agentgraph.Usage{}, false
		}
	}
	contextWindow := left.ModelContextWindow
	if right.ModelContextWindow > 0 {
		contextWindow = right.ModelContextWindow
	}
	return agentgraph.Usage{
		InputTokens:           left.InputTokens + right.InputTokens,
		CachedInputTokens:     left.CachedInputTokens + right.CachedInputTokens,
		CacheWriteInputTokens: left.CacheWriteInputTokens + right.CacheWriteInputTokens,
		OutputTokens:          left.OutputTokens + right.OutputTokens,
		ReasoningOutputTokens: left.ReasoningOutputTokens + right.ReasoningOutputTokens,
		TotalTokens:           left.TotalTokens + right.TotalTokens,
		ModelContextWindow:    contextWindow,
	}, true
}

func tokenUsageFingerprint(threadID, turnID string, usage rpcThreadTokenUsage) string {
	return digestStable(struct {
		Kind     string              `json:"kind"`
		ThreadID string              `json:"thread_id"`
		TurnID   string              `json:"turn_id"`
		Usage    rpcThreadTokenUsage `json:"usage"`
	}{Kind: "codex_token_usage_v1", ThreadID: threadID, TurnID: turnID, Usage: usage})
}

func vendorUsageFingerprint(estimate ThreadUsageEstimate) string {
	return digestStable(struct {
		Kind     string              `json:"kind"`
		Estimate ThreadUsageEstimate `json:"estimate"`
	}{Kind: "codex_vendor_usage_v1", Estimate: estimate})
}

func digestStable(value any) string {
	body, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func cloneUsageUpdate(update UsageUpdate) UsageUpdate {
	if update.VendorEstimate != nil {
		copy := cloneThreadUsageEstimate(*update.VendorEstimate)
		update.VendorEstimate = &copy
	}
	return update
}

func cloneThreadUsageEstimate(estimate ThreadUsageEstimate) ThreadUsageEstimate {
	estimate.EstimatedUsageUSDMicros = cloneInt64(estimate.EstimatedUsageUSDMicros)
	estimate.Groups = append([]ThreadUsageGroup(nil), estimate.Groups...)
	for i := range estimate.Groups {
		group := &estimate.Groups[i]
		group.Model = cloneString(group.Model)
		group.ReasoningEffort = cloneString(group.ReasoningEffort)
		group.Speed = cloneString(group.Speed)
		group.InputTokens = cloneInt64(group.InputTokens)
		group.CachedInputTokens = cloneInt64(group.CachedInputTokens)
		group.NetNewInputTokens = cloneInt64(group.NetNewInputTokens)
		group.OutputTokens = cloneInt64(group.OutputTokens)
		group.TotalTokens = cloneInt64(group.TotalTokens)
	}
	return estimate
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
