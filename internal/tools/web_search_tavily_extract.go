package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	tavilyExtractEndpoint = "https://api.tavily.com/extract"
	// tavilyExtractMaxURLs is Tavily's per-request URL cap for /extract.
	tavilyExtractMaxURLs = 20
	// extractTimeoutSeconds allows for advanced extraction latency.
	extractTimeoutSeconds = 45
)

// tavilyExtractor pulls FULL page content (not snippets) for a batch of URLs via
// Tavily's /extract endpoint. This is what makes search-based sheet research
// accurate: emails and details live in page bodies (contact/about/team pages),
// not in search-result snippets.
type tavilyExtractor struct {
	apiKey string
	client *http.Client
}

func newTavilyExtractor(apiKey string) *tavilyExtractor {
	return &tavilyExtractor{
		apiKey: apiKey,
		client: &http.Client{Timeout: time.Duration(extractTimeoutSeconds) * time.Second},
	}
}

// Extract returns url -> full page text for the given URLs (max 20 per call).
// Failed/empty URLs are simply omitted from the map.
func (e *tavilyExtractor) Extract(ctx context.Context, urls []string) (map[string]string, error) {
	if e == nil || e.apiKey == "" {
		return nil, fmt.Errorf("tavily extract: no api key configured")
	}
	if len(urls) == 0 {
		return map[string]string{}, nil
	}
	if len(urls) > tavilyExtractMaxURLs {
		urls = urls[:tavilyExtractMaxURLs]
	}

	reqBody, err := json.Marshal(map[string]any{
		"urls":          urls,
		"extract_depth": "basic",
		"format":        "text",
	})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tavilyExtractEndpoint, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", webSearchUserAgent)

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tavily extract returned %d: %s", resp.StatusCode, truncateStr(string(body), 200))
	}

	var out struct {
		Results []struct {
			URL        string `json:"url"`
			RawContent string `json:"raw_content"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	m := make(map[string]string, len(out.Results))
	for _, r := range out.Results {
		if r.RawContent != "" {
			m[r.URL] = r.RawContent
		}
	}
	return m, nil
}
