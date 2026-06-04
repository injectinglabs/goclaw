package http

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/skills"
)

// MarketplaceSkillEntry is one entry in a marketplace index response.
type MarketplaceSkillEntry struct {
	Slug        string   `json:"slug"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Source      string   `json:"source"`
	Tags        []string `json:"tags,omitempty"`
	Verified    bool     `json:"verified"`
}

// MarketplaceIndexResponse is the body returned by GET /v1/skills/marketplace.
type MarketplaceIndexResponse struct {
	URL         string                  `json:"url"`
	Name        string                  `json:"name,omitempty"`
	Description string                  `json:"description,omitempty"`
	Skills      []MarketplaceSkillEntry `json:"skills"`
	FetchedAt   time.Time               `json:"fetched_at"`
	Cached      bool                    `json:"cached,omitempty"`
}

// MarketplaceEntry is a known marketplace listed by GET /v1/skills/marketplaces.
type MarketplaceEntry struct {
	URL         string `json:"url"`
	Name        string `json:"name"`
	TrustLevel  string `json:"trust_level"`
	Description string `json:"description,omitempty"`
}

// defaultMarketplaces is the hardcoded list returned by GET /v1/skills/marketplaces.
// A future migration can move this into a skill_registries table without
// changing the HTTP shape.
var defaultMarketplaces = []MarketplaceEntry{
	{
		URL:         "https://raw.githubusercontent.com/anthropics/skills/main/.claude-plugin/marketplace.json",
		Name:        "Anthropic Skills",
		TrustLevel:  "community",
		Description: "Official Anthropic skill library",
	},
	{
		URL:         "https://raw.githubusercontent.com/injectinglabs/skills-hub/main/index.json",
		Name:        "Injecting AI Skills Hub",
		TrustLevel:  "verified",
		Description: "Skills curated by injecting.ai",
	},
}

// Limits enforced on remote marketplace fetches.
const (
	marketplaceMaxBytes   = 1 << 20 // 1 MB
	marketplaceTimeout    = 10 * time.Second
	marketplaceCacheTTL   = 5 * time.Minute
	marketplaceJSONHeader = "application/json"
)

// marketplaceCacheEntry holds a cached parsed marketplace + expiry.
type marketplaceCacheEntry struct {
	value  MarketplaceIndexResponse
	expiry time.Time
}

// marketplaceCache is process-wide. sync.Map suffices since reads dominate and
// the keyspace (URLs) is small.
var marketplaceCache sync.Map

// rawMarketplaceIndex matches our own index.json schema.
type rawMarketplaceIndex struct {
	Name        string                  `json:"name"`
	Description string                  `json:"description"`
	Skills      []MarketplaceSkillEntry `json:"skills"`
}

// rawAnthropicMarketplace matches the Anthropic Claude Code marketplace.json schema.
type rawAnthropicMarketplace struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Plugins     []struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Source      any      `json:"source"`
		Tags        []string `json:"tags"`
	} `json:"plugins"`
}

// handleMarketplaceFetch parses ?source=<url>, validates the host allowlist,
// downloads + parses the JSON, caches the result for 5 minutes, and returns
// the normalised marketplace index.
func (h *SkillsHandler) handleMarketplaceFetch(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(r.URL.Query().Get("source"))
	if raw == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "source query param required"})
		return
	}

	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "source must be a https:// URL"})
		return
	}
	if !skills.IsHostAllowed(u.Hostname()) {
		slog.Warn("security.skills.marketplace_host_blocked",
			"host", u.Hostname(), "user", r.RemoteAddr)
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("host not in allowlist: %s", u.Hostname()),
		})
		return
	}

	// Cache check.
	if cached, ok := marketplaceCache.Load(raw); ok {
		entry := cached.(marketplaceCacheEntry)
		if time.Now().Before(entry.expiry) {
			resp := entry.value
			resp.Cached = true
			writeJSON(w, http.StatusOK, resp)
			return
		}
		// Expired — fall through to refetch.
		marketplaceCache.Delete(raw)
	}

	resp, err := fetchAndParseMarketplace(r.Context(), raw)
	if err != nil {
		slog.Warn("skills.marketplace.fetch_failed", "url", raw, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	marketplaceCache.Store(raw, marketplaceCacheEntry{
		value:  resp,
		expiry: time.Now().Add(marketplaceCacheTTL),
	})

	writeJSON(w, http.StatusOK, resp)
}

// handleMarketplacesList returns the hardcoded default registry list.
func (h *SkillsHandler) handleMarketplacesList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"marketplaces": defaultMarketplaces})
}

// fetchAndParseMarketplace performs the HTTP GET, content-type check, body
// read (max 1 MB), and attempts both supported JSON shapes.
func fetchAndParseMarketplace(parentCtx context.Context, rawURL string) (MarketplaceIndexResponse, error) {
	ctx, cancel := context.WithTimeout(parentCtx, marketplaceTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return MarketplaceIndexResponse{}, fmt.Errorf("marketplace: build request: %w", err)
	}
	req.Header.Set("Accept", marketplaceJSONHeader)

	client := &http.Client{Timeout: marketplaceTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return MarketplaceIndexResponse{}, fmt.Errorf("marketplace: fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return MarketplaceIndexResponse{}, fmt.Errorf("marketplace: upstream status %d", resp.StatusCode)
	}

	ct := strings.ToLower(strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0]))
	if ct != "" && ct != marketplaceJSONHeader && ct != "text/json" {
		return MarketplaceIndexResponse{}, fmt.Errorf("marketplace: unexpected content-type %q", ct)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, marketplaceMaxBytes+1))
	if err != nil {
		return MarketplaceIndexResponse{}, fmt.Errorf("marketplace: read body: %w", err)
	}
	if int64(len(body)) > marketplaceMaxBytes {
		return MarketplaceIndexResponse{}, fmt.Errorf("marketplace: body exceeds 1MB limit")
	}

	parsed, err := parseMarketplaceJSON(body)
	if err != nil {
		return MarketplaceIndexResponse{}, err
	}
	parsed.URL = rawURL
	parsed.FetchedAt = time.Now().UTC()
	return parsed, nil
}

// parseMarketplaceJSON tries both supported schemas.
//
// Our format (index.json): {"name", "description", "skills":[{slug, source, ...}]}
// Anthropic format (marketplace.json): {"name", "description", "plugins":[{name, source:{type,repo}, ...}]}
func parseMarketplaceJSON(body []byte) (MarketplaceIndexResponse, error) {
	// First try "skills" shape (ours).
	var native rawMarketplaceIndex
	if err := json.Unmarshal(body, &native); err == nil && len(native.Skills) > 0 {
		return MarketplaceIndexResponse{
			Name:        native.Name,
			Description: native.Description,
			Skills:      native.Skills,
		}, nil
	}

	// Then try "plugins" shape (Anthropic).
	var anth rawAnthropicMarketplace
	if err := json.Unmarshal(body, &anth); err == nil && len(anth.Plugins) > 0 {
		translated := make([]MarketplaceSkillEntry, 0, len(anth.Plugins))
		for _, p := range anth.Plugins {
			src := extractAnthropicSource(p.Source)
			if src == "" {
				continue
			}
			slug := slugFromName(p.Name)
			translated = append(translated, MarketplaceSkillEntry{
				Slug:        slug,
				Name:        p.Name,
				Description: p.Description,
				Source:      src,
				Tags:        p.Tags,
				Verified:    false,
			})
		}
		return MarketplaceIndexResponse{
			Name:        anth.Name,
			Description: anth.Description,
			Skills:      translated,
		}, nil
	}

	return MarketplaceIndexResponse{}, fmt.Errorf("marketplace: payload does not match a supported schema (skills[] or plugins[])")
}

// extractAnthropicSource translates the Anthropic plugin source object into
// the goclaw locator string. Supports:
//
//	{"type": "github", "repo": "owner/repo"}        → github:owner/repo
//	{"type": "github", "repo": "owner/repo", "path": "subdir"} → github:owner/repo (path tracked separately)
//	string forms are passed through verbatim.
func extractAnthropicSource(raw any) string {
	switch v := raw.(type) {
	case string:
		return v
	case map[string]any:
		t, _ := v["type"].(string)
		if t != "github" {
			return ""
		}
		repo, _ := v["repo"].(string)
		if repo == "" {
			return ""
		}
		// Ignore "path" — the install pipeline pulls the repo root and
		// SKILL.md must be at archive root or a single subdir level.
		return "github:" + repo
	default:
		return ""
	}
}

// slugFromName turns "PDF Reader" into "pdf-reader".
func slugFromName(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	var out strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && out.Len() > 0 {
				out.WriteRune('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(out.String(), "-")
}
