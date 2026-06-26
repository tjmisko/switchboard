package history

import "strings"

// pricing.go is the single source of per-model token prices used to recompute a
// dollar cost from a usage_sample's raw token counts. Claude Code's statusline
// reports its own internal cost; Switchboard owns no such number, and no native
// API exposes dollar cost for a solo Pro/Max subscription, so the activity log's
// cost is recomputed here from tokens × per-model price, keyed on the model the
// usage was sampled against.
//
// Rates are dollars per million tokens (per MTok), confirmed against the
// claude-api skill reference (Anthropic public pricing) and may need periodic
// updates as the price list changes. cacheCreate is priced at the 5-minute cache
// write rate (1.25× input); there is a small drift vs the 1-hour write rate
// (2× input), which is acceptable for an approximate activity-cost view.

// Price holds the per-MTok rates for one model family.
type Price struct {
	input       float64 // base input tokens
	output      float64 // output tokens
	cacheRead   float64 // cache-read input tokens (~0.1× input)
	cacheCreate float64 // cache-write input tokens, 5-minute TTL (1.25× input)
}

// prices maps a normalized model family to its per-MTok rates. The key is the
// family substring modelFamily extracts from a transcript model id.
//
//	input / output / cacheRead / cacheCreate, all $ per MTok:
//	  opus   (claude-opus-4-8/-4-7/-4-6)  5  / 25 / 0.5 / 6.25
//	  sonnet (claude-sonnet-4-6)          3  / 15 / 0.3 / 3.75
//	  haiku  (claude-haiku-4-5)           1  / 5  / 0.1 / 1.25
//	  fable  (claude-fable-5)             10 / 50 / 1   / 12.5
var prices = map[string]Price{
	"opus":   {input: 5, output: 25, cacheRead: 0.5, cacheCreate: 6.25},
	"sonnet": {input: 3, output: 15, cacheRead: 0.3, cacheCreate: 3.75},
	"haiku":  {input: 1, output: 5, cacheRead: 0.1, cacheCreate: 1.25},
	"fable":  {input: 10, output: 50, cacheRead: 1, cacheCreate: 12.5},
}

// priceFamilies is the deterministic lookup order for modelFamily (the families
// are mutually exclusive within any real model id, but a fixed order keeps the
// match reproducible).
var priceFamilies = []string{"opus", "sonnet", "haiku", "fable"}

// CostUSD returns the dollar cost of one usage sample: each token bucket times
// its per-token rate (the per-MTok rate / 1e6), summed. The token params are
// int64 to match the codebase's usage token fields (transcript.Usage and
// Event.Tok*).
//
// Model matching is robust to real transcript model ids — "claude-opus-4-8",
// "claude-opus-4-8[1m]", "claude-sonnet-4-6", "claude-haiku-4-5-20251001",
// "claude-fable-5" — via modelFamily, which normalizes (drops a "[…]" suffix and
// a trailing "-YYYYMMDD" date) and matches on the family substring. An unknown
// model returns 0 (and never panics), so an unpriced/foreign model contributes
// no cost rather than a wrong one.
func CostUSD(model string, tokIn, tokOut, cacheRead, cacheCreate int64) float64 {
	p, ok := prices[modelFamily(model)]
	if !ok {
		return 0
	}
	const perMTok = 1_000_000.0
	return (float64(tokIn)*p.input +
		float64(tokOut)*p.output +
		float64(cacheRead)*p.cacheRead +
		float64(cacheCreate)*p.cacheCreate) / perMTok
}

// modelFamily reduces a transcript model id to its pricing key
// ("opus"/"sonnet"/"haiku"/"fable"), or "" when none matches. It first strips a
// "[…]" context-window suffix and a trailing "-YYYYMMDD" date snapshot so the
// family search is not thrown off, then looks for a known family name.
func modelFamily(model string) string {
	m := normalizeModel(model)
	for _, family := range priceFamilies {
		if strings.Contains(m, family) {
			return family
		}
	}
	return ""
}

// normalizeModel lower-cases a model id and strips its volatile parts: a
// trailing "[…]" suffix (e.g. the "[1m]" context-window marker) and a trailing
// "-YYYYMMDD" date snapshot (e.g. "claude-haiku-4-5-20251001"). What remains
// still contains the family name modelFamily matches on.
func normalizeModel(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if i := strings.IndexByte(m, '['); i >= 0 {
		m = m[:i]
	}
	if i := strings.LastIndexByte(m, '-'); i >= 0 {
		if tail := m[i+1:]; len(tail) == 8 && isAllDigits(tail) {
			m = m[:i]
		}
	}
	return m
}

func isAllDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
