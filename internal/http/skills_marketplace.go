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

// rawAnthropicMarketplace matches the Anthropic Claude Code marketplace.json
// schema as actually served at
// https://raw.githubusercontent.com/anthropics/skills/main/.claude-plugin/marketplace.json
//
// Each plugin entry is a *bundle* of skills, not a single skill. The plugin
// itself does not carry a github source struct; the skills array holds
// repo-relative paths like "./skills/pdf" which resolve against the
// marketplace URL's owner/repo/ref.
type rawAnthropicMarketplace struct {
	Name     string `json:"name"`
	Metadata struct {
		Description string `json:"description"`
		Version     string `json:"version"`
	} `json:"metadata"`
	Description string `json:"description"` // legacy/top-level, falls back to metadata.description
	Plugins     []struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Source      any      `json:"source"`
		Tags        []string `json:"tags"`
		Skills      []string `json:"skills"`
	} `json:"plugins"`
}

// acceptedMarketplaceContentTypes lists the content-types we accept directly
// without falling back to JSON-parse. GitHub raw serves `.json` as
// `text/plain; charset=utf-8`, so the historical strict check rejected the
// canonical Anthropic marketplace entirely.
var acceptedMarketplaceContentTypes = map[string]bool{
	"":                         true, // some self-hosted servers omit it
	"application/json":         true,
	"application/jsonl":        true, // defensive (we don't actually parse jsonl)
	"text/plain":               true, // GitHub raw, GitLab raw
	"text/json":                true,
	"application/octet-stream": true, // some CDNs
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

	body, err := io.ReadAll(io.LimitReader(resp.Body, marketplaceMaxBytes+1))
	if err != nil {
		return MarketplaceIndexResponse{}, fmt.Errorf("marketplace: read body: %w", err)
	}
	if int64(len(body)) > marketplaceMaxBytes {
		return MarketplaceIndexResponse{}, fmt.Errorf("marketplace: body exceeds 1MB limit")
	}

	// Most upstreams (GitHub/GitLab raw, S3, self-hosted) misreport JSON files
	// as text/plain, text/json, or octet-stream. Accept the common set
	// directly; for anything else, try a best-effort JSON.Valid probe before
	// rejecting. The parser itself is the final arbiter.
	if !acceptedMarketplaceContentTypes[ct] {
		if !json.Valid(body) {
			return MarketplaceIndexResponse{}, fmt.Errorf("marketplace: unexpected content-type %q", ct)
		}
	}

	parsed, err := parseMarketplaceJSON(body, rawURL)
	if err != nil {
		return MarketplaceIndexResponse{}, err
	}
	parsed.URL = rawURL
	parsed.FetchedAt = time.Now().UTC()
	return parsed, nil
}

// parseMarketplaceJSON tries both supported schemas.
//
// Our format (index.json):
//
//	{"name", "description", "skills":[{slug, source, ...}]}
//
// Anthropic format (marketplace.json), as actually served:
//
//	{
//	  "name": "...",
//	  "metadata": {"description": "...", "version": "..."},
//	  "plugins": [
//	    {"name": "...", "description": "...", "skills": ["./skills/pdf", ...]},
//	    ...
//	  ]
//	}
//
// Each plugin is a bundle; each entry in plugin.skills[] is a repo-relative
// path that we flatten into its own MarketplaceSkillEntry. The skill's
// install source is derived from the marketplace URL itself
// (raw.githubusercontent.com/{owner}/{repo}/{ref}/...), so srcURL must be
// the same string the handler is going to cache against.
func parseMarketplaceJSON(body []byte, srcURL string) (MarketplaceIndexResponse, error) {
	// First try "skills" shape (ours). No URL context needed — sources are
	// already canonical locator strings.
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
		owner, repo, ref, basePath, urlErr := skills.ParseMarketplaceURL(srcURL)
		if urlErr != nil {
			return MarketplaceIndexResponse{}, fmt.Errorf("marketplace: cannot derive github source from URL: %w", urlErr)
		}
		description := anth.Description
		if description == "" {
			description = anth.Metadata.Description
		}

		flattened := make([]MarketplaceSkillEntry, 0)
		for _, p := range anth.Plugins {
			if len(p.Skills) == 0 {
				continue
			}
			for _, rel := range p.Skills {
				entry, ok := buildAnthropicSkillEntry(p.Name, p.Description, p.Tags, rel, owner, repo, ref, basePath)
				if !ok {
					continue
				}
				flattened = append(flattened, entry)
			}
		}
		return MarketplaceIndexResponse{
			Name:        anth.Name,
			Description: description,
			Skills:      flattened,
		}, nil
	}

	return MarketplaceIndexResponse{}, fmt.Errorf("marketplace: payload does not match a supported schema (skills[] or plugins[])")
}

// buildAnthropicSkillEntry turns a single plugin.skills[] entry (a
// repo-relative path like "./skills/pdf") into a flat MarketplaceSkillEntry
// keyed at owner/repo/path@ref. pluginDescription is reused as the per-skill
// description because the Anthropic schema does not surface one per skill.
//
// basePath (the dir holding marketplace.json) is accepted for forward
// compatibility but not consumed here: real-world Anthropic paths start with
// a leading `./` and are repo-root anchored. Schemas that publish basePath-
// relative entries would need a different convention to disambiguate.
func buildAnthropicSkillEntry(pluginName, pluginDescription string, pluginTags []string, rel, owner, repo, ref, basePath string) (MarketplaceSkillEntry, bool) {
	_ = basePath
	cleaned := strings.TrimSpace(rel)
	cleaned = strings.TrimPrefix(cleaned, "./")
	cleaned = strings.Trim(cleaned, "/")
	if cleaned == "" || strings.Contains(cleaned, "..") {
		return MarketplaceSkillEntry{}, false
	}
	segments := strings.Split(cleaned, "/")
	last := segments[len(segments)-1]
	slug := slugFromName(last)
	if slug == "" {
		slug = slugFromName(pluginName + "-" + last)
	}
	if slug == "" {
		return MarketplaceSkillEntry{}, false
	}
	source := fmt.Sprintf("github:%s/%s/%s@%s", owner, repo, cleaned, ref)
	tags := pluginTags
	if tags == nil {
		tags = []string{}
	}
	return MarketplaceSkillEntry{
		Slug:        slug,
		Name:        humanizeSlug(slug),
		Description: pluginDescription,
		Source:      source,
		Tags:        tags,
		Verified:    false,
	}, true
}

// humanizeSlug turns "skill-creator" into "Skill Creator" for display. Falls
// back to the slug itself when input is empty.
func humanizeSlug(s string) string {
	if s == "" {
		return s
	}
	parts := strings.Split(s, "-")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
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
