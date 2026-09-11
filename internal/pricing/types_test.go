package pricing

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestUSDJSONIsDecimalSafeAndDollarDenominated(t *testing.T) {
	want := USDFromMicros(12_345_678)
	body, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(body); got != "12.345678" {
		t.Fatalf("MarshalJSON = %s, want 12.345678", got)
	}
	var roundTrip USD
	if err := json.Unmarshal([]byte("12.3456784"), &roundTrip); err != nil {
		t.Fatal(err)
	}
	if roundTrip != want {
		t.Fatalf("round trip = %s, want %s", roundTrip, want)
	}
}

func TestEstimateAndVendorSnapshotJSONPreserveNullMoney(t *testing.T) {
	body, err := json.Marshal(Estimate{Status: CostUnknown})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"api_equivalent_usd":null`, `"vendor_estimated_usd":null`, `"estimated_billed_usd":null`} {
		if !strings.Contains(string(body), field) {
			t.Fatalf("estimate JSON %s missing %s", body, field)
		}
	}
	vendor, err := json.Marshal(VendorUsageSnapshot{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(vendor), `"estimated_usage_usd":null`) {
		t.Fatalf("vendor snapshot JSON coerced absent USD: %s", vendor)
	}
}

func TestBootstrapCatalogsValidateAndOnlyUseExactAliases(t *testing.T) {
	set := BootstrapCatalogs()
	for provider, catalog := range set {
		if err := catalog.Validate(); err != nil {
			t.Fatalf("%s bootstrap catalog: %v", provider, err)
		}
		if catalog.VersionHash == "" {
			t.Fatalf("%s bootstrap has no version hash", provider)
		}
		if got := catalog.FreshnessAt(time.Date(2026, 8, 25, 1, 0, 0, 0, time.UTC)); got != FreshnessStale {
			t.Fatalf("%s bundled freshness = %s, want stale", provider, got)
		}
		if got := catalog.FreshnessAt(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)); got != FreshnessUnusable {
			t.Fatalf("%s seven-day-old bundled freshness = %s, want unusable", provider, got)
		}
	}
	anthropic := set[ProviderAnthropic]
	if _, ok := anthropic.Lookup("claude-opus-4-8"); !ok {
		t.Fatal("exact Claude model was not found")
	}
	if _, ok := anthropic.Lookup("claude-opus-4-8[1m]"); !ok {
		t.Fatal("explicit Claude context alias was not found")
	}
	if _, ok := anthropic.Lookup("some-opus-model"); ok {
		t.Fatal("family substring unexpectedly matched")
	}
	openai := set[ProviderOpenAI]
	if model, ok := openai.Lookup("gpt-5.6"); !ok || model.ExactModelID != "gpt-5.6-sol" {
		t.Fatalf("explicit OpenAI alias = %+v, ok=%t", model, ok)
	}
}

func TestCatalogFreshnessBoundaries(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	catalog := BootstrapCatalogs()[ProviderOpenAI]
	catalog.Bundled = false
	catalog.RetrievedAt = now.Add(-StaleAfter)
	if got := catalog.FreshnessAt(now); got != FreshnessStale {
		t.Fatalf("24h catalog freshness = %s, want stale", got)
	}
	catalog.RetrievedAt = now.Add(-UnusableAfter)
	if got := catalog.FreshnessAt(now); got != FreshnessUnusable {
		t.Fatalf("7d catalog freshness = %s, want unusable", got)
	}
}
