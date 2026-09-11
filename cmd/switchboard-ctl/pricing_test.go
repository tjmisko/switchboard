package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjmisko/switchboard/internal/pricing"
)

// isolateTimelinePrices gives CLI tests their own known catalog. The command
// still uses its production cache reader, without depending on host prices or
// the age of the bundled fallback.
func isolateTimelinePrices(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", root)
	dir := filepath.Join(root, "switchboard", "pricing")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for provider, catalog := range pricing.BootstrapCatalogs() {
		catalog.RetrievedAt = time.Now().UTC()
		catalog.Bundled = false
		hash, err := catalog.ContentHash()
		if err != nil {
			t.Fatal(err)
		}
		catalog.VersionHash = hash
		body, err := json.Marshal(catalog)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, provider+".json"), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

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
