package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/tjmisko/switchboard/internal/pricing"
)

type pricingCommandOutput struct {
	Action      string               `json:"action"`
	Diagnostics []pricing.Diagnostic `json:"diagnostics"`
}

// cmdPricing is file/network-only and intentionally independent of the daemon.
// status performs no network I/O; refresh fetches only the fixed first-party
// public documents encoded by internal/pricing.
func cmdPricing(args []string, globalJSON bool, w io.Writer) error {
	if len(args) == 0 {
		return errors.New("requires status or refresh")
	}
	action := args[0]
	if action != "status" && action != "refresh" {
		return fmt.Errorf("unknown action %q (want status or refresh)", action)
	}
	fs := flag.NewFlagSet("pricing "+action, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	provider := fs.String("provider", "all", "provider: all|anthropic|openai")
	cacheDir := fs.String("cache-dir", "", "public pricing cache directory")
	asJSON := fs.Bool("json", globalJSON, "emit JSON diagnostics")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	providers, err := pricingProviders(*provider)
	if err != nil {
		return err
	}

	manager := pricing.NewManager(*cacheDir)
	diagnostics := make([]pricing.Diagnostic, 0, len(providers))
	var failures []string
	for _, name := range providers {
		if action == "status" {
			_, diagnostic, _ := manager.Status(name)
			diagnostics = append(diagnostics, diagnostic)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, diagnostic, refreshErr := manager.Load(ctx, name, true)
		cancel()
		diagnostics = append(diagnostics, diagnostic)
		if refreshErr != nil {
			failures = append(failures, name+": "+refreshErr.Error())
		}
	}

	output := pricingCommandOutput{Action: action, Diagnostics: diagnostics}
	if *asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(output); err != nil {
			return err
		}
	} else {
		renderPricingDiagnostics(w, output)
	}
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "; "))
	}
	return nil
}

func pricingProviders(value string) ([]string, error) {
	switch value {
	case "all":
		return []string{pricing.ProviderAnthropic, pricing.ProviderOpenAI}, nil
	case pricing.ProviderAnthropic, pricing.ProviderOpenAI:
		return []string{value}, nil
	default:
		return nil, fmt.Errorf("invalid provider %q (want all, anthropic, or openai)", value)
	}
}

func renderPricingDiagnostics(w io.Writer, output pricingCommandOutput) {
	fmt.Fprintf(w, "pricing %s\n", output.Action)
	for _, diagnostic := range output.Diagnostics {
		asOf := "unknown"
		if !diagnostic.RetrievedAt.IsZero() {
			asOf = diagnostic.RetrievedAt.UTC().Format(time.RFC3339)
		}
		effective := "unspecified"
		if diagnostic.EffectiveAt != nil {
			effective = diagnostic.EffectiveAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(w, "  %s freshness=%s models=%d age=%s fallback=%t\n",
			diagnostic.Provider, diagnostic.Freshness, diagnostic.ModelCount,
			(time.Duration(diagnostic.AgeSeconds) * time.Second).Round(time.Second), diagnostic.UsedFallback)
		fmt.Fprintf(w, "    source=%s\n    retrieved_at=%s effective_at=%s hash=%s\n",
			diagnostic.Source, asOf, effective, diagnostic.VersionHash)
		if diagnostic.ValidationError != "" {
			fmt.Fprintf(w, "    validation_error=%s\n", diagnostic.ValidationError)
		}
		if diagnostic.RefreshError != "" {
			fmt.Fprintf(w, "    refresh_error=%s\n", diagnostic.RefreshError)
		}
	}
}
