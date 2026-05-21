package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

const sampleModelsBody = `{
  "data": [
    {"alias": "default", "id": "deepseek/deepseek-v4-flash", "provider": "openrouter", "context_window": 1048576, "max_tokens": 8192},
    {"alias": "fast", "id": "openai/gpt-4.1-mini", "provider": "openai", "context_window": 200000, "max_tokens": 4096},
    {"alias": "", "id": "", "provider": "openai", "context_window": 100000},
    {"alias": "bogus", "provider": "openai", "context_window": 0}
  ]
}`

func TestModelAliasFetcher_RegistersAliases(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer testtok" {
			t.Fatalf("unexpected auth header: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(sampleModelsBody))
	}))
	defer srv.Close()

	reg := NewInMemoryRegistry()
	f := NewModelAliasFetcher(reg,
		WithFetcherURL(srv.URL),
		WithAuthToken("testtok"),
	)
	n, err := f.fetchOnce(context.Background())
	if err != nil {
		t.Fatalf("fetchOnce: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 aliases registered, got %d", n)
	}

	// Per the brief, when client sends model="default", resolver must find a
	// 1M-context spec under the openrouter-compat provider.
	if spec := reg.Resolve("openrouter-compat", "default"); spec == nil || spec.ContextWindow != 1048576 {
		t.Fatalf("default not resolvable under openrouter-compat (spec=%v)", spec)
	}
	// "fast" should be reachable under any provider key for the summarizer call.
	if spec := reg.Resolve("openai", "fast"); spec == nil || spec.ContextWindow != 200000 {
		t.Fatalf("fast not resolvable under openai (spec=%v)", spec)
	}
}

func TestModelAliasFetcher_DisabledWhenNoURL(t *testing.T) {
	reg := NewInMemoryRegistry()
	f := NewModelAliasFetcher(reg, WithFetcherURL(""))
	// Should exit cleanly — no goroutine, no panic.
	f.Start(context.Background())
}

func TestModelAliasFetcher_ResilientOn5xx(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer srv.Close()

	reg := NewInMemoryRegistry()
	f := NewModelAliasFetcher(reg, WithFetcherURL(srv.URL))
	_, err := f.fetchOnce(context.Background())
	if err == nil {
		t.Fatalf("expected error from 502 response")
	}
	// Subsequent fetches must still work without panicking — simulate by
	// invoking again.
	if _, err2 := f.fetchOnce(context.Background()); err2 == nil {
		t.Fatalf("expected error on retry")
	}
	if atomic.LoadInt32(&hits) < 2 {
		t.Fatalf("expected ≥2 fetch attempts, got %d", hits)
	}
}

func TestModelAliasFetcher_LoopRespectsContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sampleModelsBody))
	}))
	defer srv.Close()

	reg := NewInMemoryRegistry()
	f := NewModelAliasFetcher(reg,
		WithFetcherURL(srv.URL),
		WithRefreshInterval(50*time.Millisecond),
	)

	ctx, cancel := context.WithCancel(context.Background())
	f.Start(ctx)
	time.Sleep(150 * time.Millisecond)
	cancel()
	// loop goroutine should exit; verify by giving it a moment then checking
	// that doneCh closes within timeout.
	select {
	case <-f.doneCh:
	case <-time.After(time.Second):
		t.Fatalf("loop did not exit after context cancel")
	}

	if ts, _ := f.LastFetched(); ts.IsZero() {
		t.Fatalf("expected LastFetched to be set")
	}
}
