package pricing

import (
	"math"
	"testing"
	"time"
)

func freshCatalogs(now time.Time) CatalogSet {
	set := BootstrapCatalogs()
	for provider, catalog := range set {
		catalog.Bundled = false
		catalog.RetrievedAt = now
		set[provider] = catalog
	}
	return set
}

func TestEstimateClaudePreservesCacheTTLsGeoAndToolCharges(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	estimate := EstimateRequest(freshCatalogs(now), Identity{
		AgentClient: "claude", ExecutionProvider: ProviderAnthropic, BillingRoute: "api",
		Model: "claude-opus-4-8", InferenceGeo: "us",
	}, Usage{
		InputTokens: 1_000_000, CachedInputTokens: 1_000_000,
		CacheWrite5mInputTokens: 1_000_000, CacheWrite1hInputTokens: 1_000_000,
		OutputTokens: 1_000_000, WebSearchRequests: 2,
	}, now)
	// Token subtotal $46.75, US multiplier 1.1 => $51.425; two searches => $0.02.
	assertUSD(t, estimate.APIEquivalentUSD, 51.445)
	assertUSD(t, estimate.EstimatedBilledUSD, 51.445)
	if estimate.Status != CostEstimated || estimate.Coverage != 1 {
		t.Fatalf("estimate = %+v", estimate)
	}
}

func TestEstimateClaudeCombinedCacheWriteIsPartial(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	estimate := EstimateRequest(freshCatalogs(now), Identity{
		ExecutionProvider: ProviderAnthropic, Model: "claude-opus-4-8",
	}, Usage{InputTokens: 1_000_000, CacheWriteInputTokens: 1_000_000}, now)
	assertUSD(t, estimate.APIEquivalentUSD, 5)
	if estimate.Status != CostPartial || estimate.UnpricedTokens != 1_000_000 || estimate.Coverage != 0.5 {
		t.Fatalf("estimate = %+v", estimate)
	}
}

func TestEstimateClaudeCodeExecutionBillingFailsClosedUnlessWebMakesItFree(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	identity := Identity{ExecutionProvider: ProviderAnthropic, Model: "claude-opus-4-8"}

	partial := EstimateRequest(freshCatalogs(now), identity, Usage{
		InputTokens: 1_000_000, CodeExecutionRequests: 1,
	}, now)
	assertUSD(t, partial.APIEquivalentUSD, 5)
	if partial.Status != CostPartial || partial.UnpricedToolUnits != 1 || len(partial.UnpricedReasons) != 1 {
		t.Fatalf("container-hour uncertainty = %+v", partial)
	}

	freeWithWeb := EstimateRequest(freshCatalogs(now), identity, Usage{
		InputTokens: 1_000_000, WebFetchRequests: 1, CodeExecutionRequests: 1,
	}, now)
	assertUSD(t, freeWithWeb.APIEquivalentUSD, 5)
	if freeWithWeb.Status != CostEstimated || freeWithWeb.UnpricedToolUnits != 0 || freeWithWeb.Coverage != 1 {
		t.Fatalf("code execution with web fetch = %+v", freeWithWeb)
	}
}

func TestEstimateClaudeUnknownServerToolFailsClosed(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	estimate := EstimateRequest(freshCatalogs(now), Identity{
		ExecutionProvider: ProviderAnthropic, Model: "claude-opus-4-8",
	}, Usage{InputTokens: 1_000_000, UnclassifiedServerToolUnits: 3}, now)
	assertUSD(t, estimate.APIEquivalentUSD, 5)
	if estimate.Status != CostPartial || estimate.UnpricedToolUnits != 3 || len(estimate.UnpricedReasons) != 1 {
		t.Fatalf("unknown server tool = %+v", estimate)
	}
}

func TestEstimateClaudePriorityKeepsSpotEquivalentButNotContractBill(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	estimate := EstimateRequest(freshCatalogs(now), Identity{
		ExecutionProvider: ProviderAnthropic, BillingRoute: "api", Model: "claude-opus-4-8", ServiceTier: "priority",
	}, Usage{InputTokens: 1_000_000}, now)
	assertUSD(t, estimate.APIEquivalentUSD, 5)
	if estimate.EstimatedBilledUSD != nil || estimate.Status != CostPartial || estimate.UnpricedEvents != 1 ||
		len(estimate.UnpricedReasons) != 1 {
		t.Fatalf("priority capacity semantics = %+v", estimate)
	}

	fast := EstimateRequest(freshCatalogs(now), Identity{
		ExecutionProvider: ProviderAnthropic, Model: "claude-opus-4-8", ServiceTier: "priority", Speed: "fast",
	}, Usage{InputTokens: 1_000_000}, now)
	assertUSD(t, fast.APIEquivalentUSD, 10)
	if fast.Status != CostPartial {
		t.Fatalf("fast priority comparator = %+v", fast)
	}
}

func TestEstimateClaudeUnsupportedBatchTierFailsClosed(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	estimate := EstimateRequest(freshCatalogs(now), Identity{
		ExecutionProvider: ProviderAnthropic, Model: "claude-opus-4-8", ServiceTier: "batch",
	}, Usage{InputTokens: 1_000_000}, now)
	if estimate.APIEquivalentUSD != nil || estimate.Status != CostUnknown || len(estimate.UnpricedReasons) == 0 {
		t.Fatalf("unsupported batch estimate = %+v", estimate)
	}
}

func TestEstimateOpenAIDoesNotDoubleChargeCachedWriteOrReasoning(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	estimate := EstimateRequest(freshCatalogs(now), Identity{
		AgentClient: "codex", ExecutionProvider: ProviderOpenAI, BillingRoute: "api", Model: "gpt-5.6-sol",
	}, Usage{
		InputTokens: 1_000_000, CachedInputTokens: 600_000, CacheWriteInputTokens: 100_000,
		OutputTokens: 200_000, ReasoningOutputTokens: 80_000,
	}, now)
	// The 1M-token request uses the long card: 300k*$8 + 600k*$0.80 +
	// 100k*$10 + 200k*$30 = $9.88. Reasoning remains a subset of output.
	assertUSD(t, estimate.APIEquivalentUSD, 9.88)
	assertUSD(t, estimate.EstimatedBilledUSD, 9.88)
	if estimate.PricedTokens != 1_200_000 || estimate.Status != CostEstimated {
		t.Fatalf("estimate = %+v", estimate)
	}
}

func TestEstimateOpenAIPricesLongContextPerRequest(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	estimate := EstimateRequest(freshCatalogs(now), Identity{
		ExecutionProvider: ProviderOpenAI, Model: "gpt-5.6-sol",
	}, Usage{InputTokens: 300_000, OutputTokens: 100_000}, now)
	// >272k applies $8 input and $30 output to the whole request.
	assertUSD(t, estimate.APIEquivalentUSD, 5.4)
	if estimate.Status != CostEstimated {
		t.Fatalf("status = %s", estimate.Status)
	}
}

func TestEstimateOpenAIFastLongContextUsesPublicationVariant(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	estimate := EstimateRequest(freshCatalogs(now), Identity{
		ExecutionProvider: ProviderOpenAI, Model: "gpt-5.6-sol", Speed: "fast", ServiceTier: "priority",
	}, Usage{InputTokens: 300_000, OutputTokens: 100_000}, now)
	// Fast long-context Sol: 300k*$16 + 100k*$60 = $10.80.
	assertUSD(t, estimate.APIEquivalentUSD, 10.8)
	if estimate.Status != CostEstimated {
		t.Fatalf("estimate = %+v", estimate)
	}
}

func TestEstimateOpenAIUltrafastIsExplicitlyUnpriced(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	estimate := EstimateRequest(freshCatalogs(now), Identity{
		ExecutionProvider: ProviderOpenAI, Model: "gpt-5.6-sol", ServiceTier: "ultrafast",
	}, Usage{InputTokens: 100_000}, now)
	if estimate.APIEquivalentUSD != nil || estimate.Status != CostUnknown || estimate.UnpricedEvents != 1 ||
		len(estimate.UnpricedReasons) == 0 || estimate.UnpricedReasons[0] != "price variant service_tier=ultrafast is unavailable" {
		t.Fatalf("unsupported ultrafast estimate = %+v", estimate)
	}
}

func TestEstimateOpenAIRegionalUsesExplicitUplift(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	estimate := EstimateRequest(freshCatalogs(now), Identity{
		ExecutionProvider: ProviderOpenAI, Model: "gpt-5.6-terra", InferenceGeo: "regional",
	}, Usage{InputTokens: 100_000}, now)
	assertUSD(t, estimate.APIEquivalentUSD, 0.22)

	unknownAlias := EstimateRequest(freshCatalogs(now), Identity{
		ExecutionProvider: ProviderOpenAI, Model: "gpt-5.6-terra", InferenceGeo: "us",
	}, Usage{InputTokens: 100_000}, now)
	if unknownAlias.Status != CostUnknown || unknownAlias.APIEquivalentUSD != nil ||
		len(unknownAlias.UnpricedReasons) == 0 || unknownAlias.UnpricedReasons[0] != "inference geography price is unavailable" {
		t.Fatalf("ambiguous region must fail explicitly: %+v", unknownAlias)
	}
}

func TestEstimateSubscriptionKeepsAPIEquivalentSeparateFromBilledUSD(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	estimate := EstimateRequest(freshCatalogs(now), Identity{
		ExecutionProvider: ProviderOpenAI, BillingRoute: "chatgpt_subscription", Model: "gpt-5.6-sol",
	}, Usage{InputTokens: 1_000_000}, now)
	assertUSD(t, estimate.APIEquivalentUSD, 8)
	if estimate.EstimatedBilledUSD != nil || estimate.Status != CostIncluded {
		t.Fatalf("estimate = %+v", estimate)
	}
}

func TestEstimateUniqueModelWithUnknownProviderIsPartialComparisonOnly(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	estimate := EstimateRequest(freshCatalogs(now), Identity{
		BillingRoute: "api", Model: "claude-opus-4-8",
	}, Usage{InputTokens: 1_000_000}, now)
	assertUSD(t, estimate.APIEquivalentUSD, 5)
	if estimate.EstimatedBilledUSD != nil || estimate.Status != CostPartial || estimate.UnpricedEvents != 1 ||
		len(estimate.UnpricedReasons) == 0 {
		t.Fatalf("inferred-provider estimate = %+v", estimate)
	}
}

func TestEstimateUnknownModelAndCloudProviderNeverBecomeZero(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	for _, identity := range []Identity{
		{ExecutionProvider: ProviderAnthropic, Model: "claude-future-99"},
		{ExecutionProvider: "aws-bedrock", Model: "claude-opus-4-8"},
	} {
		estimate := EstimateRequest(freshCatalogs(now), identity, Usage{InputTokens: 1_000}, now)
		if estimate.APIEquivalentUSD != nil || estimate.Status != CostUnknown || estimate.UnpricedEvents != 1 {
			t.Fatalf("identity %+v estimate = %+v", identity, estimate)
		}
	}
}

func TestEstimateStaleCatalogIsExplicit(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	set := freshCatalogs(now)
	catalog := set[ProviderAnthropic]
	catalog.RetrievedAt = now.Add(-48 * time.Hour)
	set[ProviderAnthropic] = catalog
	estimate := EstimateRequest(set, Identity{ExecutionProvider: ProviderAnthropic, Model: "claude-opus-4-8"}, Usage{InputTokens: 1_000_000}, now)
	assertUSD(t, estimate.APIEquivalentUSD, 5)
	if estimate.Status != CostStale {
		t.Fatalf("status = %s, want stale", estimate.Status)
	}
}

func TestEstimateExpiredBundledCatalogFailsClosed(t *testing.T) {
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	estimate := EstimateRequest(BootstrapCatalogs(), Identity{
		ExecutionProvider: ProviderAnthropic, Model: "claude-opus-4-8",
	}, Usage{InputTokens: 1_000_000}, now)
	if estimate.APIEquivalentUSD != nil || estimate.Status != CostUnknown || estimate.UnpricedEvents != 1 {
		t.Fatalf("expired bundled estimate = %+v", estimate)
	}
}

func TestMergeEstimatesCarriesPartialCoverageAndMixedProvenance(t *testing.T) {
	a := Estimate{
		APIEquivalentUSD: usdPtr(mustUSD("2")), Status: CostEstimated, Coverage: 1,
		PricedTokens: 100, PricingProvider: ProviderAnthropic, PricingSource: "a", PricingVersion: "v1",
	}
	b := Estimate{
		Status: CostUnknown, UnpricedTokens: 100, UnpricedEvents: 1,
		PricingProvider: ProviderOpenAI, PricingSource: "b", PricingVersion: "v2",
	}
	got := MergeEstimates(a, b)
	assertUSD(t, got.APIEquivalentUSD, 2)
	if got.Status != CostPartial || got.Coverage != 0.5 || !got.MixedPricing || got.PricingProvider != "mixed" {
		t.Fatalf("merged = %+v", got)
	}
}

func TestMergeEstimatesMissingProvenanceIsNotMixed(t *testing.T) {
	known := Estimate{PricingProvider: ProviderAnthropic, PricingSource: "official", PricingVersion: "v1", PricingKind: "spot_estimate"}
	missing := Estimate{PricingKind: "spot_estimate", Status: CostUnknown}
	got := MergeEstimates(known, missing)
	if got.MixedPricing || got.PricingProvider != ProviderAnthropic || got.PricingSource != "official" || got.PricingVersion != "v1" {
		t.Fatalf("merged provenance = %+v", got)
	}
}

func TestMergeEstimatesNonAPIAmtKnownPlusUnknownIsPartial(t *testing.T) {
	credits := CreditsFromMicros(2_000_000)
	billed := USDFromMicros(3_000_000)
	tests := []struct {
		name  string
		known Estimate
		check func(Estimate) bool
	}{
		{
			name:  "plan credits",
			known: Estimate{PlanCredits: &credits, Status: CostEstimated, Coverage: 1},
			check: func(got Estimate) bool {
				return got.PlanCredits != nil && got.PlanCredits.Micros() == credits.Micros()
			},
		},
		{
			name:  "estimated billed USD",
			known: Estimate{EstimatedBilledUSD: &billed, Status: CostEstimated, Coverage: 1},
			check: func(got Estimate) bool {
				return got.EstimatedBilledUSD != nil && got.EstimatedBilledUSD.Micros() == billed.Micros()
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			unknown := Estimate{Status: CostUnknown, UnpricedEvents: 1, UnpricedTokens: 100}
			got := MergeEstimates(test.known, unknown)
			if !test.check(got) || got.Status != CostPartial {
				t.Fatalf("non-API amount merge = %+v", got)
			}
		})
	}
}

func TestMergeEstimatesRetainsEveryOfficialSourceURL(t *testing.T) {
	anthropic := Estimate{PricingProvider: ProviderAnthropic, PricingSource: AnthropicPricingURL, PricingSources: []string{AnthropicPricingURL}, PricingVersion: "a", PricingVersions: []string{"a"}}
	openai := Estimate{PricingProvider: ProviderOpenAI, PricingSource: OpenAIPricingURL, PricingSources: []string{OpenAIPricingURL}, PricingVersion: "b", PricingVersions: []string{"b"}}
	got := MergeEstimates(anthropic, openai)
	if len(got.PricingSources) != 2 || got.PricingSources[0] != OpenAIPricingURL || got.PricingSources[1] != AnthropicPricingURL {
		t.Fatalf("pricing sources = %+v", got.PricingSources)
	}
	if len(got.PricingVersions) != 2 || got.PricingVersions[0] != "a" || got.PricingVersions[1] != "b" {
		t.Fatalf("pricing versions = %+v", got.PricingVersions)
	}
}

func assertUSD(t *testing.T, got *USD, want float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("cost = nil, want %.6f", want)
	}
	if math.Abs(got.Float64()-want) > 0.0000001 {
		t.Fatalf("cost = %.6f, want %.6f", got.Float64(), want)
	}
}
