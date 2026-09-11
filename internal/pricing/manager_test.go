package pricing

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

type fixtureTransport struct {
	bodies      map[string][]byte
	notModified bool
	err         error
}

func (f *fixtureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.notModified {
		return &http.Response{
			StatusCode: http.StatusNotModified, Status: "304 Not Modified",
			Header: http.Header{"ETag": []string{"fixture-v1"}}, Body: io.NopCloser(bytes.NewReader(nil)), Request: req,
		}, nil
	}
	body, ok := f.bodies[req.URL.String()]
	if !ok {
		return &http.Response{
			StatusCode: http.StatusNotFound, Status: "404 Not Found", Header: make(http.Header),
			Body: io.NopCloser(bytes.NewReader(nil)), Request: req,
		}, nil
	}
	return &http.Response{
		StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"ETag": []string{"fixture-v1"}},
		Body: io.NopCloser(bytes.NewReader(body)), Request: req,
	}, nil
}

func TestManagerRefreshesAtomicallyAndKeepsLastKnownGood(t *testing.T) {
	transport := &fixtureTransport{bodies: openAIFixtureBodies(t)}
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	manager := NewManager(t.TempDir())
	manager.Client = &http.Client{Transport: transport}
	manager.Now = func() time.Time { return now }

	catalog, diagnostic, err := manager.Load(context.Background(), ProviderOpenAI, true)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Bundled || diagnostic.ModelCount != 3 || diagnostic.UsedFallback {
		t.Fatalf("first refresh catalog=%+v diagnostic=%+v", catalog, diagnostic)
	}
	path, _ := manager.cachePath(ProviderOpenAI)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	broken := openAIFixtureBodies(t)
	broken[openAIModelDocuments[0].FetchURL] = []byte("# redesigned without prices")
	transport.bodies = broken
	now = now.Add(7 * time.Hour)
	fallback, diagnostic, err := manager.Load(context.Background(), ProviderOpenAI, true)
	if err == nil || !diagnostic.UsedFallback || diagnostic.RefreshError == "" {
		t.Fatalf("invalid refresh err=%v diagnostic=%+v", err, diagnostic)
	}
	if fallback.VersionHash != catalog.VersionHash {
		t.Fatalf("fallback hash = %s, want %s", fallback.VersionHash, catalog.VersionHash)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("failed validation replaced the last-known-good cache")
	}
}

func TestManagerConditionalRevalidationUpdatesAgeWithoutChangingVersion(t *testing.T) {
	transport := &fixtureTransport{bodies: openAIFixtureBodies(t)}
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	manager := NewManager(t.TempDir())
	manager.Client = &http.Client{Transport: transport}
	manager.Now = func() time.Time { return now }
	first, _, err := manager.Load(context.Background(), ProviderOpenAI, true)
	if err != nil {
		t.Fatal(err)
	}

	transport.notModified = true
	now = now.Add(7 * time.Hour)
	second, diagnostic, err := manager.Load(context.Background(), ProviderOpenAI, false)
	if err != nil {
		t.Fatal(err)
	}
	if !diagnostic.NotModified || !second.RetrievedAt.Equal(now) || second.VersionHash != first.VersionHash {
		t.Fatalf("second=%+v diagnostic=%+v", second, diagnostic)
	}
}

func TestManagerReturnsCachedCatalogWhenNetworkFails(t *testing.T) {
	transport := &fixtureTransport{bodies: openAIFixtureBodies(t)}
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	manager := NewManager(t.TempDir())
	manager.Client = &http.Client{Transport: transport}
	manager.Now = func() time.Time { return now }
	first, _, err := manager.Load(context.Background(), ProviderOpenAI, true)
	if err != nil {
		t.Fatal(err)
	}

	transport.err = errors.New("offline")
	now = now.Add(25 * time.Hour)
	got, diagnostic, err := manager.Load(context.Background(), ProviderOpenAI, false)
	if err == nil || !diagnostic.UsedFallback || diagnostic.Freshness != FreshnessStale {
		t.Fatalf("err=%v diagnostic=%+v", err, diagnostic)
	}
	if got.VersionHash != first.VersionHash {
		t.Fatalf("fallback hash = %s, want %s", got.VersionHash, first.VersionHash)
	}
}

func TestManagerRefreshLoopAttemptsImmediatelyAndPeriodically(t *testing.T) {
	manager := NewManager(t.TempDir())
	// No source means each bounded attempt falls back immediately without any
	// network I/O; the callback still proves startup and periodic ownership.
	manager.Sources = map[string]Source{}
	manager.RefreshInterval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempts := make(chan struct{}, 3)
	go manager.RunRefreshLoop(ctx, func(_ []Diagnostic, _ error) {
		select {
		case attempts <- struct{}{}:
		default:
		}
	})
	for i := 0; i < 2; i++ {
		select {
		case <-attempts:
		case <-time.After(time.Second):
			t.Fatalf("refresh attempt %d did not run", i+1)
		}
	}
	cancel()
}

func openAIFixtureBodies(t *testing.T) map[string][]byte {
	t.Helper()
	return map[string][]byte{
		openAIPricingDocument.FetchURL:   readFixture(t, "openai-pricing.md"),
		openAIModelDocuments[0].FetchURL: readFixture(t, "openai-gpt-5.6-sol.md"),
		openAIModelDocuments[1].FetchURL: readFixture(t, "openai-gpt-5.6-terra.md"),
		openAIModelDocuments[2].FetchURL: readFixture(t, "openai-gpt-5.6-luna.md"),
	}
}
