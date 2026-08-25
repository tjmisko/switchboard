package history

import (
	"time"

	"github.com/tjmisko/switchboard/internal/pricing"
)

// priceBook performs no network I/O. The daemon's background price refresher
// atomically updates this cache; timeline folds always get either that validated
// last-known-good catalog or the bundled catalog, which starts stale and fails
// closed after seven days.
func priceBook(now time.Time) pricing.CatalogSet {
	return pricing.CachedOrBootstrapCatalogs("", now)
}

// EstimateEvent derives explicit cost semantics for one usage_sample. Raw usage
// is always repriced against the selected catalog; vendor-returned USD/credits
// remain separate and are never overwritten by token arithmetic.
func EstimateEvent(ev Event, catalogs pricing.CatalogSet, now time.Time) CostEstimate {
	estimate := pricing.EstimateRequest(catalogs, ev.PricingIdentity(), ev.CanonicalUsage(), now)
	if ev.Cost == nil {
		return applyUsageCoverage(estimate, ev.UsageCoverage)
	}
	reported := *ev.Cost
	if reported.VendorEstimatedUSD != nil {
		value := *reported.VendorEstimatedUSD
		estimate.VendorEstimatedUSD = &value
	}
	if reported.PlanCredits != nil {
		value := *reported.PlanCredits
		estimate.PlanCredits = &value
	}
	if reported.EstimatedBilledUSD != nil {
		value := *reported.EstimatedBilledUSD
		estimate.EstimatedBilledUSD = &value
	}
	// A provider-specific override or vendor-carried API equivalent remains a
	// useful fallback when the public first-party catalog cannot price the route.
	if estimate.APIEquivalentUSD == nil && reported.APIEquivalentUSD != nil {
		value := *reported.APIEquivalentUSD
		estimate.APIEquivalentUSD = &value
		estimate.PricingProvider = reported.PricingProvider
		estimate.PricingSource = reported.PricingSource
		estimate.PricingSources = append([]string(nil), reported.PricingSources...)
		estimate.PricingRetrievedAt = reported.PricingRetrievedAt
		estimate.PricingEffectiveAt = reported.PricingEffectiveAt
		estimate.PricingVersion = reported.PricingVersion
		estimate.PricingVersions = append([]string(nil), reported.PricingVersions...)
		estimate.PricingKind = reported.PricingKind
	}
	if estimate.Status == pricing.CostUnknown &&
		(estimate.VendorEstimatedUSD != nil || estimate.PlanCredits != nil || estimate.EstimatedBilledUSD != nil) {
		switch reported.Status {
		case pricing.CostEstimated, pricing.CostPartial, pricing.CostStale:
			estimate.Status = reported.Status
		default:
			estimate.Status = pricing.CostEstimated
		}
	}
	if reported.Status == pricing.CostIncluded && ev.BillingRoute == "chatgpt_subscription" &&
		estimate.EstimatedBilledUSD == nil {
		estimate.Status = pricing.CostIncluded
	}
	return applyUsageCoverage(estimate, ev.UsageCoverage)
}

// EstimateCoverageGap turns a durable collector cutover/gap marker into an
// explicit unknown component. This prevents a priced post-cutover subtotal from
// looking complete merely because the amount of missing historical usage is
// unknowable.
func EstimateCoverageGap(ev Event) CostEstimate {
	reason := "usage collector coverage is incomplete"
	if ev.UsageCoverage != "" {
		reason += ": " + ev.UsageCoverage
	}
	return CostEstimate{
		Status: pricing.CostUnknown, Coverage: 0, UnpricedEvents: 1,
		UnpricedReasons: []string{reason}, PricingKind: "coverage_gap",
	}
}

// estimateObservedUsage prices only the usage present on an event. Timeline
// folds merge the session-scoped collector gap separately and exactly once,
// even when every subsequent snapshot repeats the same coverage marker.
func estimateObservedUsage(ev Event, catalogs pricing.CatalogSet, now time.Time) CostEstimate {
	ev.UsageCoverage = ""
	return EstimateEvent(ev, catalogs, now)
}

func applyUsageCoverage(estimate CostEstimate, coverage string) CostEstimate {
	if coverage == "" || coverage == "complete" || coverage == "full" {
		return estimate
	}
	reason := "usage collector coverage is incomplete: " + coverage
	seen := false
	for _, existing := range estimate.UnpricedReasons {
		if existing == reason {
			seen = true
			break
		}
	}
	if !seen {
		estimate.UnpricedReasons = append(estimate.UnpricedReasons, reason)
	}
	estimate.UnpricedEvents++
	if estimate.APIEquivalentUSD != nil || estimate.VendorEstimatedUSD != nil ||
		estimate.PlanCredits != nil || estimate.EstimatedBilledUSD != nil {
		estimate.Status = pricing.CostPartial
	} else {
		estimate.Status = pricing.CostUnknown
	}
	return estimate
}

func mergeCost(existing *CostEstimate, next CostEstimate) *CostEstimate {
	if existing == nil {
		copy := next
		return &copy
	}
	merged := pricing.MergeEstimates(*existing, next)
	return &merged
}

func legacyCostAlias(cost *CostEstimate) *pricing.USD {
	if cost == nil || cost.APIEquivalentUSD == nil {
		return nil
	}
	value := *cost.APIEquivalentUSD
	return &value
}
