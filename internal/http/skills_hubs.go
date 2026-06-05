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
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// HubSkillEntry is one entry in a hub index response.
type HubSkillEntry struct {
	Slug        string   `json:"slug"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Source      string   `json:"source"`
	Tags        []string `json:"tags,omitempty"`
	Verified    bool     `json:"verified"`
}

// HubIndexResponse is the body returned by GET /v1/skills/hubs/fetch.
type HubIndexResponse struct {
	URL         string          `json:"url"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Skills      []HubSkillEntry `json:"skills"`
	FetchedAt   time.Time       `json:"fetched_at"`
	Cached      bool            `json:"cached,omitempty"`
}

// Limits enforced on remote hub fetches.
const (
	hubFetchMaxBytes   = 1 << 20 // 1 MB
	hubFetchTimeout    = 10 * time.Second
	hubFetchCacheTTL   = 5 * time.Minute
	hubFetchJSONHeader = "application/json"
)

// hubFetchCacheEntry holds a cached parsed hub index + expiry.
type hubFetchCacheEntry struct {
	value  HubIndexResponse
	expiry time.Time
}

// hubFetchCache is process-wide. sync.Map suffices since reads dominate and
// the keyspace (URLs) is small.
var hubFetchCache sync.Map

// rawHubIndex matches our own index.json schema.
type rawHubIndex struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Skills      []HubSkillEntry `json:"skills"`
}

// rawAnthropicHubIndex matches the Anthropic Claude Code marketplace.json
// schema served at
// https://raw.githubusercontent.com/anthropics/skills/main/.claude-plugin/marketplace.json
//
// Each plugin entry is a *bundle* of skills, not a single skill. The plugin
// itself does not carry a github source struct; the skills array holds
// repo-relative paths like "./skills/pdf" which resolve against the hub
// URL's owner/repo/ref.
type rawAnthropicHubIndex struct {
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

// acceptedHubContentTypes lists the content-types we accept directly without
// falling back to JSON-parse. GitHub raw serves `.json` as
// `text/plain; charset=utf-8`, so a strict application/json-only check would
// reject the canonical Anthropic hub entirely.
var acceptedHubContentTypes = map[string]bool{
	"":                         true, // some self-hosted servers omit it
	"application/json":         true,
	"application/jsonl":        true, // defensive (we don't actually parse jsonl)
	"text/plain":               true, // GitHub raw, GitLab raw
	"text/json":                true,
	"application/octet-stream": true, // some CDNs
}

// handleHubFetch parses ?source=<url>, validates the host allowlist,
// downloads + parses the JSON, caches the result for 5 minutes, and returns
// the normalised hub index.
func (h *SkillsHandler) handleHubFetch(w http.ResponseWriter, r *http.Request) {
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
		slog.Warn("security.skills.hub_host_blocked",
			"host", u.Hostname(), "user", r.RemoteAddr)
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("host not in allowlist: %s", u.Hostname()),
		})
		return
	}

	if cached, ok := hubFetchCache.Load(raw); ok {
		entry := cached.(hubFetchCacheEntry)
		if time.Now().Before(entry.expiry) {
			resp := entry.value
			resp.Cached = true
			writeJSON(w, http.StatusOK, resp)
			return
		}
		hubFetchCache.Delete(raw)
	}

	resp, err := fetchAndParseHub(r.Context(), raw)
	if err != nil {
		slog.Warn("skills.hub.fetch_failed", "url", raw, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	hubFetchCache.Store(raw, hubFetchCacheEntry{
		value:  resp,
		expiry: time.Now().Add(hubFetchCacheTTL),
	})

	writeJSON(w, http.StatusOK, resp)
}

// handleHubsList returns the curated hub registry from skill_hubs. Replaces
// the previous hardcoded defaultMarketplaces slice — admins now manage rows
// via SQL (no user-facing CRUD by design).
func (h *SkillsHandler) handleHubsList(w http.ResponseWriter, r *http.Request) {
	if h.hubStore == nil {
		// Defensive: lite/desktop editions can run without the hub store.
		writeJSON(w, http.StatusOK, map[string]any{"hubs": []store.SkillHub{}})
		return
	}
	hubs, err := h.hubStore.ListEnabled(r.Context())
	if err != nil {
		slog.Warn("skills.hubs.list_failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to list hubs"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"hubs": hubs})
}

// fetchAndParseHub performs the HTTP GET, content-type check, body read
// (max 1 MB), and attempts both supported JSON shapes.
func fetchAndParseHub(parentCtx context.Context, rawURL string) (HubIndexResponse, error) {
	ctx, cancel := context.WithTimeout(parentCtx, hubFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return HubIndexResponse{}, fmt.Errorf("hub: build request: %w", err)
	}
	req.Header.Set("Accept", hubFetchJSONHeader)

	client := &http.Client{Timeout: hubFetchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return HubIndexResponse{}, fmt.Errorf("hub: fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return HubIndexResponse{}, fmt.Errorf("hub: upstream status %d", resp.StatusCode)
	}

	ct := strings.ToLower(strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0]))

	body, err := io.ReadAll(io.LimitReader(resp.Body, hubFetchMaxBytes+1))
	if err != nil {
		return HubIndexResponse{}, fmt.Errorf("hub: read body: %w", err)
	}
	if int64(len(body)) > hubFetchMaxBytes {
		return HubIndexResponse{}, fmt.Errorf("hub: body exceeds 1MB limit")
	}

	// Most upstreams (GitHub/GitLab raw, S3, self-hosted) misreport JSON files
	// as text/plain, text/json, or octet-stream. Accept the common set
	// directly; for anything else, try a best-effort JSON.Valid probe before
	// rejecting. The parser itself is the final arbiter.
	if !acceptedHubContentTypes[ct] {
		if !json.Valid(body) {
			return HubIndexResponse{}, fmt.Errorf("hub: unexpected content-type %q", ct)
		}
	}

	parsed, err := parseHubJSON(body, rawURL)
	if err != nil {
		return HubIndexResponse{}, err
	}
	parsed.URL = rawURL
	parsed.FetchedAt = time.Now().UTC()
	return parsed, nil
}

// parseHubJSON dispatches across the three real-world hub schemas. The
// shapes share the same outer envelope (name/description/plugins or
// skills), so we unmarshal once into a permissive struct and let helpers
// flatten the per-plugin variants. srcURL is required so monorepo-style
// hubs can resolve their relative `./path/to/skill` entries against the
// owner/repo/ref of the marketplace JSON itself.
//
//  1. native — our own index.json: top-level `skills` array of full entries
//     already in canonical (slug + source) form.
//
//  2. anthropic-bundle — `anthropics/skills` style: plugin.skills is a list
//     of repo-relative paths, each becoming its own HubSkillEntry.
//
//  3. community-plugin-as-skill — every other public hub seen so far. Each
//     plugin entry IS one skill. Its `source` is either:
//       - a monorepo string ("./marketing-skill", "plugins/foo"); resolves
//         against the hub URL's owner/repo/ref;
//       - an external-repo object ({source:"github", repo:"owner/name"});
//       - a canonical "github:..." locator (we pass it through).
//
// The order matters: native first (cheapest unmarshal), then plugins-based
// schemas. We only fall back to community when no plugin had a `skills`
// array — otherwise an Anthropic-style hub with a single empty plugin would
// be misclassified.
func parseHubJSON(body []byte, srcURL string) (HubIndexResponse, error) {
	var native rawHubIndex
	if err := json.Unmarshal(body, &native); err == nil && len(native.Skills) > 0 {
		return HubIndexResponse{
			Name:        native.Name,
			Description: native.Description,
			Skills:      native.Skills,
		}, nil
	}

	var anth rawAnthropicHubIndex
	if err := json.Unmarshal(body, &anth); err != nil || len(anth.Plugins) == 0 {
		return HubIndexResponse{}, fmt.Errorf("hub: payload does not match a supported schema (skills[] or plugins[])")
	}

	owner, repo, ref, basePath, urlErr := skills.ParseMarketplaceURL(srcURL)
	if urlErr != nil {
		return HubIndexResponse{}, fmt.Errorf("hub: cannot derive github source from URL: %w", urlErr)
	}
	description := anth.Description
	if description == "" {
		description = anth.Metadata.Description
	}

	flattened := make([]HubSkillEntry, 0)
	anyPluginHadSkills := false
	for _, p := range anth.Plugins {
		if len(p.Skills) == 0 {
			continue
		}
		anyPluginHadSkills = true
		for _, rel := range p.Skills {
			entry, ok := buildAnthropicHubSkillEntry(p.Name, p.Description, p.Tags, rel, owner, repo, ref, basePath)
			if !ok {
				continue
			}
			flattened = append(flattened, entry)
		}
	}
	// Anthropic-bundle path: at least one plugin had a skills[] array.
	if anyPluginHadSkills {
		return HubIndexResponse{Name: anth.Name, Description: description, Skills: flattened}, nil
	}

	// Community plugin-as-skill: each plugin contributes exactly one entry.
	for _, p := range anth.Plugins {
		entry, ok := buildCommunityHubSkillEntry(p.Name, p.Description, p.Tags, p.Source, owner, repo, ref)
		if !ok {
			continue
		}
		flattened = append(flattened, entry)
	}
	return HubIndexResponse{Name: anth.Name, Description: description, Skills: flattened}, nil
}

// buildCommunityHubSkillEntry turns one community-schema plugin into a
// HubSkillEntry. Source can be a string (monorepo relative path OR full
// "github:" locator) or an object {source:"github", repo:"owner/name"}.
// hubOwner/hubRepo/hubRef come from the marketplace URL itself — used to
// resolve relative monorepo paths into stable canonical sources.
func buildCommunityHubSkillEntry(pluginName, pluginDescription string, pluginTags []string, rawSource any, hubOwner, hubRepo, hubRef string) (HubSkillEntry, bool) {
	slug := slugFromName(pluginName)
	if slug == "" {
		return HubSkillEntry{}, false
	}
	source, ok := resolveCommunitySource(rawSource, hubOwner, hubRepo, hubRef)
	if !ok {
		return HubSkillEntry{}, false
	}
	tags := pluginTags
	if tags == nil {
		tags = []string{}
	}
	return HubSkillEntry{
		Slug:        slug,
		Name:        humanizeSlug(slug),
		Description: pluginDescription,
		Source:      source,
		Tags:        tags,
		Verified:    false,
	}, true
}

// resolveCommunitySource normalises every community `source` shape into a
// canonical "github:owner/repo[/path]@ref" locator the install pipeline
// understands. Returns ok=false when the source is unrecognised — those
// plugins are skipped rather than emitting a broken Install button.
func resolveCommunitySource(raw any, hubOwner, hubRepo, hubRef string) (string, bool) {
	switch s := raw.(type) {
	case string:
		s = strings.TrimSpace(s)
		if s == "" {
			return "", false
		}
		// Already canonical.
		if strings.HasPrefix(s, "github:") {
			return s, true
		}
		// Monorepo relative path → resolve against hub URL.
		cleaned := strings.TrimPrefix(s, "./")
		cleaned = strings.Trim(cleaned, "/")
		if cleaned == "" || strings.Contains(cleaned, "..") || hubOwner == "" || hubRepo == "" {
			return "", false
		}
		ref := hubRef
		if ref == "" {
			ref = "main"
		}
		return fmt.Sprintf("github:%s/%s/%s@%s", hubOwner, hubRepo, cleaned, ref), true
	case map[string]any:
		// External-repo object: {source:"github", repo:"owner/name", tag/version?}
		kind, _ := s["source"].(string)
		repoPath, _ := s["repo"].(string)
		repoPath = strings.Trim(strings.TrimSpace(repoPath), "/")
		if !strings.EqualFold(kind, "github") || repoPath == "" || !strings.Contains(repoPath, "/") {
			return "", false
		}
		ref := stringFromAny(s["tag"])
		if ref == "" {
			ref = stringFromAny(s["version"])
		}
		if ref == "" {
			ref = stringFromAny(s["branch"])
		}
		if ref == "" {
			ref = "main"
		}
		return fmt.Sprintf("github:%s@%s", repoPath, ref), true
	}
	return "", false
}

// stringFromAny is a defensive type-assertion helper for the loose
// map[string]any returned by encoding/json on optional fields.
func stringFromAny(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

// buildAnthropicHubSkillEntry turns a single plugin.skills[] entry (a
// repo-relative path like "./skills/pdf") into a flat HubSkillEntry keyed
// at owner/repo/path@ref. pluginDescription is reused as the per-skill
// description because the Anthropic schema does not surface one per skill.
//
// basePath (the dir holding marketplace.json) is accepted for forward
// compatibility but not consumed here: real-world Anthropic paths start
// with a leading `./` and are repo-root anchored.
func buildAnthropicHubSkillEntry(pluginName, pluginDescription string, pluginTags []string, rel, owner, repo, ref, basePath string) (HubSkillEntry, bool) {
	_ = basePath
	cleaned := strings.TrimSpace(rel)
	cleaned = strings.TrimPrefix(cleaned, "./")
	cleaned = strings.Trim(cleaned, "/")
	if cleaned == "" || strings.Contains(cleaned, "..") {
		return HubSkillEntry{}, false
	}
	segments := strings.Split(cleaned, "/")
	last := segments[len(segments)-1]
	slug := slugFromName(last)
	if slug == "" {
		slug = slugFromName(pluginName + "-" + last)
	}
	if slug == "" {
		return HubSkillEntry{}, false
	}
	source := fmt.Sprintf("github:%s/%s/%s@%s", owner, repo, cleaned, ref)
	tags := pluginTags
	if tags == nil {
		tags = []string{}
	}
	return HubSkillEntry{
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
