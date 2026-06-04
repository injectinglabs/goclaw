// Package providers — model_alias_fetcher.go
//
// Periodically fetches the LLM service's GET /v1/models endpoint and seeds the
// shared ModelRegistry with provider-agnostic alias entries. Without this, when
// a client sends `model="default"`, the registry has no entry → resolver falls
// back to Loop.contextWindow (default 200K) → compaction triggers way too early
// for models with large context windows (e.g. deepseek-v4-flash with 1M).
//
// See: internal/agent/loop_pipeline_adapter.go ResolveContextWindow.
package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// ModelAliasFetcher polls the LLM service /v1/models endpoint and registers
// returned model rows as aliases on the shared model registry.
type ModelAliasFetcher struct {
	registry     AliasRegisterer
	url          string
	token        string
	refresh      time.Duration
	httpClient   *http.Client
	stopOnce     sync.Once
	doneOnce     sync.Once
	stopCh       chan struct{}
	doneCh       chan struct{}
	lastFetched  time.Time
	mu           sync.RWMutex
	lastAliasCnt int
}

// ModelAliasFetcherOpt customises the fetcher.
type ModelAliasFetcherOpt func(*ModelAliasFetcher)

// WithRefreshInterval overrides the polling interval (default 5 minutes).
func WithRefreshInterval(d time.Duration) ModelAliasFetcherOpt {
	return func(f *ModelAliasFetcher) { f.refresh = d }
}

// WithHTTPClient overrides the underlying http.Client. Mainly used by tests.
func WithHTTPClient(c *http.Client) ModelAliasFetcherOpt {
	return func(f *ModelAliasFetcher) { f.httpClient = c }
}

// WithFetcherURL overrides the endpoint URL. Mainly used by tests.
func WithFetcherURL(url string) ModelAliasFetcherOpt {
	return func(f *ModelAliasFetcher) { f.url = url }
}

// WithAuthToken overrides the bearer token. Mainly used by tests.
func WithAuthToken(t string) ModelAliasFetcherOpt {
	return func(f *ModelAliasFetcher) { f.token = t }
}

// NewModelAliasFetcher constructs a fetcher. URL/token default to the
// `LLM_SERVICE_URL` and `LLM_INTERNAL_AUTH_TOKEN` env vars respectively. The
// fetcher is created in a stopped state — call Start to begin polling.
func NewModelAliasFetcher(registry AliasRegisterer, opts ...ModelAliasFetcherOpt) *ModelAliasFetcher {
	f := &ModelAliasFetcher{
		registry: registry,
		url:      strings.TrimRight(os.Getenv("LLM_SERVICE_URL"), "/"),
		token:    os.Getenv("LLM_INTERNAL_AUTH_TOKEN"),
		refresh:  5 * time.Minute,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
	for _, o := range opts {
		o(f)
	}
	return f
}

// Start kicks off an initial synchronous fetch (so the registry is warm before
// the first agent run) and a background goroutine that re-fetches every
// `refresh` duration. Idempotent: doneCh is closed exactly once via sync.Once.
// On a missing endpoint URL the fetcher logs and exits — it never blocks
// startup.
func (f *ModelAliasFetcher) Start(ctx context.Context) {
	if f == nil || f.registry == nil {
		return
	}
	if f.url == "" {
		slog.Info("model_alias_fetcher.disabled_no_url")
		f.closeDone()
		return
	}

	// Initial fetch — log on failure but never block.
	if _, err := f.fetchOnce(ctx); err != nil {
		slog.Warn("model_alias_fetcher.initial_fetch_failed", "url", f.url, "error", err)
	}

	go f.loop(ctx)
}

// closeDone guards doneCh against double-close (happens if Start is invoked on
// a fetcher with no URL more than once, or if Stop fires after the loop has
// already exited).
func (f *ModelAliasFetcher) closeDone() {
	f.doneOnce.Do(func() { close(f.doneCh) })
}

func (f *ModelAliasFetcher) loop(ctx context.Context) {
	defer f.closeDone()
	t := time.NewTicker(f.refresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-f.stopCh:
			return
		case <-t.C:
			if _, err := f.fetchOnce(ctx); err != nil {
				slog.Warn("model_alias_fetcher.refresh_failed", "url", f.url, "error", err)
			}
		}
	}
}

// Stop terminates the polling goroutine.
func (f *ModelAliasFetcher) Stop() {
	if f == nil {
		return
	}
	f.stopOnce.Do(func() { close(f.stopCh) })
}

// LastFetched returns the timestamp of the most recent successful fetch and
// the number of aliases registered during that fetch. Useful for diagnostics.
func (f *ModelAliasFetcher) LastFetched() (time.Time, int) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.lastFetched, f.lastAliasCnt
}

// modelAliasEntry mirrors a single row from GET /v1/models. Unknown fields
// are ignored.
type modelAliasEntry struct {
	Alias         string `json:"alias"`
	ID            string `json:"id"`
	Provider      string `json:"provider"`
	ModelID       string `json:"model_id"`
	ContextWindow int    `json:"context_window"`
	MaxTokens     int    `json:"max_tokens"`
	Vision        bool   `json:"vision"`
	Reasoning     bool   `json:"reasoning"`
}

type modelsResponse struct {
	Data []modelAliasEntry `json:"data"`
}

// fetchOnce performs a single GET /v1/models call, registers aliases, and
// returns the number of aliases registered. Used internally by Start/loop and
// by tests.
func (f *ModelAliasFetcher) fetchOnce(ctx context.Context) (int, error) {
	if f.url == "" {
		return 0, errors.New("LLM_SERVICE_URL not set")
	}
	endpoint := f.url + "/v1/models"

	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(rctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	if f.token != "" {
		req.Header.Set("Authorization", "Bearer "+f.token)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return 0, fmt.Errorf("models endpoint returned status %d: %s", resp.StatusCode, string(body))
	}

	var decoded modelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return 0, fmt.Errorf("decode body: %w", err)
	}

	registered := 0
	for _, row := range decoded.Data {
		alias := row.Alias
		if alias == "" {
			alias = row.ID
		}
		if alias == "" || row.ContextWindow <= 0 {
			continue
		}
		spec := ModelSpec{
			ID:               alias,
			Provider:         row.Provider,
			ContextWindow:    row.ContextWindow,
			MaxTokens:        row.MaxTokens,
			Reasoning:        row.Reasoning,
			Vision:           row.Vision,
			UpstreamProvider: row.Provider,
			UpstreamModel:    row.ModelID,
		}
		f.registry.RegisterAlias(alias, spec)
		registered++
	}

	f.mu.Lock()
	f.lastFetched = time.Now()
	f.lastAliasCnt = registered
	f.mu.Unlock()

	slog.Info("model_alias_fetcher.refreshed", "aliases", registered, "url", endpoint)
	return registered, nil
}
