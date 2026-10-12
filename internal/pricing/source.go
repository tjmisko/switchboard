package pricing

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DocumentSpec separates the machine-fetched Markdown route from the public
// human-facing URL retained in provenance.
type DocumentSpec struct {
	Key       string
	FetchURL  string
	PublicURL string
}

// Source parses a bounded set of first-party public pricing documents. Models
// APIs are intentionally absent: neither supported vendor exposes prices there.
type Source interface {
	Provider() string
	PrimaryURL() string
	Documents() []DocumentSpec
	Parse(documents map[string][]byte, retrievedAt time.Time) (Catalog, error)
}

// OfficialSources returns the first-party Anthropic and OpenAI publication
// adapters used by Manager.
func OfficialSources() map[string]Source {
	return map[string]Source{
		ProviderAnthropic: anthropicMarkdownSource{},
		ProviderOpenAI:    openAIMarkdownSource{},
	}
}

type anthropicMarkdownSource struct{}

func (anthropicMarkdownSource) Provider() string   { return ProviderAnthropic }
func (anthropicMarkdownSource) PrimaryURL() string { return AnthropicPricingURL }
func (anthropicMarkdownSource) Documents() []DocumentSpec {
	return []DocumentSpec{{
		Key: "pricing", FetchURL: AnthropicPricingURL + ".md", PublicURL: AnthropicPricingURL,
	}}
}

func (anthropicMarkdownSource) Parse(documents map[string][]byte, retrievedAt time.Time) (Catalog, error) {
	body := documents["pricing"]
	if len(body) == 0 {
		return Catalog{}, errors.New("anthropic pricing document is empty")
	}
	return ParseAnthropicMarkdown(body, retrievedAt)
}

type openAIMarkdownSource struct{}

var openAIPricingDocument = DocumentSpec{
	Key: "pricing", FetchURL: OpenAIPricingURL + ".md", PublicURL: OpenAIPricingURL,
}

var openAIModelDocuments = []DocumentSpec{
	{Key: "gpt-5.6-sol", FetchURL: "https://developers.openai.com/api/docs/models/gpt-5.6-sol.md", PublicURL: "https://developers.openai.com/api/docs/models/gpt-5.6-sol"},
	{Key: "gpt-5.6-terra", FetchURL: "https://developers.openai.com/api/docs/models/gpt-5.6-terra.md", PublicURL: "https://developers.openai.com/api/docs/models/gpt-5.6-terra"},
	{Key: "gpt-5.6-luna", FetchURL: "https://developers.openai.com/api/docs/models/gpt-5.6-luna.md", PublicURL: "https://developers.openai.com/api/docs/models/gpt-5.6-luna"},
}

func (openAIMarkdownSource) Provider() string   { return ProviderOpenAI }
func (openAIMarkdownSource) PrimaryURL() string { return OpenAIPricingURL }
func (openAIMarkdownSource) Documents() []DocumentSpec {
	documents := []DocumentSpec{openAIPricingDocument}
	return append(documents, openAIModelDocuments...)
}

func (openAIMarkdownSource) Parse(documents map[string][]byte, retrievedAt time.Time) (Catalog, error) {
	bootstrap := BootstrapCatalogs()[ProviderOpenAI]
	models := make(map[string]ModelPrice, len(openAIModelDocuments))
	urls := make([]string, 0, len(openAIModelDocuments)+1)
	urls = append(urls, openAIPricingDocument.PublicURL)
	for _, document := range openAIModelDocuments {
		body := documents[document.Key]
		if len(body) == 0 {
			return Catalog{}, fmt.Errorf("OpenAI pricing document %s is empty", document.Key)
		}
		template := bootstrap.Models[document.Key]
		price, err := ParseOpenAIModelMarkdown(document.Key, template.Aliases, body)
		if err != nil {
			return Catalog{}, fmt.Errorf("parse %s: %w", document.Key, err)
		}
		models[price.ExactModelID] = price
		urls = append(urls, document.PublicURL)
	}
	if err := applyOpenAIPricingPublication(models, documents[openAIPricingDocument.Key]); err != nil {
		return Catalog{}, fmt.Errorf("parse OpenAI pricing publication: %w", err)
	}
	catalog := Catalog{
		SchemaVersion: CatalogSchemaVersion,
		Provider:      ProviderOpenAI,
		SourceURL:     OpenAIPricingURL,
		SourceURLs:    urls,
		RetrievedAt:   retrievedAt.UTC(),
		Models:        models,
	}
	if err := finishParsedCatalog(&catalog); err != nil {
		return Catalog{}, err
	}
	return catalog, nil
}

// ParseAnthropicMarkdown extracts the exact model table and documented pricing
// modifiers. Its required headers and core model rows make a publication layout
// change fail closed.
func ParseAnthropicMarkdown(body []byte, retrievedAt time.Time) (Catalog, error) {
	lines := strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n")
	headerAt, columns := findAnthropicHeader(lines)
	if headerAt < 0 {
		return Catalog{}, errors.New("model pricing table header not found")
	}
	bootstrap := BootstrapCatalogs()[ProviderAnthropic]
	models := make(map[string]ModelPrice)
	banded := make(map[string]*anthropicPromptBands)
	for _, line := range lines[headerAt+1:] {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			break
		}
		cells := markdownCells(line)
		if len(cells) <= columns.max() || markdownSeparator(cells) {
			continue
		}
		name := plainMarkdown(cells[columns.model])
		if !strings.HasPrefix(name, "Claude ") {
			continue
		}
		// Every Claude row must resolve. Skipping an unrecognized row is how
		// a new model release used to leave its whole usage unpriced.
		row, err := parseAnthropicModelName(name)
		if err != nil {
			return Catalog{}, err
		}
		values := []struct {
			cell int
			name string
		}{
			{columns.input, "input"}, {columns.write5m, "5-minute cache write"},
			{columns.write1h, "1-hour cache write"}, {columns.cached, "cache hit"},
			{columns.output, "output"},
		}
		rates := make([]USD, len(values))
		for i, value := range values {
			rate, err := dollarFromText(cells[value.cell])
			if err != nil {
				return Catalog{}, fmt.Errorf("%s %s rate: %w", name, value.name, err)
			}
			rates[i] = rate
		}
		card := RateCard{
			Input: usdCopy(rates[0]), CacheWrite5m: usdCopy(rates[1]),
			CacheWrite1h: usdCopy(rates[2]), CachedInput: usdCopy(rates[3]), Output: usdCopy(rates[4]),
		}
		if row.promptBand == "" {
			if _, duplicate := models[row.id]; duplicate || banded[row.id] != nil {
				return Catalog{}, fmt.Errorf("Anthropic model %s appears in more than one row", row.id)
			}
			price := anthropicTemplate(bootstrap, row.id)
			price.Base = card
			models[row.id] = price
			continue
		}
		if _, duplicate := models[row.id]; duplicate {
			return Catalog{}, fmt.Errorf("Anthropic model %s has both a flat and a prompt-length row", row.id)
		}
		bands := banded[row.id]
		if bands == nil {
			bands = &anthropicPromptBands{threshold: row.threshold}
			banded[row.id] = bands
		}
		if bands.threshold != row.threshold {
			return Catalog{}, fmt.Errorf("Anthropic model %s prompt-length rows disagree on the threshold", row.id)
		}
		switch row.promptBand {
		case anthropicPromptUpTo:
			if bands.upTo != nil {
				return Catalog{}, fmt.Errorf("Anthropic model %s repeats its short-prompt row", row.id)
			}
			bands.upTo = &card
		case anthropicPromptOver:
			if bands.over != nil {
				return Catalog{}, fmt.Errorf("Anthropic model %s repeats its long-prompt row", row.id)
			}
			bands.over = &card
		}
	}
	for id, bands := range banded {
		if bands.upTo == nil || bands.over == nil {
			return Catalog{}, fmt.Errorf("Anthropic model %s lacks one of its prompt-length rows", id)
		}
		price := anthropicTemplate(bootstrap, id)
		price.Base = *bands.upTo
		price.ContextBands = []ContextBand{{MinInputTokensExclusive: bands.threshold, Rates: *bands.over}}
		models[id] = price
	}
	for _, required := range []string{
		"claude-fable-5-1", "claude-fable-5", "claude-opus-5-5", "claude-opus-5", "claude-opus-4-8",
		"claude-sonnet-5-5", "claude-sonnet-5", "claude-sonnet-4-6",
		"claude-haiku-5-5", "claude-haiku-4-5-20251001",
	} {
		if _, ok := models[required]; !ok {
			return Catalog{}, fmt.Errorf("required Anthropic model row %s is missing", required)
		}
	}

	normalized := strings.ToLower(strings.ReplaceAll(string(body), "×", "x"))
	// Current first-party Claude 4.6+ pricing explicitly keeps the full 1M
	// context window at standard token/cache rates. Validate that publication
	// contract so a future long-context premium cannot be silently missed.
	if !regexp.MustCompile(`(?s)full.{0,160}1m\s+token\s+context\s+window.{0,160}standard\s+pricing`).MatchString(normalized) {
		return Catalog{}, errors.New("standard full-context pricing marker is missing")
	}
	if !regexp.MustCompile(`1\.1\s*x\s+pricing multiplier`).MatchString(normalized) {
		return Catalog{}, errors.New("US inference 1.1x multiplier marker is missing")
	}
	if !regexp.MustCompile(`(?s)code execution is free.{0,160}web search.{0,120}web fetch`).MatchString(normalized) {
		return Catalog{}, errors.New("code-execution-with-web free-pricing marker is missing")
	}
	for id, price := range models {
		if anthropicSupportsUSGeo(id) {
			price.Multipliers = map[string]Multiplier{"inference_geo=us": {Numerator: 11, Denominator: 10}}
		}
		models[id] = price
	}

	searchRate, err := parseAnthropicWebSearchRate(normalized)
	if err != nil {
		return Catalog{}, err
	}
	for id, price := range models {
		price.ToolCharges = map[string]UnitPrice{
			"web_search": {Unit: "request", MicrosPerUnit: searchRate},
			"web_fetch":  {Unit: "request", MicrosPerUnit: 0},
		}
		models[id] = price
	}

	fastRates, err := parseAnthropicFastRates(lines)
	if err != nil {
		return Catalog{}, err
	}
	for _, required := range []string{"claude-opus-5-5", "claude-opus-5", "claude-opus-4-8"} {
		if _, ok := fastRates[required]; !ok {
			return Catalog{}, fmt.Errorf("required Anthropic fast-mode row %s is missing", required)
		}
	}
	for id, fast := range fastRates {
		price, ok := models[id]
		if !ok {
			return Catalog{}, fmt.Errorf("Anthropic fast-mode row %s has no model-pricing row", id)
		}
		variant, err := anthropicFastVariant(price.Base, fast.input, fast.output)
		if err != nil {
			return Catalog{}, fmt.Errorf("Anthropic fast-mode rates for %s: %w", id, err)
		}
		price.Variants = map[string]VariantPrice{"speed=fast": {Rates: variant}}
		models[id] = price
	}

	catalog := Catalog{
		SchemaVersion: CatalogSchemaVersion,
		Provider:      ProviderAnthropic,
		SourceURL:     AnthropicPricingURL,
		SourceURLs:    []string{AnthropicPricingURL},
		RetrievedAt:   retrievedAt.UTC(),
		Models:        models,
	}
	if err := finishParsedCatalog(&catalog); err != nil {
		return Catalog{}, err
	}
	return catalog, nil
}

// ParseOpenAIModelMarkdown extracts one exact model's standard token prices and
// its documented cache-write and long-context modifiers.
func ParseOpenAIModelMarkdown(modelID string, aliases []string, body []byte) (ModelPrice, error) {
	section := markdownSection(string(body), "pricing")
	if section == "" {
		return ModelPrice{}, errors.New("pricing section not found")
	}
	input, cached, output, err := parseOpenAITextRates(section)
	if err != nil {
		return ModelPrice{}, err
	}
	normalized := strings.ToLower(strings.ReplaceAll(string(body), "×", "x"))
	if !regexp.MustCompile(`cache writes?.{0,100}1\.25\s*x`).MatchString(normalized) &&
		!regexp.MustCompile(`1\.25\s*x.{0,100}cache writes?`).MatchString(normalized) {
		return ModelPrice{}, errors.New("1.25x cache-write marker is missing")
	}
	if !regexp.MustCompile(`>\s*272\s*k`).MatchString(normalized) ||
		!regexp.MustCompile(`2\s*x\s+input`).MatchString(normalized) ||
		!regexp.MustCompile(`1\.5\s*x\s+output`).MatchString(normalized) {
		return ModelPrice{}, errors.New("272K long-context pricing markers are missing")
	}
	write := multiplyUSD(input, Multiplier{Numerator: 5, Denominator: 4})
	return ModelPrice{
		ExactModelID: modelID,
		Aliases:      append([]string(nil), aliases...),
		Base: RateCard{
			Input: usdCopy(input), CachedInput: usdCopy(cached), CacheWrite: usdCopy(write), Output: usdCopy(output),
		},
		ContextBands: []ContextBand{{
			MinInputTokensExclusive: 272_000,
			Rates: RateCard{
				Input:       usdCopy(multiplyUSD(input, Multiplier{Numerator: 2, Denominator: 1})),
				CachedInput: usdCopy(multiplyUSD(cached, Multiplier{Numerator: 2, Denominator: 1})),
				CacheWrite:  usdCopy(multiplyUSD(write, Multiplier{Numerator: 2, Denominator: 1})),
				Output:      usdCopy(multiplyUSD(output, Multiplier{Numerator: 3, Denominator: 2})),
			},
		}},
	}, nil
}

// applyOpenAIPricingPublication cross-checks model-page standard rates and
// extracts independently published Fast-mode short/long cards and web-search
// charges. A live refresh never carries these variants from bootstrap data.
func applyOpenAIPricingPublication(models map[string]ModelPrice, body []byte) error {
	if len(body) == 0 {
		return errors.New("pricing publication is empty")
	}
	normalized := strings.ToLower(strings.ReplaceAll(string(body), "×", "x"))
	if !strings.Contains(normalized, "regional processing") ||
		!regexp.MustCompile(`data\s+residency`).MatchString(normalized) ||
		!regexp.MustCompile(`10\s*%\s+(?:pricing\s+)?(?:premium|uplift)`).MatchString(normalized) ||
		!regexp.MustCompile(`(?:march|mar\.?)[^\n]{0,30}5[^\n]{0,30}2026`).MatchString(normalized) {
		return errors.New("regional/data-residency 10% uplift eligibility marker is missing")
	}
	rows := make(map[string][][]USD)
	for _, line := range strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n") {
		cells := markdownCells(line)
		if len(cells) != 9 {
			continue
		}
		id := plainMarkdown(cells[0])
		if _, expected := models[id]; !expected {
			continue
		}
		values := make([]USD, 0, 8)
		for _, cell := range cells[1:] {
			value, err := dollarFromText(cell)
			if err != nil {
				values = nil
				break
			}
			values = append(values, value)
		}
		if values != nil {
			rows[id] = append(rows[id], values)
		}
	}
	for id, model := range models {
		if model.Base.Input == nil || model.Base.CachedInput == nil || model.Base.CacheWrite == nil ||
			model.Base.Output == nil || len(model.ContextBands) != 1 ||
			model.ContextBands[0].Rates.Input == nil || model.ContextBands[0].Rates.CachedInput == nil ||
			model.ContextBands[0].Rates.CacheWrite == nil || model.ContextBands[0].Rates.Output == nil {
			return fmt.Errorf("%s model-page rates are incomplete", id)
		}
		standard := []USD{
			*model.Base.Input, *model.Base.CachedInput, *model.Base.CacheWrite, *model.Base.Output,
			*model.ContextBands[0].Rates.Input, *model.ContextBands[0].Rates.CachedInput,
			*model.ContextBands[0].Rates.CacheWrite, *model.ContextBands[0].Rates.Output,
		}
		standardSeen := false
		var fast []USD
		for _, row := range rows[id] {
			if equalUSDSlice(row, standard) {
				standardSeen = true
			}
			if isFastOpenAIRow(row, standard) {
				fast = row
			}
		}
		if !standardSeen {
			return fmt.Errorf("%s standard short/long row does not match its model page", id)
		}
		if fast == nil {
			return fmt.Errorf("%s fixture-verified Fast-mode short/long row is missing", id)
		}
		fastPrice := VariantPrice{
			Rates: RateCard{
				Input: usdCopy(fast[0]), CachedInput: usdCopy(fast[1]), CacheWrite: usdCopy(fast[2]), Output: usdCopy(fast[3]),
			},
			ContextBands: []ContextBand{{
				MinInputTokensExclusive: 272_000,
				Rates: RateCard{
					Input: usdCopy(fast[4]), CachedInput: usdCopy(fast[5]), CacheWrite: usdCopy(fast[6]), Output: usdCopy(fast[7]),
				},
			}},
		}
		model.Variants = map[string]VariantPrice{
			"speed=fast":            fastPrice,
			"service_tier=fast":     fastPrice,
			"service_tier=priority": fastPrice,
		}
		model.Multipliers = map[string]Multiplier{
			"inference_geo=regional":       {Numerator: 11, Denominator: 10},
			"inference_geo=data_residency": {Numerator: 11, Denominator: 10},
		}
		models[id] = model
	}

	toolRE := regexp.MustCompile(`(?i)web search[^\n$]{0,100}\$\s*([0-9]+(?:\.[0-9]+)?)\s*/\s*1k calls`)
	match := toolRE.FindStringSubmatch(string(body))
	if len(match) != 2 {
		return errors.New("web-search $/1k call price is missing")
	}
	perThousandMicros, err := parseMicros(match[1])
	if err != nil {
		return fmt.Errorf("web-search price: %w", err)
	}
	perThousand := USD(perThousandMicros)
	perCall := USD(perThousand.Micros() / 1_000)
	for id, model := range models {
		model.ToolCharges = map[string]UnitPrice{"web_search": {Unit: "request", MicrosPerUnit: perCall}}
		models[id] = model
	}
	return nil
}

func equalUSDSlice(a, b []USD) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func isFastOpenAIRow(row, standard []USD) bool {
	if len(row) != 8 || len(standard) != 8 {
		return false
	}
	for i := range row {
		if row[i] != multiplyUSD(standard[i], Multiplier{Numerator: 2, Denominator: 1}) {
			return false
		}
	}
	return true
}

type anthropicColumns struct{ model, input, write5m, write1h, cached, output int }

func (c anthropicColumns) max() int {
	values := []int{c.model, c.input, c.write5m, c.write1h, c.cached, c.output}
	sort.Ints(values)
	return values[len(values)-1]
}

func findAnthropicHeader(lines []string) (int, anthropicColumns) {
	for i, line := range lines {
		cells := markdownCells(line)
		if len(cells) < 6 {
			continue
		}
		columns := anthropicColumns{model: -1, input: -1, write5m: -1, write1h: -1, cached: -1, output: -1}
		for column, cell := range cells {
			value := strings.ToLower(plainMarkdown(cell))
			switch {
			case value == "model":
				columns.model = column
			case strings.Contains(value, "base input"):
				columns.input = column
			case strings.Contains(value, "5m cache"):
				columns.write5m = column
			case strings.Contains(value, "1h cache"):
				columns.write1h = column
			case strings.Contains(value, "cache hits") || strings.Contains(value, "cache read"):
				columns.cached = column
			case strings.Contains(value, "output"):
				columns.output = column
			}
		}
		if columns.model >= 0 && columns.input >= 0 && columns.write5m >= 0 && columns.write1h >= 0 && columns.cached >= 0 && columns.output >= 0 {
			return i, columns
		}
	}
	return -1, anthropicColumns{}
}

const (
	anthropicPromptUpTo = "up to"
	anthropicPromptOver = "over"
)

// anthropicModelRow is one resolved model-table row. A model priced by prompt
// length has two rows, one per side of the threshold.
type anthropicModelRow struct {
	id         string
	promptBand string // "", anthropicPromptUpTo or anthropicPromptOver
	threshold  int64
}

type anthropicPromptBands struct {
	threshold  int64
	upTo, over *RateCard
}

type anthropicFastRate struct{ input, output USD }

var anthropicModelName = regexp.MustCompile(`^Claude ([A-Z][a-z]+) ([0-9]+)(?:\.([0-9]+))?(?: \((.*)\))?$`)
var anthropicPromptQualifier = regexp.MustCompile(`^for prompts (up to|over) ([0-9][0-9,]*) tokens$`)

// anthropicDatedModelIDs lists the models whose exact API id carries a release
// date instead of following claude-<family>-<major>[-<minor>].
var anthropicDatedModelIDs = map[string]string{
	"Claude Sonnet 4.5": "claude-sonnet-4-5-20250929",
	"Claude Sonnet 4":   "claude-sonnet-4-20250514",
	"Claude Haiku 4.5":  "claude-haiku-4-5-20251001",
	"Claude Haiku 3.5":  "claude-3-5-haiku-20241022",
}

// parseAnthropicModelName resolves a pricing-table display name such as
// "Claude Opus 5.5" or "Claude Haiku 5.5 (for prompts over 100,000 tokens)" to
// its exact API id. Ids are derived rather than listed so that a model release
// is priced without a code change; a row this cannot read is an error.
func parseAnthropicModelName(name string) (anthropicModelRow, error) {
	match := anthropicModelName.FindStringSubmatch(name)
	if match == nil {
		return anthropicModelRow{}, fmt.Errorf("unrecognized Anthropic model row %q", name)
	}
	family, major, minor, qualifier := match[1], match[2], match[3], match[4]
	display := "Claude " + family + " " + major
	id := "claude-" + strings.ToLower(family) + "-" + major
	if minor != "" {
		display += "." + minor
		id += "-" + minor
	}
	if dated, ok := anthropicDatedModelIDs[display]; ok {
		id = dated
	}
	row := anthropicModelRow{id: id}
	if qualifier == "" || strings.HasPrefix(qualifier, "retired") || strings.HasPrefix(qualifier, "limited availability") {
		return row, nil
	}
	band := anthropicPromptQualifier.FindStringSubmatch(qualifier)
	if band == nil {
		return anthropicModelRow{}, fmt.Errorf("unrecognized Anthropic model row qualifier %q", name)
	}
	threshold, err := strconv.ParseInt(strings.ReplaceAll(band[2], ",", ""), 10, 64)
	if err != nil || threshold <= 0 {
		return anthropicModelRow{}, fmt.Errorf("invalid prompt-length threshold in %q", name)
	}
	row.promptBand, row.threshold = band[1], threshold
	return row, nil
}

// anthropicTemplate keeps a bootstrap model's exact-id aliases when one
// exists; a model released after this build gets its long-context alias.
func anthropicTemplate(bootstrap Catalog, id string) ModelPrice {
	template, ok := bootstrap.Models[id]
	if !ok {
		return ModelPrice{ExactModelID: id, Aliases: []string{id + "[1m]"}}
	}
	template.Variants = nil
	template.Multipliers = nil
	template.ContextBands = nil
	return template
}

// parseAnthropicFastRates reads the fast-mode table, whose rows may name
// several models ("Claude Opus 5 / Claude Opus 4.8"). It reads only the
// table under its own heading so a later three-column table cannot leak in.
func parseAnthropicFastRates(lines []string) (map[string]anthropicFastRate, error) {
	start := -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") && strings.EqualFold(strings.TrimSpace(strings.TrimLeft(trimmed, "#")), "Fast mode pricing") {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return nil, errors.New("Claude fast-mode pricing section is missing")
	}
	rates := make(map[string]anthropicFastRate)
	for _, line := range lines[start:] {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			break
		}
		cells := markdownCells(line)
		if len(cells) != 3 || markdownSeparator(cells) {
			continue
		}
		names := plainMarkdown(cells[0])
		if !strings.HasPrefix(names, "Claude ") {
			continue
		}
		input, err := dollarFromText(cells[1])
		if err != nil {
			return nil, fmt.Errorf("fast-mode input rate for %s: %w", names, err)
		}
		output, err := dollarFromText(cells[2])
		if err != nil {
			return nil, fmt.Errorf("fast-mode output rate for %s: %w", names, err)
		}
		for _, name := range strings.Split(names, "/") {
			row, err := parseAnthropicModelName(strings.TrimSpace(name))
			if err != nil {
				return nil, err
			}
			if row.promptBand != "" {
				return nil, fmt.Errorf("fast-mode row %q has a prompt-length qualifier", name)
			}
			if _, duplicate := rates[row.id]; duplicate {
				return nil, fmt.Errorf("fast-mode rates for %s appear twice", row.id)
			}
			rates[row.id] = anthropicFastRate{input: input, output: output}
		}
	}
	if len(rates) == 0 {
		return nil, errors.New("Claude fast-mode rate table is empty")
	}
	return rates, nil
}

// anthropicFastVariant builds a fast-mode card. The publication lists only
// input and output; prompt-caching multipliers "apply on top of fast mode
// pricing", and those multipliers differ by model (0.1x, 0.05x, 0.025x cache
// hits), so each cache rate keeps the model's own ratio to its base input.
func anthropicFastVariant(base RateCard, input, output USD) (RateCard, error) {
	if base.Input == nil || base.Input.Micros() <= 0 || base.CachedInput == nil ||
		base.CacheWrite5m == nil || base.CacheWrite1h == nil || base.Output == nil {
		return RateCard{}, errors.New("base rates are incomplete")
	}
	if input <= *base.Input || output <= *base.Output {
		return RateCard{}, errors.New("fast-mode rates are not a premium over standard rates")
	}
	scale := func(rate *USD) (*USD, error) {
		value, err := applyMultiplier(input, Multiplier{Numerator: rate.Micros(), Denominator: base.Input.Micros()})
		if err != nil {
			return nil, err
		}
		return &value, nil
	}
	cached, err := scale(base.CachedInput)
	if err != nil {
		return RateCard{}, err
	}
	write5m, err := scale(base.CacheWrite5m)
	if err != nil {
		return RateCard{}, err
	}
	write1h, err := scale(base.CacheWrite1h)
	if err != nil {
		return RateCard{}, err
	}
	return RateCard{
		Input: usdCopy(input), CachedInput: cached, CacheWrite5m: write5m,
		CacheWrite1h: write1h, Output: usdCopy(output),
	}, nil
}

func parseAnthropicWebSearchRate(normalized string) (USD, error) {
	re := regexp.MustCompile(`\$\s*([0-9]+(?:\.[0-9]+)?)\s+per\s+1,?000\s+search`)
	match := re.FindStringSubmatch(normalized)
	if len(match) != 2 {
		return 0, errors.New("web-search unit price marker is missing")
	}
	totalMicros, err := parseMicros(match[1])
	if err != nil {
		return 0, fmt.Errorf("web-search price: %w", err)
	}
	total := USD(totalMicros)
	return USD(total.Micros() / 1_000), nil
}

func parseOpenAITextRates(section string) (USD, USD, USD, error) {
	lines := strings.Split(strings.ReplaceAll(section, "\r\n", "\n"), "\n")
	values := map[string]USD{}
	for i, line := range lines {
		cells := markdownCells(line)
		if len(cells) == 0 {
			continue
		}
		indices := map[string]int{}
		for column, cell := range cells {
			switch strings.ToLower(plainMarkdown(cell)) {
			case "input":
				indices["input"] = column
			case "cached input":
				indices["cached"] = column
			case "output":
				indices["output"] = column
			}
		}
		if len(indices) == 3 {
			for _, next := range lines[i+1:] {
				row := markdownCells(next)
				if markdownSeparator(row) || len(row) == 0 {
					continue
				}
				if len(row) <= maxIndex(indices) {
					break
				}
				parsed := map[string]USD{}
				for key, column := range indices {
					value, err := dollarFromText(row[column])
					if err != nil {
						parsed = nil
						break
					}
					parsed[key] = value
				}
				if parsed != nil {
					return parsed["input"], parsed["cached"], parsed["output"], nil
				}
				break
			}
		}
		if len(cells) >= 2 {
			label := strings.ToLower(plainMarkdown(cells[0]))
			key := openAIRateKey(label)
			if key != "" {
				if value, err := dollarFromText(strings.Join(cells[1:], " ")); err == nil {
					values[key] = value
				}
			}
		}
	}
	// The official Markdown renderer may emit a label and number on adjacent
	// plain lines rather than a pipe table.
	for i, line := range lines {
		key := openAIRateKey(strings.ToLower(plainMarkdown(line)))
		if key == "" || values[key] != 0 {
			continue
		}
		for _, next := range lines[i+1 : min(i+5, len(lines))] {
			if value, err := dollarFromText(next); err == nil {
				values[key] = value
				break
			}
		}
	}
	if values["input"] <= 0 || values["cached"] <= 0 || values["output"] <= 0 {
		return 0, 0, 0, errors.New("input/cached-input/output price triple not found")
	}
	return values["input"], values["cached"], values["output"], nil
}

func openAIRateKey(label string) string {
	label = strings.TrimSpace(strings.Trim(label, "#*:_- "))
	switch label {
	case "input":
		return "input"
	case "cached input":
		return "cached"
	case "output":
		return "output"
	default:
		return ""
	}
}

func maxIndex(values map[string]int) int {
	max := -1
	for _, value := range values {
		if value > max {
			max = value
		}
	}
	return max
}

var markdownLink = regexp.MustCompile(`\[([^\]]+)\]\([^)]*\)`)
var dollarAmount = regexp.MustCompile(`\$\s*([0-9]+(?:\.[0-9]+)?)`)

func markdownCells(line string) []string {
	line = strings.TrimSpace(line)
	if !strings.Contains(line, "|") {
		return nil
	}
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")
	raw := strings.Split(line, "|")
	result := make([]string, len(raw))
	for i, cell := range raw {
		result[i] = strings.TrimSpace(cell)
	}
	return result
}

func markdownSeparator(cells []string) bool {
	if len(cells) == 0 {
		return false
	}
	for _, cell := range cells {
		trimmed := strings.Trim(cell, " :-")
		if trimmed != "" {
			return false
		}
	}
	return true
}

func plainMarkdown(value string) string {
	value = markdownLink.ReplaceAllString(value, "$1")
	value = strings.NewReplacer("`", "", "*", "", "_", "", "&nbsp;", " ", "\u00a0", " ").Replace(value)
	return strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
}

func dollarFromText(value string) (USD, error) {
	match := dollarAmount.FindStringSubmatch(value)
	if len(match) != 2 {
		return 0, errors.New("dollar amount not found")
	}
	micros, err := parseMicros(match[1])
	if err != nil {
		return 0, err
	}
	return USD(micros), nil
}

func markdownSection(document, heading string) string {
	lines := strings.Split(strings.ReplaceAll(document, "\r\n", "\n"), "\n")
	start := -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "##") && strings.EqualFold(strings.TrimSpace(strings.TrimLeft(trimmed, "#")), heading) {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return ""
	}
	end := len(lines)
	for i := start; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "## ") {
			end = i
			break
		}
	}
	return strings.Join(lines[start:end], "\n")
}

func finishParsedCatalog(catalog *Catalog) error {
	if err := catalog.Validate(); err != nil {
		return fmt.Errorf("validate parsed catalog: %w", err)
	}
	hash, err := catalog.ContentHash()
	if err != nil {
		return err
	}
	catalog.VersionHash = hash
	return nil
}
