package tools

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

const (
	// maxBatchQueries caps how many queries a single batch_web_search call runs.
	maxBatchQueries = 100
	// batchSearchConcurrency bounds concurrent provider calls (respects upstream
	// rate limits while still finishing N queries in N/concurrency short waves).
	batchSearchConcurrency = 8
	// batchDefaultCount keeps each query's result set small — enough to extract
	// a few fields, without bloating the combined response.
	batchDefaultCount = 3
)

// BatchWebSearchTool runs many web searches CONCURRENTLY in one tool call.
//
// This is the cheap/fast path for "look up the same kind of info for N items"
// (building a table/sheet): instead of N separate web_search turns — or, worse,
// spawning N sub-agents that each run a full LLM research loop — the model
// makes ONE call with N queries, the searches fan out as plain HTTP against the
// provider chain (Tavily/Brave/etc.), and all results come back together for a
// single extraction pass. No per-search model turns, no sub-agent overhead.
type BatchWebSearchTool struct {
	providers []SearchProvider
	cache     *webCache
}

// NewBatchWebSearchTool builds the tool from the same provider chain as
// web_search. Returns nil when no search provider is configured.
func NewBatchWebSearchTool(cfg WebSearchConfig) *BatchWebSearchTool {
	providers := buildSearchProviders(cfg)
	if len(providers) == 0 {
		return nil
	}
	ttl := cfg.CacheTTL
	if ttl <= 0 {
		ttl = defaultCacheTTL
	}
	return &BatchWebSearchTool{
		providers: providers,
		cache:     newWebCache(defaultCacheMaxEntries, ttl),
	}
}

func (t *BatchWebSearchTool) Name() string { return "batch_web_search" }

func (t *BatchWebSearchTool) Description() string {
	return "Run MANY web searches CONCURRENTLY in one call and get all results back together. " +
		"Far faster and cheaper than issuing separate web_search calls or spawning sub-agents to search. " +
		"Use this whenever you need the same kind of info for many items (building a spreadsheet/table of N items): " +
		"pass one focused query per item (e.g. 'Sequoia Capital official website headquarters stage focus'), then extract the columns from all results in a single pass."
}

func (t *BatchWebSearchTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"queries": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": fmt.Sprintf("Search queries — one per item to look up (max %d per call). They run concurrently.", maxBatchQueries),
			},
			"count": map[string]any{
				"type":        "number",
				"description": fmt.Sprintf("Results to return per query (1-%d). Default %d — keep it small for extraction.", maxSearchCount, batchDefaultCount),
				"minimum":     1.0,
				"maximum":     float64(maxSearchCount),
			},
		},
		"required": []string{"queries"},
	}
}

func (t *BatchWebSearchTool) Execute(ctx context.Context, args map[string]any) *Result {
	raw, ok := args["queries"].([]any)
	if !ok || len(raw) == 0 {
		return ErrorResult("queries is required (a non-empty array of query strings)")
	}
	queries := make([]string, 0, len(raw))
	for _, q := range raw {
		if s, ok := q.(string); ok {
			if s = strings.TrimSpace(s); s != "" {
				queries = append(queries, s)
			}
		}
	}
	if len(queries) == 0 {
		return ErrorResult("queries contained no non-empty strings")
	}
	truncated := false
	if len(queries) > maxBatchQueries {
		queries = queries[:maxBatchQueries]
		truncated = true
	}

	count := batchDefaultCount
	if c, ok := args["count"].(float64); ok && int(c) >= 1 && int(c) <= maxSearchCount {
		count = int(c)
	}

	chain := ResolveWebSearchChain(ctx, t.providers)
	channel := ToolChannelFromCtx(ctx)

	formatted := make([]string, len(queries))
	sem := make(chan struct{}, batchSearchConcurrency)
	var wg sync.WaitGroup
	for i, q := range queries {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, q string) {
			defer wg.Done()
			defer func() { <-sem }()
			formatted[i] = t.searchOne(ctx, chain, channel, q, count)
		}(i, q)
	}
	wg.Wait()

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Batch web search — %d queries (concurrent):\n\n", len(queries)))
	for i, q := range queries {
		sb.WriteString(fmt.Sprintf("=== [%d] %s ===\n%s\n", i+1, q, formatted[i]))
	}
	if truncated {
		sb.WriteString(fmt.Sprintf("\n(note: only the first %d queries ran — split the rest into another batch_web_search call)\n", maxBatchQueries))
	}
	return NewResult(wrapExternalContent(sb.String(), "Web Search", false))
}

// searchOne runs one query through the provider chain (first success wins),
// using the batch tool's own cache. Errors are returned inline so one bad query
// never fails the whole batch.
func (t *BatchWebSearchTool) searchOne(ctx context.Context, chain []SearchProvider, channel, query string, count int) string {
	params := searchParams{Query: query, Count: count}
	cacheKey := fmt.Sprintf("batch:%s:%s", channel, buildSearchCacheKey(params))
	if cached, ok := t.cache.get(cacheKey); ok {
		return cached
	}
	var lastErr error
	for _, provider := range chain {
		results, err := provider.Search(ctx, params)
		if err != nil {
			lastErr = err
			continue
		}
		out := formatSearchResults(query, results, provider.Name())
		t.cache.set(cacheKey, out)
		return out
	}
	if lastErr != nil {
		return fmt.Sprintf("(search failed: %v)", lastErr)
	}
	return "(no results)"
}
