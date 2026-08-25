package pricing

import "time"

const (
	AnthropicPricingURL = "https://platform.claude.com/docs/en/about-claude/pricing"
	OpenAIPricingURL    = "https://developers.openai.com/api/docs/pricing"
)

// BootstrapCatalogs provides a fail-closed offline starting point from the
// official publications listed in SourceURL. Bundled is always true, so the
// estimator labels these rates stale (and, after seven days, unusable) until
// Manager successfully refreshes them.
func BootstrapCatalogs() CatalogSet {
	effective := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	set := CatalogSet{
		ProviderAnthropic: bootstrapAnthropic(effective),
		ProviderOpenAI:    bootstrapOpenAI(effective),
	}
	for provider, catalog := range set {
		hash, err := catalog.ContentHash()
		if err != nil {
			panic(err)
		}
		catalog.VersionHash = hash
		set[provider] = catalog
	}
	return set
}

func bootstrapAnthropic(effective time.Time) Catalog {
	webSearch := UnitPrice{Unit: "request", MicrosPerUnit: USDFromMicros(10_000)} // $10 / 1,000 searches
	webFetch := UnitPrice{Unit: "request", MicrosPerUnit: 0}
	tools := func() map[string]UnitPrice {
		return map[string]UnitPrice{"web_search": webSearch, "web_fetch": webFetch}
	}
	geoUS := map[string]Multiplier{"inference_geo=us": {Numerator: 11, Denominator: 10}}
	model := func(id string, aliases []string, in, cached, write5m, write1h, out string) ModelPrice {
		return ModelPrice{
			ExactModelID: id,
			Aliases:      aliases,
			Base: RateCard{
				Input: rate(in), CachedInput: rate(cached), CacheWrite5m: rate(write5m),
				CacheWrite1h: rate(write1h), Output: rate(out),
			},
			ToolCharges: tools(),
		}
	}

	models := map[string]ModelPrice{}
	add := func(p ModelPrice) { models[p.ExactModelID] = p }
	add(model("claude-fable-5", []string{"claude-fable-5[1m]"}, "10", "1", "12.5", "20", "50"))
	add(model("claude-mythos-5", []string{"claude-mythos-5[1m]"}, "10", "1", "12.5", "20", "50"))
	add(model("claude-opus-5", []string{"claude-opus-5[1m]"}, "5", "0.5", "6.25", "10", "25"))
	add(model("claude-opus-4-8", []string{"claude-opus-4-8[1m]"}, "5", "0.5", "6.25", "10", "25"))
	add(model("claude-opus-4-7", []string{"claude-opus-4-7[1m]"}, "5", "0.5", "6.25", "10", "25"))
	add(model("claude-opus-4-6", []string{"claude-opus-4-6[1m]"}, "5", "0.5", "6.25", "10", "25"))
	add(model("claude-opus-4-5", []string{"claude-opus-4-5-20251101"}, "5", "0.5", "6.25", "10", "25"))
	add(model("claude-opus-4-1", []string{"claude-opus-4-1-20250805"}, "15", "1.5", "18.75", "30", "75"))
	add(model("claude-opus-4", []string{"claude-opus-4-20250514"}, "15", "1.5", "18.75", "30", "75"))
	add(model("claude-sonnet-5", []string{"claude-sonnet-5[1m]"}, "2", "0.2", "2.5", "4", "10"))
	add(model("claude-sonnet-4-6", []string{"claude-sonnet-4-6[1m]"}, "3", "0.3", "3.75", "6", "15"))
	add(model("claude-sonnet-4-5-20250929", []string{"claude-sonnet-4-5"}, "3", "0.3", "3.75", "6", "15"))
	add(model("claude-sonnet-4-20250514", []string{"claude-sonnet-4"}, "3", "0.3", "3.75", "6", "15"))
	add(model("claude-haiku-4-5-20251001", []string{"claude-haiku-4-5"}, "1", "0.1", "1.25", "2", "5"))
	add(model("claude-3-5-haiku-20241022", []string{"claude-3-5-haiku-latest", "claude-haiku-3-5"}, "0.8", "0.08", "1", "1.6", "4"))

	for id, p := range models {
		if anthropicSupportsUSGeo(id) {
			p.Multipliers = cloneMultipliers(geoUS)
		}
		if id == "claude-opus-5" || id == "claude-opus-4-8" {
			p.Variants = map[string]VariantPrice{
				"speed=fast": {Rates: RateCard{
					Input: rate("10"), CachedInput: rate("1"), CacheWrite5m: rate("12.5"),
					CacheWrite1h: rate("20"), Output: rate("50"),
				}},
			}
		}
		models[id] = p
	}

	return Catalog{
		SchemaVersion: CatalogSchemaVersion,
		Provider:      ProviderAnthropic,
		SourceURL:     AnthropicPricingURL,
		SourceURLs:    []string{AnthropicPricingURL},
		RetrievedAt:   effective,
		EffectiveAt:   timePtr(effective),
		Bundled:       true,
		Models:        models,
	}
}

func bootstrapOpenAI(effective time.Time) Catalog {
	model := func(id string, aliases []string, in, cached, out string) ModelPrice {
		input := mustUSD(in)
		cachedInput := mustUSD(cached)
		output := mustUSD(out)
		write := multiplyUSD(input, Multiplier{Numerator: 5, Denominator: 4})
		longRates := RateCard{
			Input:       usdCopy(multiplyUSD(input, Multiplier{Numerator: 2, Denominator: 1})),
			CachedInput: usdCopy(multiplyUSD(cachedInput, Multiplier{Numerator: 2, Denominator: 1})),
			CacheWrite:  usdCopy(multiplyUSD(write, Multiplier{Numerator: 2, Denominator: 1})),
			Output:      usdCopy(multiplyUSD(output, Multiplier{Numerator: 3, Denominator: 2})),
		}
		return ModelPrice{
			ExactModelID: id,
			Aliases:      aliases,
			Base: RateCard{
				Input: usdCopy(input), CachedInput: usdCopy(cachedInput),
				CacheWrite: usdCopy(write), Output: usdCopy(output),
			},
			ContextBands: []ContextBand{{MinInputTokensExclusive: 272_000, Rates: longRates}},
		}
	}

	models := map[string]ModelPrice{}
	add := func(p ModelPrice) { models[p.ExactModelID] = p }
	add(model("gpt-5.6-sol", []string{"gpt-5.6"}, "4", "0.4", "20"))
	add(model("gpt-5.6-terra", nil, "2", "0.2", "12"))
	add(model("gpt-5.6-luna", nil, "0.2", "0.02", "1.2"))
	for id, price := range models {
		price.Multipliers = map[string]Multiplier{
			"inference_geo=regional":       {Numerator: 11, Denominator: 10},
			"inference_geo=data_residency": {Numerator: 11, Denominator: 10},
		}
		models[id] = price
	}

	sol := models["gpt-5.6-sol"]
	fast := RateCard{
		Input: rate("8"), CachedInput: rate("0.8"), CacheWrite: rate("10"), Output: rate("40"),
	}
	fastLong := RateCard{
		Input: rate("16"), CachedInput: rate("1.6"), CacheWrite: rate("20"), Output: rate("60"),
	}
	fastPrice := VariantPrice{Rates: fast, ContextBands: []ContextBand{{MinInputTokensExclusive: 272_000, Rates: fastLong}}}
	sol.Variants = map[string]VariantPrice{
		"speed=fast":            fastPrice,
		"service_tier=fast":     fastPrice,
		"service_tier=priority": fastPrice,
	}
	models[sol.ExactModelID] = sol

	return Catalog{
		SchemaVersion: CatalogSchemaVersion,
		Provider:      ProviderOpenAI,
		SourceURL:     OpenAIPricingURL,
		SourceURLs: []string{
			"https://developers.openai.com/api/docs/models/gpt-5.6-sol",
			"https://developers.openai.com/api/docs/models/gpt-5.6-terra",
			"https://developers.openai.com/api/docs/models/gpt-5.6-luna",
		},
		RetrievedAt: effective,
		EffectiveAt: timePtr(effective),
		Bundled:     true,
		Models:      models,
	}
}

func anthropicSupportsUSGeo(modelID string) bool {
	switch modelID {
	case "claude-fable-5", "claude-mythos-5", "claude-opus-5", "claude-opus-4-8",
		"claude-opus-4-7", "claude-opus-4-6", "claude-sonnet-5", "claude-sonnet-4-6":
		return true
	default:
		return false
	}
}

func cloneMultipliers(in map[string]Multiplier) map[string]Multiplier {
	out := make(map[string]Multiplier, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func rate(value string) *USD {
	v := mustUSD(value)
	return &v
}

func mustUSD(value string) USD {
	micros, err := parseMicros(value)
	if err != nil {
		panic(err)
	}
	return USD(micros)
}

func usdCopy(value USD) *USD { return &value }

func timePtr(value time.Time) *time.Time { return &value }
