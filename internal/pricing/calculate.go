package pricing

import (
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"
)

// EstimateRequest prices one request/turn. Callers must not aggregate tokens
// across turns first because context bands, speed, service tier, and geography
// are request-level dimensions.
func EstimateRequest(catalogs CatalogSet, identity Identity, usage Usage, now time.Time) Estimate {
	estimate := Estimate{Status: CostUnknown, PricingKind: "spot_estimate"}
	providerInferred := identity.ExecutionProvider == ""
	if identity.Model == "" {
		return unknownEstimate(estimate, usage, "model is unknown")
	}
	if invalidUsage(usage) {
		return unknownEstimate(estimate, usage, "usage contains a negative value")
	}

	catalog, model, ok := catalogs.Resolve(identity.ExecutionProvider, identity.Model)
	if !ok {
		reason := "exact model price is unavailable"
		if identity.ExecutionProvider != "" {
			reason = "exact model price is unavailable for execution provider " + identity.ExecutionProvider
		}
		return unknownEstimate(estimate, usage, reason)
	}
	estimate.PricingProvider = catalog.Provider
	estimate.PricingSource = catalog.SourceURL
	estimate.PricingSources = []string{catalog.SourceURL}
	estimate.PricingRetrievedAt = timePtr(catalog.RetrievedAt)
	estimate.PricingEffectiveAt = cloneTimePtr(catalog.EffectiveAt)
	estimate.PricingVersion = catalog.VersionHash
	estimate.PricingVersions = []string{catalog.VersionHash}
	if catalog.FreshnessAt(now) == FreshnessUnusable {
		return unknownEstimate(estimate, usage, "price catalog is older than seven days")
	}

	rates, modifier, dimensionReason := selectRates(catalog.Provider, model, identity, usage)
	if dimensionReason != "" {
		return unknownEstimate(estimate, usage, dimensionReason)
	}

	var total USD
	addTokens := func(tokens int64, rate *USD, reason string) {
		if tokens == 0 {
			return
		}
		if rate == nil {
			estimate.UnpricedTokens += tokens
			estimate.UnpricedReasons = appendUnique(estimate.UnpricedReasons, reason)
			return
		}
		cost, err := chargeTokens(tokens, *rate)
		if err != nil {
			estimate.UnpricedTokens += tokens
			estimate.UnpricedReasons = appendUnique(estimate.UnpricedReasons, reason+": "+err.Error())
			return
		}
		cost, err = applyMultiplier(cost, modifier)
		if err != nil {
			estimate.UnpricedTokens += tokens
			estimate.UnpricedReasons = appendUnique(estimate.UnpricedReasons, reason+": "+err.Error())
			return
		}
		if next, ok := addUSD(total, cost); ok {
			total = next
			estimate.PricedTokens += tokens
		} else {
			estimate.UnpricedTokens += tokens
			estimate.UnpricedReasons = appendUnique(estimate.UnpricedReasons, "cost exceeds supported range")
		}
	}

	switch catalog.Provider {
	case ProviderAnthropic:
		addTokens(usage.InputTokens, rates.Input, "input token rate is unavailable")
		addTokens(usage.CachedInputTokens, rates.CachedInput, "cache-read token rate is unavailable")
		// A generic Anthropic cache-write total has no defensible TTL price.
		if usage.CacheWriteInputTokens > 0 {
			estimate.UnpricedTokens += usage.CacheWriteInputTokens
			estimate.UnpricedReasons = appendUnique(estimate.UnpricedReasons, "cache-write TTL is unknown")
		}
		addTokens(usage.CacheWrite5mInputTokens, rates.CacheWrite5m, "5-minute cache-write rate is unavailable")
		addTokens(usage.CacheWrite1hInputTokens, rates.CacheWrite1h, "1-hour cache-write rate is unavailable")
		addTokens(usage.OutputTokens, rates.Output, "output token rate is unavailable")
	case ProviderOpenAI:
		if usage.CacheWrite5mInputTokens > 0 || usage.CacheWrite1hInputTokens > 0 {
			unpriced := usage.CacheWrite5mInputTokens + usage.CacheWrite1hInputTokens
			estimate.UnpricedTokens += unpriced
			estimate.UnpricedReasons = appendUnique(estimate.UnpricedReasons, "provider-specific cache TTL buckets cannot use the OpenAI cache-write rate")
		}
		uncached := usage.InputTokens - usage.CachedInputTokens - usage.CacheWriteInputTokens
		if uncached < 0 {
			return unknownEstimate(estimate, usage, "cached and cache-write input exceed total input")
		}
		addTokens(uncached, rates.Input, "uncached input token rate is unavailable")
		addTokens(usage.CachedInputTokens, rates.CachedInput, "cached input token rate is unavailable")
		addTokens(usage.CacheWriteInputTokens, rates.CacheWrite, "cache-write input token rate is unavailable")
		addTokens(usage.OutputTokens, rates.Output, "output token rate is unavailable")
	default:
		return unknownEstimate(estimate, usage, "unsupported pricing provider "+catalog.Provider)
	}

	addTool := func(units int64, key string) {
		if units == 0 {
			return
		}
		price, ok := model.ToolCharges[key]
		if !ok {
			estimate.UnpricedToolUnits += units
			estimate.UnpricedReasons = appendUnique(estimate.UnpricedReasons, key+" unit price is unavailable")
			return
		}
		cost, ok := multiplyUnits(price.MicrosPerUnit, units)
		if !ok {
			estimate.UnpricedToolUnits += units
			estimate.UnpricedReasons = appendUnique(estimate.UnpricedReasons, key+" cost exceeds supported range")
			return
		}
		if next, ok := addUSD(total, cost); ok {
			total = next
			estimate.PricedToolUnits += units
		} else {
			estimate.UnpricedToolUnits += units
			estimate.UnpricedReasons = appendUnique(estimate.UnpricedReasons, key+" cost exceeds supported range")
		}
	}
	addTool(usage.WebSearchRequests, "web_search")
	addTool(usage.WebFetchRequests, "web_fetch")
	if usage.CodeExecutionRequests > 0 {
		if catalog.Provider == ProviderAnthropic &&
			(usage.WebSearchRequests > 0 || usage.WebFetchRequests > 0) {
			// Anthropic documents server-side code execution as free when used
			// with web search or web fetch in the same request.
			estimate.PricedToolUnits += usage.CodeExecutionRequests
		} else {
			estimate.UnpricedToolUnits += usage.CodeExecutionRequests
			estimate.UnpricedReasons = appendUnique(estimate.UnpricedReasons,
				"code-execution container duration and organization allowance are unavailable")
		}
	}
	if usage.UnclassifiedServerToolUnits > 0 {
		estimate.UnpricedToolUnits += usage.UnclassifiedServerToolUnits
		estimate.UnpricedReasons = appendUnique(estimate.UnpricedReasons,
			"unrecognized server-tool billing units are present")
	}

	priced := estimate.PricedTokens + estimate.PricedToolUnits
	unpriced := estimate.UnpricedTokens + estimate.UnpricedToolUnits
	if priced+unpriced == 0 {
		// Exact identity plus a valid card proves a genuine zero-cost request.
		estimate.Coverage = 1
		estimate.APIEquivalentUSD = usdPtr(0)
	} else {
		estimate.Coverage = float64(priced) / float64(priced+unpriced)
		if priced > 0 {
			estimate.APIEquivalentUSD = usdPtr(total)
		}
	}

	if unpriced > 0 {
		estimate.UnpricedEvents = 1
		if estimate.APIEquivalentUSD == nil {
			estimate.Status = CostUnknown
		} else {
			estimate.Status = CostPartial
		}
		return estimate
	}

	switch catalog.FreshnessAt(now) {
	case FreshnessStale:
		estimate.Status = CostStale
	default:
		if identity.BillingRoute == "chatgpt_subscription" || identity.BillingRoute == "subscription" {
			estimate.Status = CostIncluded
		} else {
			estimate.Status = CostEstimated
		}
	}
	if providerInferred {
		estimate.Status = CostPartial
		estimate.UnpricedEvents = 1
		estimate.UnpricedReasons = appendUnique(estimate.UnpricedReasons,
			"execution provider inferred from exact model; billing route is unconfirmed")
	}
	anthropicPriority := catalog.Provider == ProviderAnthropic && strings.EqualFold(strings.TrimSpace(identity.ServiceTier), "priority")
	if anthropicPriority {
		estimate.Status = CostPartial
		estimate.UnpricedEvents++
		estimate.UnpricedReasons = appendUnique(estimate.UnpricedReasons,
			"Anthropic priority capacity uses contract pricing; amount is standard API spot equivalent")
	}
	if identity.BillingRoute == "api" && !providerInferred && !anthropicPriority && estimate.APIEquivalentUSD != nil {
		value := *estimate.APIEquivalentUSD
		estimate.EstimatedBilledUSD = &value
	}
	return estimate
}

func selectRates(provider string, model ModelPrice, identity Identity, usage Usage) (RateCard, Multiplier, string) {
	rates := model.Base
	contextBand := false
	for _, band := range model.ContextBands {
		if usage.InputTokens > band.MinInputTokensExclusive {
			rates = band.Rates
			contextBand = true
		}
	}

	variant := ""
	if speed := strings.ToLower(strings.TrimSpace(identity.Speed)); speed != "" && speed != "standard" && speed != "default" {
		variant = "speed=" + speed
	}
	if tier := strings.ToLower(strings.TrimSpace(identity.ServiceTier)); tier != "" && tier != "standard" && tier != "default" && tier != "auto" {
		// Anthropic response usage reports "priority" for a capacity commitment,
		// whose negotiated dollars are not a public token-rate variant. Standard
		// spot rates remain a valid API-equivalent comparator. standard_only is a
		// request policy rather than the assigned response tier.
		if !(provider == ProviderAnthropic && (tier == "priority" || tier == "standard_only")) {
			tierVariant := "service_tier=" + tier
			if variant != "" {
				// Duplicate evidence for the same OpenAI fast route is safe; all other
				// combinations need an explicit compound card.
				if !(variant == "speed=fast" && (tierVariant == "service_tier=fast" || tierVariant == "service_tier=priority")) {
					return RateCard{}, Multiplier{}, "combined speed and service tier price is unavailable"
				}
				variant = tierVariant
			} else {
				variant = tierVariant
			}
		}
	}
	if variant != "" {
		price, ok := model.Variants[variant]
		if !ok {
			return RateCard{}, Multiplier{}, "price variant " + variant + " is unavailable"
		}
		rates = price.Rates
		if contextBand {
			matched := false
			for _, band := range price.ContextBands {
				if usage.InputTokens > band.MinInputTokensExclusive {
					rates = band.Rates
					matched = true
				}
			}
			if !matched {
				return RateCard{}, Multiplier{}, "long-context price for " + variant + " is unavailable"
			}
		}
	}

	multiplier := Multiplier{Numerator: 1, Denominator: 1}
	geo := strings.ToLower(strings.TrimSpace(identity.InferenceGeo))
	if geo != "" && geo != "global" && geo != "default" {
		value, ok := model.Multipliers["inference_geo="+geo]
		if !ok {
			return RateCard{}, Multiplier{}, "inference geography price is unavailable"
		}
		multiplier = value
	}
	return rates, multiplier, ""
}

func unknownEstimate(estimate Estimate, usage Usage, reason string) Estimate {
	units := usageBillableUnits(usage)
	estimate.UnpricedTokens = units.tokens
	estimate.UnpricedToolUnits = units.tools
	estimate.UnpricedEvents = 1
	estimate.UnpricedReasons = appendUnique(estimate.UnpricedReasons, reason)
	estimate.Coverage = 0
	estimate.Status = CostUnknown
	return estimate
}

type billableUnits struct{ tokens, tools int64 }

func usageBillableUnits(usage Usage) billableUnits {
	tokens := usage.InputTokens + usage.OutputTokens + usage.CacheWrite5mInputTokens + usage.CacheWrite1hInputTokens
	// InputTokens includes OpenAI cache buckets but excludes Anthropic cache
	// reads. max avoids inflating a malformed record that is rejected anyway.
	if usage.CachedInputTokens > usage.InputTokens {
		tokens += usage.CachedInputTokens
	}
	if usage.CacheWriteInputTokens > usage.InputTokens {
		tokens += usage.CacheWriteInputTokens
	}
	return billableUnits{tokens: tokens, tools: usage.WebSearchRequests + usage.WebFetchRequests +
		usage.CodeExecutionRequests + usage.UnclassifiedServerToolUnits}
}

func invalidUsage(u Usage) bool {
	return u.InputTokens < 0 || u.CachedInputTokens < 0 || u.CacheWriteInputTokens < 0 ||
		u.CacheWrite5mInputTokens < 0 || u.CacheWrite1hInputTokens < 0 || u.OutputTokens < 0 ||
		u.ReasoningOutputTokens < 0 || u.TotalTokens < 0 || u.ModelContextWindow < 0 ||
		u.WebSearchRequests < 0 || u.WebFetchRequests < 0 || u.CodeExecutionRequests < 0 ||
		u.UnclassifiedServerToolUnits < 0
}

func chargeTokens(tokens int64, rate USD) (USD, error) {
	if tokens < 0 || rate < 0 {
		return 0, errors.New("negative tokens or rate")
	}
	product := new(big.Int).Mul(big.NewInt(tokens), big.NewInt(int64(rate)))
	product.Add(product, big.NewInt(500_000))
	product.Quo(product, big.NewInt(1_000_000))
	if !product.IsInt64() {
		return 0, errors.New("token charge overflows micro-USD")
	}
	return USD(product.Int64()), nil
}

func multiplyUSD(value USD, multiplier Multiplier) USD {
	result, err := applyMultiplier(value, multiplier)
	if err != nil {
		panic(err)
	}
	return result
}

func applyMultiplier(value USD, multiplier Multiplier) (USD, error) {
	if value < 0 || multiplier.Numerator < 0 || multiplier.Denominator <= 0 {
		return 0, errors.New("invalid monetary multiplier")
	}
	product := new(big.Int).Mul(big.NewInt(int64(value)), big.NewInt(multiplier.Numerator))
	product.Add(product, big.NewInt(multiplier.Denominator/2))
	product.Quo(product, big.NewInt(multiplier.Denominator))
	if !product.IsInt64() {
		return 0, errors.New("multiplied cost overflows micro-USD")
	}
	return USD(product.Int64()), nil
}

func multiplyUnits(unitPrice USD, units int64) (USD, bool) {
	if unitPrice < 0 || units < 0 {
		return 0, false
	}
	product := new(big.Int).Mul(big.NewInt(int64(unitPrice)), big.NewInt(units))
	if !product.IsInt64() {
		return 0, false
	}
	return USD(product.Int64()), true
}

func addUSD(a, b USD) (USD, bool) {
	sum := new(big.Int).Add(big.NewInt(int64(a)), big.NewInt(int64(b)))
	if !sum.IsInt64() {
		return 0, false
	}
	return USD(sum.Int64()), true
}

func addUSDPtr(total *USD, value *USD) *USD {
	if value == nil {
		return total
	}
	if total == nil {
		copy := *value
		return &copy
	}
	sum, ok := addUSD(*total, *value)
	if !ok {
		return nil
	}
	return &sum
}

func addCreditsPtr(total *Credits, value *Credits) *Credits {
	if value == nil {
		return total
	}
	if total == nil {
		copy := *value
		return &copy
	}
	sum := new(big.Int).Add(big.NewInt(int64(*total)), big.NewInt(int64(*value)))
	if !sum.IsInt64() {
		return nil
	}
	result := Credits(sum.Int64())
	return &result
}

// MergeEstimates sums like-with-like concepts and carries conservative status,
// coverage, and provenance across sessions/providers.
func MergeEstimates(estimates ...Estimate) Estimate {
	if len(estimates) == 0 {
		return Estimate{Status: CostUnknown}
	}
	var out Estimate
	allIncluded := true
	anyStale, anyPartial, anyUnknown, anyCompleteAmount := false, false, false, false
	for i, estimate := range estimates {
		sources := estimate.PricingSources
		if len(sources) == 0 && estimate.PricingSource != "" && estimate.PricingSource != "mixed" {
			sources = []string{estimate.PricingSource}
		}
		for _, source := range sources {
			out.PricingSources = appendUnique(out.PricingSources, source)
		}
		versions := estimate.PricingVersions
		if len(versions) == 0 && estimate.PricingVersion != "" && estimate.PricingVersion != "mixed" {
			versions = []string{estimate.PricingVersion}
		}
		for _, version := range versions {
			out.PricingVersions = appendUnique(out.PricingVersions, version)
		}
		out.APIEquivalentUSD = addUSDPtr(out.APIEquivalentUSD, estimate.APIEquivalentUSD)
		out.VendorEstimatedUSD = addUSDPtr(out.VendorEstimatedUSD, estimate.VendorEstimatedUSD)
		out.PlanCredits = addCreditsPtr(out.PlanCredits, estimate.PlanCredits)
		out.EstimatedBilledUSD = addUSDPtr(out.EstimatedBilledUSD, estimate.EstimatedBilledUSD)
		out.PricedTokens += estimate.PricedTokens
		out.UnpricedTokens += estimate.UnpricedTokens
		out.PricedToolUnits += estimate.PricedToolUnits
		out.UnpricedToolUnits += estimate.UnpricedToolUnits
		out.UnpricedEvents += estimate.UnpricedEvents
		for _, reason := range estimate.UnpricedReasons {
			out.UnpricedReasons = appendUnique(out.UnpricedReasons, reason)
		}
		anyCompleteAmount = anyCompleteAmount || estimate.APIEquivalentUSD != nil || estimate.VendorEstimatedUSD != nil ||
			estimate.PlanCredits != nil || estimate.EstimatedBilledUSD != nil
		switch estimate.Status {
		case CostIncluded:
		case CostStale:
			allIncluded = false
			anyStale = true
		case CostPartial:
			allIncluded = false
			anyPartial = true
		case CostUnknown:
			allIncluded = false
			anyUnknown = true
		default:
			allIncluded = false
		}
		if i == 0 {
			out.PricingProvider = estimate.PricingProvider
			out.PricingSource = estimate.PricingSource
			out.PricingVersion = estimate.PricingVersion
			out.PricingKind = estimate.PricingKind
			out.PricingRetrievedAt = cloneTimePtr(estimate.PricingRetrievedAt)
			out.PricingEffectiveAt = cloneTimePtr(estimate.PricingEffectiveAt)
			out.MixedPricing = estimate.MixedPricing
		} else {
			var conflict bool
			out.PricingProvider, conflict = mergeProvenanceValue(out.PricingProvider, estimate.PricingProvider)
			out.MixedPricing = out.MixedPricing || conflict
			out.PricingSource, conflict = mergeProvenanceValue(out.PricingSource, estimate.PricingSource)
			out.MixedPricing = out.MixedPricing || conflict
			out.PricingVersion, conflict = mergeProvenanceValue(out.PricingVersion, estimate.PricingVersion)
			out.MixedPricing = out.MixedPricing || conflict
			out.PricingKind, conflict = mergeProvenanceValue(out.PricingKind, estimate.PricingKind)
			out.MixedPricing = out.MixedPricing || conflict
			out.PricingRetrievedAt = oldestTime(out.PricingRetrievedAt, estimate.PricingRetrievedAt)
			out.PricingEffectiveAt = oldestTime(out.PricingEffectiveAt, estimate.PricingEffectiveAt)
			out.MixedPricing = out.MixedPricing || estimate.MixedPricing
		}
	}
	denominator := out.PricedTokens + out.UnpricedTokens + out.PricedToolUnits + out.UnpricedToolUnits
	if denominator == 0 {
		out.Coverage = 1
	} else {
		out.Coverage = float64(out.PricedTokens+out.PricedToolUnits) / float64(denominator)
	}
	switch {
	case (anyPartial || anyUnknown) && anyCompleteAmount:
		out.Status = CostPartial
	case anyUnknown:
		out.Status = CostUnknown
	case anyPartial:
		out.Status = CostPartial
	case anyStale:
		out.Status = CostStale
	case allIncluded:
		out.Status = CostIncluded
	default:
		out.Status = CostEstimated
	}
	sort.Strings(out.UnpricedReasons)
	sort.Strings(out.PricingSources)
	sort.Strings(out.PricingVersions)
	return out
}

func appendUnique(values []string, value string) []string {
	if value == "" {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func cloneTimePtr(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func oldestTime(a, b *time.Time) *time.Time {
	if a == nil {
		return cloneTimePtr(b)
	}
	if b == nil || a.Before(*b) {
		return cloneTimePtr(a)
	}
	return cloneTimePtr(b)
}

func mergeProvenanceValue(current, next string) (string, bool) {
	if next == "" {
		return current, false
	}
	if current == "" {
		return next, false
	}
	if current == next {
		return current, false
	}
	return "mixed", true
}

func (e Estimate) Summary() string {
	amount := "unknown"
	if e.APIEquivalentUSD != nil {
		amount = "$" + e.APIEquivalentUSD.String()
	}
	return fmt.Sprintf("%s (%s, %.0f%% coverage)", amount, e.Status, e.Coverage*100)
}
