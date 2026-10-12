package pricing

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseAnthropicMarkdownFixture(t *testing.T) {
	body := readFixture(t, "anthropic-pricing.md")
	at := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	catalog, err := ParseAnthropicMarkdown(body, at)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Bundled || !catalog.RetrievedAt.Equal(at) || catalog.VersionHash == "" {
		t.Fatalf("catalog metadata = %+v", catalog)
	}
	model, ok := catalog.Lookup("claude-sonnet-5")
	if !ok || model.Base.Input == nil || *model.Base.Input != mustUSD("2") || model.Base.CacheWrite1h == nil || *model.Base.CacheWrite1h != mustUSD("4") {
		t.Fatalf("Sonnet 5 = %+v, ok=%t", model, ok)
	}
	fast := catalog.Models["claude-opus-4-8"].Variants["speed=fast"].Rates
	if fast.Input == nil || *fast.Input != mustUSD("10") || fast.CacheWrite1h == nil || *fast.CacheWrite1h != mustUSD("20") {
		t.Fatalf("fast rates = %+v", fast)
	}
}

func TestParseAnthropicMarkdownFailsClosedOnHeaderChange(t *testing.T) {
	body := strings.ReplaceAll(string(readFixture(t, "anthropic-pricing.md")), "5m cache writes", "Temporary storage")
	if _, err := ParseAnthropicMarkdown([]byte(body), time.Now()); err == nil {
		t.Fatal("parser accepted a source whose required header changed")
	}
}

func TestParseAnthropicMarkdownFailsClosedWithoutStandardLongContextMarker(t *testing.T) {
	body := strings.ReplaceAll(string(readFixture(t, "anthropic-pricing.md")), "standard pricing", "premium pricing")
	if _, err := ParseAnthropicMarkdown([]byte(body), time.Now()); err == nil {
		t.Fatal("parser accepted a source whose long-context billing semantics changed")
	}
}

func TestParseAnthropicMarkdownFailsClosedWithoutCodeExecutionFreeMarker(t *testing.T) {
	body := strings.ReplaceAll(string(readFixture(t, "anthropic-pricing.md")),
		"Code execution is free when used with web search or web fetch.",
		"Code execution pricing is unspecified.")
	if _, err := ParseAnthropicMarkdown([]byte(body), time.Now()); err == nil {
		t.Fatal("parser accepted a source whose code-execution billing semantics changed")
	}
}

func TestOpenAISourceParsesEveryExactModelFixture(t *testing.T) {
	documents := map[string][]byte{
		"pricing":       readFixture(t, "openai-pricing.md"),
		"gpt-5.6-sol":   readFixture(t, "openai-gpt-5.6-sol.md"),
		"gpt-5.6-terra": readFixture(t, "openai-gpt-5.6-terra.md"),
		"gpt-5.6-luna":  readFixture(t, "openai-gpt-5.6-luna.md"),
	}
	catalog, err := (openAIMarkdownSource{}).Parse(documents, time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"} {
		model, ok := catalog.Lookup(id)
		if !ok {
			t.Errorf("model %s missing", id)
			continue
		}
		for _, variant := range []string{"speed=fast", "service_tier=fast", "service_tier=priority"} {
			price, exists := model.Variants[variant]
			if !exists || price.Rates.Input == nil || len(price.ContextBands) != 1 || price.ContextBands[0].Rates.Output == nil {
				t.Errorf("model %s variant %s incomplete: %+v", id, variant, price)
			}
		}
	}
	sol := catalog.Models["gpt-5.6-sol"]
	if sol.Base.CacheWrite == nil || *sol.Base.CacheWrite != mustUSD("5") {
		t.Fatalf("Sol cache write = %+v", sol.Base.CacheWrite)
	}
	if len(sol.ContextBands) != 1 || sol.ContextBands[0].Rates.Output == nil || *sol.ContextBands[0].Rates.Output != mustUSD("30") {
		t.Fatalf("Sol context bands = %+v", sol.ContextBands)
	}
	fastPrice, ok := sol.Variants["service_tier=priority"]
	if !ok || fastPrice.Rates.Output == nil || *fastPrice.Rates.Output != mustUSD("40") ||
		len(fastPrice.ContextBands) != 1 || fastPrice.ContextBands[0].Rates.Output == nil || *fastPrice.ContextBands[0].Rates.Output != mustUSD("60") {
		t.Fatalf("Sol fast variant = %+v, ok=%t", fastPrice, ok)
	}
	if _, unsupported := sol.Variants["service_tier=ultrafast"]; unsupported {
		t.Fatal("ultrafast was aliased to Fast without a published ultrafast rate")
	}
	if multiplier := sol.Multipliers["inference_geo=regional"]; multiplier.Numerator != 11 || multiplier.Denominator != 10 {
		t.Fatalf("Sol regional multiplier = %+v", multiplier)
	}
}

func TestOpenAISourceFailsClosedWithoutRegionalMarker(t *testing.T) {
	documents := map[string][]byte{
		"pricing":       []byte(strings.ReplaceAll(string(readFixture(t, "openai-pricing.md")), "regional processing", "some processing")),
		"gpt-5.6-sol":   readFixture(t, "openai-gpt-5.6-sol.md"),
		"gpt-5.6-terra": readFixture(t, "openai-gpt-5.6-terra.md"),
		"gpt-5.6-luna":  readFixture(t, "openai-gpt-5.6-luna.md"),
	}
	if _, err := (openAIMarkdownSource{}).Parse(documents, time.Now()); err == nil {
		t.Fatal("parser accepted a source missing regional eligibility evidence")
	}
}

func TestParseOpenAIFailsClosedWithoutLongContextMarker(t *testing.T) {
	body := strings.ReplaceAll(string(readFixture(t, "openai-gpt-5.6-sol.md")), ">272K", ">many")
	if _, err := ParseOpenAIModelMarkdown("gpt-5.6-sol", nil, []byte(body)); err == nil {
		t.Fatal("parser accepted a source missing the long-context threshold")
	}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func parseAnthropicFixture(t *testing.T, edit func(string) string) (Catalog, error) {
	t.Helper()
	body := string(readFixture(t, "anthropic-pricing.md"))
	if edit != nil {
		body = edit(body)
	}
	return ParseAnthropicMarkdown([]byte(body), time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC))
}

func assertRate(t *testing.T, label string, got *USD, want string) {
	t.Helper()
	if got == nil || *got != mustUSD(want) {
		t.Fatalf("%s = %v, want $%s", label, got, want)
	}
}

func TestParseAnthropicMarkdownPricesClaude5xRowsAndTheirOwnCacheMultipliers(t *testing.T) {
	catalog, err := parseAnthropicFixture(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	opus55, ok := catalog.Lookup("claude-opus-5-5[1m]")
	if !ok {
		t.Fatal("claude-opus-5-5[1m] is unpriced")
	}
	assertRate(t, "Opus 5.5 input", opus55.Base.Input, "4")
	assertRate(t, "Opus 5.5 cache hit (0.05x)", opus55.Base.CachedInput, "0.2")
	assertRate(t, "Opus 5.5 5m write", opus55.Base.CacheWrite5m, "5")
	assertRate(t, "Opus 5.5 1h write", opus55.Base.CacheWrite1h, "8")
	assertRate(t, "Opus 5.5 output", opus55.Base.Output, "20")
	assertRate(t, "Fable 5.1 cache hit (0.025x)", catalog.Models["claude-fable-5-1"].Base.CachedInput, "0.25")
	assertRate(t, "Sonnet 5.5 cache hit (0.05x)", catalog.Models["claude-sonnet-5-5"].Base.CachedInput, "0.1")
	for _, id := range []string{"claude-mythos-5-1", "claude-opus-4-1", "claude-opus-4", "claude-sonnet-4-20250514", "claude-3-5-haiku-20241022"} {
		if _, ok := catalog.Models[id]; !ok {
			t.Fatalf("qualified or dated row %s is missing", id)
		}
	}
}

func TestParseAnthropicMarkdownBandsHaiku55ByPromptLength(t *testing.T) {
	catalog, err := parseAnthropicFixture(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	haiku := catalog.Models["claude-haiku-5-5"]
	assertRate(t, "Haiku 5.5 short-prompt input", haiku.Base.Input, "0.1")
	if len(haiku.ContextBands) != 1 || haiku.ContextBands[0].MinInputTokensExclusive != 100_000 {
		t.Fatalf("Haiku 5.5 bands = %+v", haiku.ContextBands)
	}
	long := haiku.ContextBands[0].Rates
	assertRate(t, "Haiku 5.5 long-prompt input", long.Input, "0.5")
	assertRate(t, "Haiku 5.5 long-prompt cache hit", long.CachedInput, "0.05")
	assertRate(t, "Haiku 5.5 long-prompt output", long.Output, "2.5")
}

func TestParseAnthropicMarkdownScalesFastCacheRatesByEachModelsOwnMultiplier(t *testing.T) {
	catalog, err := parseAnthropicFixture(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	opus55 := catalog.Models["claude-opus-5-5"].Variants["speed=fast"].Rates
	assertRate(t, "Opus 5.5 fast input", opus55.Input, "8")
	assertRate(t, "Opus 5.5 fast cache hit", opus55.CachedInput, "0.4")
	assertRate(t, "Opus 5.5 fast 5m write", opus55.CacheWrite5m, "10")
	assertRate(t, "Opus 5.5 fast 1h write", opus55.CacheWrite1h, "16")
	assertRate(t, "Opus 5.5 fast output", opus55.Output, "40")
	opus5 := catalog.Models["claude-opus-5"].Variants["speed=fast"].Rates
	assertRate(t, "Opus 5 fast cache hit", opus5.CachedInput, "1")
	if _, ok := catalog.Models["claude-opus-4-7"].Variants["speed=fast"]; ok {
		t.Fatal("Opus 4.7 has a fast variant the publication does not list")
	}
}

func TestParseAnthropicMarkdownAppliesUSGeoToClaude46AndLaterOnly(t *testing.T) {
	catalog, err := parseAnthropicFixture(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{
		"claude-opus-5-5": true, "claude-haiku-5-5": true, "claude-fable-5-1": true,
		"claude-opus-4-6": true, "claude-sonnet-4-6": true,
		"claude-opus-4-5": false, "claude-sonnet-4-5-20250929": false, "claude-haiku-4-5-20251001": false,
	} {
		_, got := catalog.Models[id].Multipliers["inference_geo=us"]
		if got != want {
			t.Fatalf("%s US geo multiplier present = %t, want %t", id, got, want)
		}
	}
}

func TestParseAnthropicMarkdownPricesAnUnreleasedModelWithoutACodeChange(t *testing.T) {
	catalog, err := parseAnthropicFixture(t, func(body string) string {
		row := "| Claude Opus 6 | $3 / MTok | $3.75 / MTok | $6 / MTok | $0.15 / MTok | $15 / MTok |\n"
		return strings.Replace(body, "| Claude Opus 5.5 ", strings.TrimSuffix(row, "\n")+"\n| Claude Opus 5.5 ", 1)
	})
	if err != nil {
		t.Fatal(err)
	}
	model, ok := catalog.Lookup("claude-opus-6[1m]")
	if !ok {
		t.Fatal("derived id claude-opus-6 is not priced")
	}
	assertRate(t, "Opus 6 input", model.Base.Input, "3")
	if _, ok := model.Multipliers["inference_geo=us"]; !ok {
		t.Fatal("a post-4.6 model lacks the US geo multiplier")
	}
}

func TestParseAnthropicMarkdownFailsClosedOnAnUnreadableClaudeRow(t *testing.T) {
	for name, edit := range map[string]func(string) string{
		"unknown qualifier": func(body string) string {
			return strings.Replace(body, "| Claude Opus 5.5 ", "| Claude Opus 5.5 (preview pricing) ", 1)
		},
		"unparseable name": func(body string) string {
			return strings.Replace(body, "| Claude Opus 5.5 ", "| Claude Opus Five ", 1)
		},
		"half of a prompt-length pair": func(body string) string {
			start := strings.Index(body, "| Claude Haiku 5.5 (for prompts over")
			end := start + strings.Index(body[start:], "\n") + 1
			return body[:start] + body[end:]
		},
		"fast row for an unpriced model": func(body string) string {
			return strings.Replace(body, "| Claude Opus 5.5                 | $8", "| Claude Opus 9.9                 | $8", 1)
		},
	} {
		if _, err := parseAnthropicFixture(t, edit); err == nil {
			t.Fatalf("%s: parser accepted the publication", name)
		}
	}
}

func TestBootstrapAnthropicMatchesTheParsedPublication(t *testing.T) {
	parsed, err := parseAnthropicFixture(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	for id, bundled := range BootstrapCatalogs()[ProviderAnthropic].Models {
		live, ok := parsed.Models[id]
		if !ok {
			t.Fatalf("bootstrap model %s is absent from the publication", id)
		}
		bundled.Aliases, live.Aliases = nil, nil
		if !reflect.DeepEqual(bundled, live) {
			t.Fatalf("bootstrap %s drifted from the publication:\nbundled %+v\nparsed  %+v", id, bundled, live)
		}
	}
}
