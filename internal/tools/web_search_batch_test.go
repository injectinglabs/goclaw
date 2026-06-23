package tools

import (
	"context"
	"strings"
	"testing"
)

type batchStubProvider struct{}

func (b *batchStubProvider) Name() string { return "stub" }
func (b *batchStubProvider) Search(_ context.Context, p searchParams) ([]searchResult, error) {
	return []searchResult{{Title: "Result for " + p.Query, URL: "https://example.com/" + p.Query, Description: "snippet"}}, nil
}

func newBatchToolForTest() *BatchWebSearchTool {
	return &BatchWebSearchTool{
		providers: []SearchProvider{&batchStubProvider{}},
		cache:     newWebCache(defaultCacheMaxEntries, defaultCacheTTL),
	}
}

func TestBatchWebSearch_RunsAllQueries(t *testing.T) {
	tool := newBatchToolForTest()
	r := tool.Execute(context.Background(), map[string]any{"queries": []any{"alpha firm", "beta firm", "gamma firm"}})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	for _, q := range []string{"alpha firm", "beta firm", "gamma firm"} {
		if !strings.Contains(r.ForLLM, q) {
			t.Fatalf("result missing query %q:\n%s", q, r.ForLLM)
		}
	}
}

func TestBatchWebSearch_RequiresQueries(t *testing.T) {
	tool := newBatchToolForTest()
	if r := tool.Execute(context.Background(), map[string]any{}); !r.IsError {
		t.Fatal("expected error when queries missing")
	}
	if r := tool.Execute(context.Background(), map[string]any{"queries": []any{"", "  "}}); !r.IsError {
		t.Fatal("expected error when queries are all blank")
	}
}

func TestBatchWebSearch_TruncatesAboveMax(t *testing.T) {
	tool := newBatchToolForTest()
	qs := make([]any, maxBatchQueries+5)
	for i := range qs {
		qs[i] = "query"
	}
	r := tool.Execute(context.Background(), map[string]any{"queries": qs})
	if !strings.Contains(r.ForLLM, "only the first") {
		t.Fatalf("expected truncation note for >%d queries", maxBatchQueries)
	}
}
