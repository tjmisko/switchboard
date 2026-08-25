// Package pricing owns provider-independent price catalogs and cost estimates.
//
// It deliberately keeps money as integer micro-units until JSON encoding. This
// avoids binary floating-point drift while retaining the dashboard's historical
// JSON-number contract at the process boundary.
package pricing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	ProviderAnthropic = "anthropic"
	ProviderOpenAI    = "openai"

	CatalogSchemaVersion = 1
)

// USD stores one millionth of a US dollar. It marshals as a dollar-denominated
// JSON number so existing consumers can continue to decode cost fields as a
// float while all in-process arithmetic remains decimal-safe.
type USD int64

func USDFromMicros(micros int64) USD { return USD(micros) }

func USDFromDollars(dollars int64) USD { return USD(dollars * 1_000_000) }

func (u USD) Micros() int64 { return int64(u) }

func (u USD) Float64() float64 { return float64(u) / 1_000_000 }

func (u USD) String() string { return formatMicros(int64(u)) }

func (u USD) MarshalJSON() ([]byte, error) { return []byte(u.String()), nil }

func (u *USD) UnmarshalJSON(data []byte) error {
	if u == nil {
		return errors.New("pricing.USD: UnmarshalJSON on nil receiver")
	}
	micros, err := parseMicros(string(data))
	if err != nil {
		return fmt.Errorf("pricing.USD: %w", err)
	}
	*u = USD(micros)
	return nil
}

// Credits stores one millionth of a provider plan credit. Credits are not
// dollars and intentionally use a distinct type even though their JSON shape is
// also a decimal number.
type Credits int64

func CreditsFromMicros(micros int64) Credits { return Credits(micros) }

func (c Credits) Micros() int64 { return int64(c) }

func (c Credits) Float64() float64 { return float64(c) / 1_000_000 }

func (c Credits) String() string { return formatMicros(int64(c)) }

func (c Credits) MarshalJSON() ([]byte, error) { return []byte(c.String()), nil }

func (c *Credits) UnmarshalJSON(data []byte) error {
	if c == nil {
		return errors.New("pricing.Credits: UnmarshalJSON on nil receiver")
	}
	micros, err := parseMicros(string(data))
	if err != nil {
		return fmt.Errorf("pricing.Credits: %w", err)
	}
	*c = Credits(micros)
	return nil
}

func formatMicros(micros int64) string {
	negative := micros < 0
	if negative {
		micros = -micros
	}
	whole, fraction := micros/1_000_000, micros%1_000_000
	value := strconv.FormatInt(whole, 10)
	if fraction != 0 {
		frac := strings.TrimRight(fmt.Sprintf("%06d", fraction), "0")
		value += "." + frac
	}
	if negative {
		return "-" + value
	}
	return value
}

func parseMicros(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return 0, errors.New("expected a JSON number")
	}
	r, ok := new(big.Rat).SetString(raw)
	if !ok {
		return 0, fmt.Errorf("invalid decimal %q", raw)
	}
	r.Mul(r, big.NewRat(1_000_000, 1))
	// Round half away from zero to the nearest micro-unit.
	q := new(big.Int).Quo(r.Num(), r.Denom())
	rem := new(big.Int).Rem(r.Num(), r.Denom())
	absRem := new(big.Int).Abs(rem)
	twice := new(big.Int).Lsh(absRem, 1)
	if twice.Cmp(r.Denom()) >= 0 {
		if r.Sign() < 0 {
			q.Sub(q, big.NewInt(1))
		} else {
			q.Add(q, big.NewInt(1))
		}
	}
	if !q.IsInt64() {
		return 0, fmt.Errorf("decimal %q exceeds micro-unit range", raw)
	}
	return q.Int64(), nil
}

// RateCard contains dollar rates per one million tokens. Nil means that a
// bucket is not priced by this card; zero is a real, explicitly free rate.
type RateCard struct {
	Input        *USD `json:"input_per_mtok_usd,omitempty"`
	CachedInput  *USD `json:"cached_input_per_mtok_usd,omitempty"`
	CacheWrite   *USD `json:"cache_write_per_mtok_usd,omitempty"`
	CacheWrite5m *USD `json:"cache_write_5m_per_mtok_usd,omitempty"`
	CacheWrite1h *USD `json:"cache_write_1h_per_mtok_usd,omitempty"`
	Output       *USD `json:"output_per_mtok_usd,omitempty"`
}

// ContextBand replaces rates for a single request whose input crosses the
// threshold. Thresholds are exclusive because vendor wording is typically
// "more than N input tokens".
type ContextBand struct {
	MinInputTokensExclusive int64    `json:"min_input_tokens_exclusive"`
	Rates                   RateCard `json:"rates"`
}

// VariantPrice is a speed/service-tier card with its own context bands. Fast
// long-context rates cannot be derived from either the standard long-context
// card or the fast short-context card after the fact.
type VariantPrice struct {
	Rates        RateCard      `json:"rates"`
	ContextBands []ContextBand `json:"context_bands,omitempty"`
}

// Multiplier is a decimal-safe rational modifier, for example 11/10 for
// Anthropic US-only inference.
type Multiplier struct {
	Numerator   int64 `json:"numerator"`
	Denominator int64 `json:"denominator"`
}

// UnitPrice is a non-token metered charge such as one web search request.
type UnitPrice struct {
	Unit          string `json:"unit"`
	MicrosPerUnit USD    `json:"usd_per_unit"`
}

// ModelPrice is one exact model and its explicitly enumerated aliases. Variants
// use evidence keys such as "speed=fast" or "service_tier=ultrafast"; arbitrary
// family substring matching is never performed.
type ModelPrice struct {
	ExactModelID string                  `json:"exact_model_id"`
	Aliases      []string                `json:"aliases,omitempty"`
	Base         RateCard                `json:"base"`
	ContextBands []ContextBand           `json:"context_bands,omitempty"`
	Variants     map[string]VariantPrice `json:"variants,omitempty"`
	Multipliers  map[string]Multiplier   `json:"multipliers,omitempty"`
	ToolCharges  map[string]UnitPrice    `json:"tool_charges,omitempty"`
}

// HTTPValidator retains only public cache validators for conditional refresh.
// It never carries authorization or account metadata.
type HTTPValidator struct {
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
}

// Catalog is a validated snapshot of one execution provider's public prices.
type Catalog struct {
	SchemaVersion int                      `json:"schema_version"`
	Provider      string                   `json:"provider"`
	SourceURL     string                   `json:"source_url"`
	SourceURLs    []string                 `json:"source_urls,omitempty"`
	RetrievedAt   time.Time                `json:"retrieved_at"`
	EffectiveAt   *time.Time               `json:"effective_at,omitempty"`
	VersionHash   string                   `json:"version_hash"`
	Bundled       bool                     `json:"bundled,omitempty"`
	Validators    map[string]HTTPValidator `json:"validators,omitempty"`
	Models        map[string]ModelPrice    `json:"models"`
}

// Validate rejects incomplete or ambiguous catalogs. A source redesign must
// fail here instead of replacing a good catalog with zero or partial rates.
func (c Catalog) Validate() error {
	if c.SchemaVersion != CatalogSchemaVersion {
		return fmt.Errorf("unsupported schema_version %d", c.SchemaVersion)
	}
	if c.Provider == "" {
		return errors.New("provider is required")
	}
	if c.SourceURL == "" {
		return errors.New("source_url is required")
	}
	if len(c.Models) == 0 {
		return errors.New("at least one model price is required")
	}
	seen := make(map[string]string)
	for key, model := range c.Models {
		if key == "" || model.ExactModelID == "" || key != model.ExactModelID {
			return fmt.Errorf("model map key %q does not match exact id %q", key, model.ExactModelID)
		}
		if err := validateRateCard(model.Base, true); err != nil {
			return fmt.Errorf("model %s base rates: %w", key, err)
		}
		identifiers := append([]string{model.ExactModelID}, model.Aliases...)
		for _, identifier := range identifiers {
			if identifier == "" {
				return fmt.Errorf("model %s has an empty alias", key)
			}
			if owner, duplicate := seen[identifier]; duplicate {
				return fmt.Errorf("model identifier %q is shared by %s and %s", identifier, owner, key)
			}
			seen[identifier] = key
		}
		lastThreshold := int64(-1)
		for _, band := range model.ContextBands {
			if band.MinInputTokensExclusive < 0 || band.MinInputTokensExclusive <= lastThreshold {
				return fmt.Errorf("model %s has unsorted/invalid context bands", key)
			}
			lastThreshold = band.MinInputTokensExclusive
			if err := validateRateCard(band.Rates, true); err != nil {
				return fmt.Errorf("model %s context band: %w", key, err)
			}
		}
		for variant, price := range model.Variants {
			if variant == "" {
				return fmt.Errorf("model %s has an empty variant key", key)
			}
			if err := validateRateCard(price.Rates, true); err != nil {
				return fmt.Errorf("model %s variant %s: %w", key, variant, err)
			}
			lastVariantThreshold := int64(-1)
			for _, band := range price.ContextBands {
				if band.MinInputTokensExclusive < 0 || band.MinInputTokensExclusive <= lastVariantThreshold {
					return fmt.Errorf("model %s variant %s has unsorted/invalid context bands", key, variant)
				}
				lastVariantThreshold = band.MinInputTokensExclusive
				if err := validateRateCard(band.Rates, true); err != nil {
					return fmt.Errorf("model %s variant %s context band: %w", key, variant, err)
				}
			}
		}
		for dimension, multiplier := range model.Multipliers {
			if dimension == "" || multiplier.Numerator < 0 || multiplier.Denominator <= 0 {
				return fmt.Errorf("model %s has invalid multiplier %q", key, dimension)
			}
		}
		for tool, price := range model.ToolCharges {
			if tool == "" || price.Unit == "" || price.MicrosPerUnit < 0 {
				return fmt.Errorf("model %s has invalid tool charge %q", key, tool)
			}
		}
	}
	return nil
}

func validateRateCard(card RateCard, requireInputOutput bool) error {
	if requireInputOutput && (card.Input == nil || card.Output == nil) {
		return errors.New("input and output rates are required")
	}
	for name, rate := range map[string]*USD{
		"input": card.Input, "cached_input": card.CachedInput, "cache_write": card.CacheWrite,
		"cache_write_5m": card.CacheWrite5m, "cache_write_1h": card.CacheWrite1h, "output": card.Output,
	} {
		if rate != nil && *rate < 0 {
			return fmt.Errorf("%s rate is negative", name)
		}
	}
	return nil
}

// Lookup performs an exact identifier or explicit-alias match.
func (c Catalog) Lookup(modelID string) (ModelPrice, bool) {
	if model, ok := c.Models[modelID]; ok {
		return model, true
	}
	for _, model := range c.Models {
		for _, alias := range model.Aliases {
			if modelID == alias {
				return model, true
			}
		}
	}
	return ModelPrice{}, false
}

// ContentHash deterministically hashes the price-bearing catalog content. It
// excludes retrieval time and HTTP validators so a 304 revalidation retains
// the same version.
func (c Catalog) ContentHash() (string, error) {
	type hashCatalog struct {
		SchemaVersion int                   `json:"schema_version"`
		Provider      string                `json:"provider"`
		SourceURL     string                `json:"source_url"`
		SourceURLs    []string              `json:"source_urls,omitempty"`
		EffectiveAt   *time.Time            `json:"effective_at,omitempty"`
		Models        map[string]ModelPrice `json:"models"`
	}
	urls := append([]string(nil), c.SourceURLs...)
	sort.Strings(urls)
	payload, err := json.Marshal(hashCatalog{
		SchemaVersion: c.SchemaVersion, Provider: c.Provider, SourceURL: c.SourceURL,
		SourceURLs: urls, EffectiveAt: c.EffectiveAt, Models: c.Models,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Freshness is the price snapshot's usability at a point in time.
type Freshness string

const (
	FreshnessCurrent  Freshness = "current"
	FreshnessStale    Freshness = "stale"
	FreshnessUnusable Freshness = "unusable"
)

// FreshnessAt uses the rollout policy from the cost audit. Bundled catalogs are
// intentionally never current and expire at the same seven-day cutoff as LKG.
func (c Catalog) FreshnessAt(now time.Time) Freshness {
	if c.RetrievedAt.IsZero() {
		return FreshnessUnusable
	}
	age := now.Sub(c.RetrievedAt)
	if age < 0 {
		age = 0
	}
	if age >= UnusableAfter {
		return FreshnessUnusable
	}
	if c.Bundled {
		return FreshnessStale
	}
	if age >= StaleAfter {
		return FreshnessStale
	}
	return FreshnessCurrent
}

// CatalogSet selects prices without conflating a client name with its execution
// provider. When provider is absent it will use an exact model match only when
// that match is unique across all catalogs.
type CatalogSet map[string]Catalog

func (set CatalogSet) Resolve(provider, modelID string) (Catalog, ModelPrice, bool) {
	if provider != "" {
		catalog, ok := set[provider]
		if !ok {
			return Catalog{}, ModelPrice{}, false
		}
		model, ok := catalog.Lookup(modelID)
		return catalog, model, ok
	}
	var foundCatalog Catalog
	var foundModel ModelPrice
	matches := 0
	for _, catalog := range set {
		if model, ok := catalog.Lookup(modelID); ok {
			foundCatalog, foundModel = catalog, model
			matches++
		}
	}
	return foundCatalog, foundModel, matches == 1
}

// Identity separates the agent client from the provider that executed and
// billed a request.
type Identity struct {
	AgentClient       string `json:"agent_client,omitempty"`
	ExecutionProvider string `json:"execution_provider,omitempty"`
	BillingRoute      string `json:"billing_route,omitempty"`
	AccountKind       string `json:"account_kind,omitempty"`
	AuthMode          string `json:"auth_mode,omitempty"`
	Model             string `json:"model,omitempty"`
	ServiceTier       string `json:"service_tier,omitempty"`
	Speed             string `json:"speed,omitempty"`
	InferenceGeo      string `json:"inference_geo,omitempty"`
	ReasoningEffort   string `json:"reasoning_effort,omitempty"`
}

// Usage is one request/turn delta. ReasoningOutputTokens is a subset of output,
// never a separately billed bucket.
type Usage struct {
	InputTokens             int64 `json:"input_tokens,omitempty"`
	CachedInputTokens       int64 `json:"cached_input_tokens,omitempty"`
	CacheWriteInputTokens   int64 `json:"cache_write_input_tokens,omitempty"`
	CacheWrite5mInputTokens int64 `json:"cache_write_5m_input_tokens,omitempty"`
	CacheWrite1hInputTokens int64 `json:"cache_write_1h_input_tokens,omitempty"`
	OutputTokens            int64 `json:"output_tokens,omitempty"`
	ReasoningOutputTokens   int64 `json:"reasoning_output_tokens,omitempty"`
	TotalTokens             int64 `json:"total_tokens,omitempty"`
	ModelContextWindow      int64 `json:"model_context_window,omitempty"`
	WebSearchRequests       int64 `json:"web_search_requests,omitempty"`
	WebFetchRequests        int64 `json:"web_fetch_requests,omitempty"`
}

func (u Usage) IsZero() bool { return u == (Usage{}) }

// VendorUsageSnapshot preserves a provider's cumulative account/thread
// estimate independently from additive request deltas. It is corroboration or
// a billed-route estimate, never another token delta to fold into usage totals.
type VendorUsageSnapshot struct {
	ThreadID              string             `json:"thread_id,omitempty"`
	EstimatedUsageCredits Credits            `json:"estimated_usage_credits"`
	EstimatedUsageUSD     *USD               `json:"estimated_usage_usd"`
	Groups                []VendorUsageGroup `json:"groups,omitempty"`
	ObservedAt            time.Time          `json:"observed_at"`
	Revision              int64              `json:"revision,omitempty"`
	Stale                 bool               `json:"stale,omitempty"`
}

// VendorUsageGroup mirrors the nullable grouping dimensions returned by a
// vendor estimate. Pointer token fields retain absent-vs-zero semantics.
type VendorUsageGroup struct {
	Model                 *string `json:"model"`
	ReasoningEffort       *string `json:"reasoning_effort"`
	Speed                 *string `json:"speed"`
	InputTokens           *int64  `json:"input_tokens"`
	CachedInputTokens     *int64  `json:"cached_input_tokens"`
	NetNewInputTokens     *int64  `json:"net_new_input_tokens"`
	OutputTokens          *int64  `json:"output_tokens"`
	TotalTokens           *int64  `json:"total_tokens"`
	EstimatedUsageCredits Credits `json:"estimated_usage_credits"`
}

type CostStatus string

const (
	CostEstimated CostStatus = "estimated"
	CostIncluded  CostStatus = "included"
	CostPartial   CostStatus = "partial"
	CostStale     CostStatus = "stale"
	CostUnknown   CostStatus = "unknown"
)

// Estimate keeps each billing concept distinct. Pointer amounts preserve the
// difference between an unknown value and a real zero.
type Estimate struct {
	APIEquivalentUSD   *USD       `json:"api_equivalent_usd"`
	VendorEstimatedUSD *USD       `json:"vendor_estimated_usd"`
	PlanCredits        *Credits   `json:"plan_credits"`
	EstimatedBilledUSD *USD       `json:"estimated_billed_usd"`
	Status             CostStatus `json:"status"`
	Coverage           float64    `json:"coverage"`

	PricedTokens      int64    `json:"priced_tokens,omitempty"`
	UnpricedTokens    int64    `json:"unpriced_tokens,omitempty"`
	PricedToolUnits   int64    `json:"priced_tool_units,omitempty"`
	UnpricedToolUnits int64    `json:"unpriced_tool_units,omitempty"`
	UnpricedEvents    int64    `json:"unpriced_events,omitempty"`
	UnpricedReasons   []string `json:"unpriced_reasons,omitempty"`

	PricingProvider    string     `json:"pricing_provider,omitempty"`
	PricingSource      string     `json:"pricing_source,omitempty"`
	PricingSources     []string   `json:"pricing_sources,omitempty"`
	PricingRetrievedAt *time.Time `json:"pricing_retrieved_at,omitempty"`
	PricingEffectiveAt *time.Time `json:"pricing_effective_at,omitempty"`
	PricingVersion     string     `json:"pricing_version,omitempty"`
	PricingVersions    []string   `json:"pricing_versions,omitempty"`
	PricingKind        string     `json:"pricing_kind,omitempty"`
	MixedPricing       bool       `json:"mixed_pricing_versions,omitempty"`
}

func usdPtr(value USD) *USD { return &value }

func creditsPtr(value Credits) *Credits { return &value }
