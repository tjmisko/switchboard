package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tjmisko/switchboard/internal/pricing"
)

func TestPricingStatusReportsFallbackProvenanceInJSON(t *testing.T) {
	var output bytes.Buffer
	if err := cmdPricing([]string{"status", "--cache-dir", t.TempDir(), "--json"}, false, &output); err != nil {
		t.Fatal(err)
	}
	var decoded pricingCommandOutput
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Action != "status" || len(decoded.Diagnostics) != 2 {
		t.Fatalf("output = %+v", decoded)
	}
	for _, diagnostic := range decoded.Diagnostics {
		if !diagnostic.UsedFallback || diagnostic.Source == "" || diagnostic.VersionHash == "" || diagnostic.ModelCount == 0 {
			t.Fatalf("diagnostic = %+v", diagnostic)
		}
	}
}

func TestPricingStatusTextNamesExactProvider(t *testing.T) {
	var output bytes.Buffer
	if err := cmdPricing([]string{"status", "--provider", pricing.ProviderOpenAI, "--cache-dir", t.TempDir()}, false, &output); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, want := range []string{"pricing status", "openai", "source=", "retrieved_at=", "hash="} {
		if !strings.Contains(text, want) {
			t.Fatalf("output %q missing %q", text, want)
		}
	}
}
