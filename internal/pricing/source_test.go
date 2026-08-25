package pricing

import (
	"os"
	"path/filepath"
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
	body := strings.ReplaceAll(string(readFixture(t, "anthropic-pricing.md")), "5m Cache Writes", "Temporary Storage")
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
