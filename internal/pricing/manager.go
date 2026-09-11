package pricing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	RefreshAfter          = 6 * time.Hour
	StaleAfter            = 24 * time.Hour
	UnusableAfter         = 7 * 24 * time.Hour
	RefreshAttemptTimeout = time.Minute
	maxDocumentBytes      = 4 << 20
)

// Diagnostic is content-free refresh/cache state suitable for CLI output.
type Diagnostic struct {
	Provider        string     `json:"provider"`
	Source          string     `json:"source"`
	RetrievedAt     time.Time  `json:"retrieved_at,omitempty"`
	EffectiveAt     *time.Time `json:"effective_at,omitempty"`
	AgeSeconds      int64      `json:"age_seconds,omitempty"`
	VersionHash     string     `json:"version_hash,omitempty"`
	ModelCount      int        `json:"model_count"`
	Freshness       Freshness  `json:"freshness"`
	NeedsRefresh    bool       `json:"needs_refresh"`
	UsedFallback    bool       `json:"used_fallback,omitempty"`
	NotModified     bool       `json:"not_modified,omitempty"`
	ValidationError string     `json:"validation_error,omitempty"`
	RefreshError    string     `json:"refresh_error,omitempty"`
}

// Manager refreshes and atomically caches official price publications.
type Manager struct {
	CacheDir string
	Client   *http.Client
	Sources  map[string]Source
	Now      func() time.Time
	// RefreshInterval is primarily injectable for tests. Zero uses RefreshAfter.
	RefreshInterval time.Duration
}

// RunRefreshLoop owns normal-operation refresh: it loads immediately at
// startup, then periodically revalidates the official publications. A failed
// attempt leaves the atomically stored last-known-good catalogs untouched.
// onAttempt is optional and receives only content-free diagnostics.
func (m *Manager) RunRefreshLoop(ctx context.Context, onAttempt func([]Diagnostic, error)) {
	interval := m.RefreshInterval
	if interval <= 0 {
		interval = RefreshAfter
	}
	attempt := func() {
		attemptCtx, cancel := context.WithTimeout(ctx, RefreshAttemptTimeout)
		defer cancel()
		_, diagnostics, err := m.LoadAll(attemptCtx, false)
		if onAttempt != nil {
			onAttempt(diagnostics, err)
		}
	}
	attempt()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			attempt()
		}
	}
}

func NewManager(cacheDir string) *Manager {
	if cacheDir == "" {
		cacheDir = DefaultCacheDir()
	}
	return &Manager{
		CacheDir: cacheDir,
		Client:   &http.Client{Timeout: 10 * time.Second},
		Sources:  OfficialSources(),
		Now:      time.Now,
	}
}

// DefaultCacheDir stores regenerable public price data separately from durable
// history. No credentials are read or written.
func DefaultCacheDir() string {
	if root := os.Getenv("XDG_CACHE_HOME"); root != "" {
		return filepath.Join(root, "switchboard", "pricing")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "switchboard-pricing")
	}
	return filepath.Join(home, ".cache", "switchboard", "pricing")
}

// Load returns a current catalog when possible. It refreshes an absent or
// six-hour-old cache. On fetch/parse failure it returns the last known good
// cache (or bundled catalog) together with the refresh error; callers can still
// use the catalog, whose freshness determines estimate status.
func (m *Manager) Load(ctx context.Context, provider string, force bool) (Catalog, Diagnostic, error) {
	now := m.now().UTC()
	cached, cacheErr := m.readCache(provider)
	if cacheErr == nil && !force && !needsRefresh(cached, now) {
		return cached, diagnosticFor(cached, now), nil
	}
	source := m.Sources[provider]
	if source == nil {
		err := fmt.Errorf("no pricing source for provider %q", provider)
		return m.fallback(provider, cached, cacheErr, now, err)
	}

	refreshed, notModified, err := m.refresh(ctx, source, cached, cacheErr == nil, now)
	if err != nil {
		return m.fallback(provider, cached, cacheErr, now, err)
	}
	if err := m.writeCache(refreshed); err != nil {
		return m.fallback(provider, cached, cacheErr, now, fmt.Errorf("write cache: %w", err))
	}
	diagnostic := diagnosticFor(refreshed, now)
	diagnostic.NotModified = notModified
	return refreshed, diagnostic, nil
}

// LoadAll refreshes/loads all known official providers in deterministic order.
// It returns usable fallback catalogs even when one or more refreshes fail.
func (m *Manager) LoadAll(ctx context.Context, force bool) (CatalogSet, []Diagnostic, error) {
	set := make(CatalogSet)
	providers := []string{ProviderAnthropic, ProviderOpenAI}
	diagnostics := make([]Diagnostic, 0, len(providers))
	var failures []string
	for _, provider := range providers {
		catalog, diagnostic, err := m.Load(ctx, provider, force)
		if len(catalog.Models) > 0 {
			set[provider] = catalog
		}
		diagnostics = append(diagnostics, diagnostic)
		if err != nil {
			failures = append(failures, provider+": "+err.Error())
		}
	}
	if len(failures) > 0 {
		return set, diagnostics, errors.New(strings.Join(failures, "; "))
	}
	return set, diagnostics, nil
}

// Status reads the last-known-good cache without network access, falling back
// to the explicitly stale bundled catalog.
func (m *Manager) Status(provider string) (Catalog, Diagnostic, error) {
	now := m.now().UTC()
	catalog, err := m.readCache(provider)
	if err == nil {
		return catalog, diagnosticFor(catalog, now), nil
	}
	bootstrap, ok := BootstrapCatalogs()[provider]
	if !ok {
		return Catalog{}, Diagnostic{Provider: provider, Freshness: FreshnessUnusable}, err
	}
	diagnostic := diagnosticFor(bootstrap, now)
	diagnostic.UsedFallback = true
	diagnostic.ValidationError = err.Error()
	return bootstrap, diagnostic, err
}

// CachedOrBootstrapCatalogs is the non-networking price book used by timeline
// folds. The daemon refresh loop and `switchboard-ctl pricing refresh` update
// the cache atomically.
func CachedOrBootstrapCatalogs(cacheDir string, now time.Time) CatalogSet {
	manager := NewManager(cacheDir)
	manager.Now = func() time.Time { return now }
	set := make(CatalogSet)
	for _, provider := range []string{ProviderAnthropic, ProviderOpenAI} {
		catalog, _, _ := manager.Status(provider)
		set[provider] = catalog
	}
	return set
}

func (m *Manager) refresh(ctx context.Context, source Source, previous Catalog, hasPrevious bool, now time.Time) (Catalog, bool, error) {
	documents := make(map[string][]byte)
	validators := make(map[string]HTTPValidator)
	notModified := make(map[string]bool)
	for _, spec := range source.Documents() {
		validator := HTTPValidator{}
		if hasPrevious {
			validator = previous.Validators[spec.FetchURL]
		}
		body, nextValidator, unchanged, err := m.fetch(ctx, spec.FetchURL, validator)
		if err != nil {
			return Catalog{}, false, err
		}
		validators[spec.FetchURL] = nextValidator
		notModified[spec.Key] = unchanged
		if !unchanged {
			documents[spec.Key] = body
		}
	}
	allNotModified := len(notModified) > 0
	for _, unchanged := range notModified {
		allNotModified = allNotModified && unchanged
	}
	if allNotModified {
		if !hasPrevious {
			return Catalog{}, false, errors.New("source returned 304 without a cached catalog")
		}
		previous.RetrievedAt = now
		previous.Bundled = false
		previous.Validators = validators
		return previous, true, nil
	}
	// A mixed 200/304 response cannot be parsed without the unchanged document
	// body. Refetch only those documents unconditionally.
	for _, spec := range source.Documents() {
		if !notModified[spec.Key] {
			continue
		}
		body, nextValidator, unchanged, err := m.fetch(ctx, spec.FetchURL, HTTPValidator{})
		if err != nil {
			return Catalog{}, false, err
		}
		if unchanged {
			return Catalog{}, false, fmt.Errorf("%s returned 304 to an unconditional request", spec.FetchURL)
		}
		documents[spec.Key] = body
		validators[spec.FetchURL] = nextValidator
	}
	catalog, err := source.Parse(documents, now)
	if err != nil {
		return Catalog{}, false, fmt.Errorf("parse %s pricing: %w", source.Provider(), err)
	}
	catalog.Provider = source.Provider()
	catalog.SourceURL = source.PrimaryURL()
	catalog.RetrievedAt = now
	catalog.Bundled = false
	catalog.Validators = validators
	if err := catalog.Validate(); err != nil {
		return Catalog{}, false, fmt.Errorf("validate %s pricing: %w", source.Provider(), err)
	}
	hash, err := catalog.ContentHash()
	if err != nil {
		return Catalog{}, false, err
	}
	catalog.VersionHash = hash
	return catalog, false, nil
}

func (m *Manager) fetch(ctx context.Context, url string, validator HTTPValidator) ([]byte, HTTPValidator, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, HTTPValidator{}, false, err
	}
	req.Header.Set("Accept", "text/markdown, text/plain;q=0.9")
	req.Header.Set("User-Agent", "switchboard-pricing/1")
	if validator.ETag != "" {
		req.Header.Set("If-None-Match", validator.ETag)
	}
	if validator.LastModified != "" {
		req.Header.Set("If-Modified-Since", validator.LastModified)
	}
	client := m.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, HTTPValidator{}, false, err
	}
	defer resp.Body.Close()
	next := HTTPValidator{ETag: resp.Header.Get("ETag"), LastModified: resp.Header.Get("Last-Modified")}
	if resp.StatusCode == http.StatusNotModified {
		if next.ETag == "" {
			next.ETag = validator.ETag
		}
		if next.LastModified == "" {
			next.LastModified = validator.LastModified
		}
		return nil, next, true, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, next, false, fmt.Errorf("GET %s: HTTP %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDocumentBytes+1))
	if err != nil {
		return nil, next, false, err
	}
	if len(body) > maxDocumentBytes {
		return nil, next, false, fmt.Errorf("GET %s: document exceeds %d bytes", url, maxDocumentBytes)
	}
	if len(body) == 0 {
		return nil, next, false, fmt.Errorf("GET %s: empty document", url)
	}
	return body, next, false, nil
}

func (m *Manager) fallback(provider string, cached Catalog, cacheErr error, now time.Time, refreshErr error) (Catalog, Diagnostic, error) {
	if cacheErr == nil {
		diagnostic := diagnosticFor(cached, now)
		diagnostic.UsedFallback = true
		diagnostic.RefreshError = refreshErr.Error()
		return cached, diagnostic, refreshErr
	}
	bootstrap, ok := BootstrapCatalogs()[provider]
	if !ok {
		return Catalog{}, Diagnostic{Provider: provider, Freshness: FreshnessUnusable, RefreshError: refreshErr.Error()}, refreshErr
	}
	diagnostic := diagnosticFor(bootstrap, now)
	diagnostic.UsedFallback = true
	diagnostic.RefreshError = refreshErr.Error()
	if cacheErr != nil {
		diagnostic.ValidationError = cacheErr.Error()
	}
	return bootstrap, diagnostic, refreshErr
}

func (m *Manager) readCache(provider string) (Catalog, error) {
	path, err := m.cachePath(provider)
	if err != nil {
		return Catalog{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return Catalog{}, err
	}
	defer f.Close()
	dec := json.NewDecoder(io.LimitReader(f, 8<<20))
	dec.DisallowUnknownFields()
	var catalog Catalog
	if err := dec.Decode(&catalog); err != nil {
		return Catalog{}, fmt.Errorf("decode %s: %w", path, err)
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return Catalog{}, fmt.Errorf("decode %s trailing data: %w", path, err)
	}
	if err := catalog.Validate(); err != nil {
		return Catalog{}, fmt.Errorf("validate %s: %w", path, err)
	}
	hash, err := catalog.ContentHash()
	if err != nil {
		return Catalog{}, err
	}
	if catalog.VersionHash == "" || catalog.VersionHash != hash {
		return Catalog{}, fmt.Errorf("validate %s: version hash mismatch", path)
	}
	return catalog, nil
}

func (m *Manager) writeCache(catalog Catalog) error {
	path, err := m.cachePath(catalog.Provider)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".catalog-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	clean := func() {
		tmp.Close()
		_ = os.Remove(tmpPath)
	}
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(catalog); err != nil {
		clean()
		return err
	}
	if err := tmp.Sync(); err != nil {
		clean()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func (m *Manager) cachePath(provider string) (string, error) {
	switch provider {
	case ProviderAnthropic, ProviderOpenAI:
		return filepath.Join(m.CacheDir, provider+".json"), nil
	default:
		return "", fmt.Errorf("invalid provider %q", provider)
	}
}

func (m *Manager) now() time.Time {
	if m.Now == nil {
		return time.Now()
	}
	return m.Now()
}

func needsRefresh(catalog Catalog, now time.Time) bool {
	if catalog.Bundled || catalog.RetrievedAt.IsZero() {
		return true
	}
	age := now.Sub(catalog.RetrievedAt)
	return age < 0 || age >= RefreshAfter
}

func diagnosticFor(catalog Catalog, now time.Time) Diagnostic {
	age := now.Sub(catalog.RetrievedAt)
	if age < 0 {
		age = 0
	}
	return Diagnostic{
		Provider: catalog.Provider, Source: catalog.SourceURL, RetrievedAt: catalog.RetrievedAt,
		EffectiveAt: catalog.EffectiveAt,
		AgeSeconds:  int64(age.Seconds()), VersionHash: catalog.VersionHash, ModelCount: len(catalog.Models),
		Freshness: catalog.FreshnessAt(now), NeedsRefresh: needsRefresh(catalog, now), UsedFallback: catalog.Bundled,
	}
}
