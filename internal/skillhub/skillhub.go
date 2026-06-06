// Package skillhub is a thin, read-only catalog aggregator for the AOS Skills
// Hub. It fetches one or more skill *registries* (Git/static index files,
// agentskills.io-style), normalizes them into a single catalog tagged by trust
// tier, supports BM25 search, and resolves per-skill detail (SKILL.md). It
// reuses goclaw's skill code: BM25 search, the host allowlist, source parsing,
// and the SKILL.md frontmatter parser — all from internal/skills.
//
// It is deliberately stateless and dependency-light (no DB, no auth) so it can
// run as a small standalone service (cmd/skillhub) behind a public catalog API.
package skillhub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/skills"
)

const (
	fetchMaxBytes = 1 << 20 // 1 MiB
	fetchTimeout  = 10 * time.Second
)

// Registry is a configured skill source (a registry index URL + trust tier).
type Registry struct {
	URL        string `json:"url"`
	Name       string `json:"name"`
	TrustLevel string `json:"trust_level"` // official | verified | community
}

// Entry is one skill in the aggregated catalog.
type Entry struct {
	Slug        string   `json:"slug"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Source      string   `json:"source"` // canonical github:owner/repo/path@ref locator
	Tags        []string `json:"tags,omitempty"`
	Verified    bool     `json:"verified"`
	Tier        string   `json:"tier"`     // from the registry's trust_level
	HubName     string   `json:"hub_name"` // registry display name
}

// Detail is an Entry plus its full SKILL.md content.
type Detail struct {
	Entry
	Content string `json:"content"`
}

var httpClient = &http.Client{Timeout: fetchTimeout}

// --- registry index schemas ---

type nativeIndex struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Skills      []Entry `json:"skills"`
}

type anthropicIndex struct {
	Name    string `json:"name"`
	Plugins []struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Tags        []string `json:"tags"`
		Skills      []string `json:"skills"` // repo-relative paths like "./skills/pdf"
	} `json:"plugins"`
}

// FetchCatalog fetches every registry and returns a single catalog. Each entry
// is tagged with its registry's trust tier + name. On a per-registry failure it
// logs (via the returned error list) and continues, so one dead registry never
// blanks the whole catalog. Slugs are de-duplicated, first registry wins
// (callers should list higher-trust registries first).
func FetchCatalog(ctx context.Context, registries []Registry) ([]Entry, []error) {
	var out []Entry
	var errs []error
	seen := map[string]bool{}
	for _, reg := range registries {
		entries, err := fetchRegistry(ctx, reg)
		if err != nil {
			errs = append(errs, fmt.Errorf("registry %q (%s): %w", reg.Name, reg.URL, err))
			continue
		}
		for _, e := range entries {
			if e.Slug == "" || seen[e.Slug] {
				continue
			}
			seen[e.Slug] = true
			e.Tier = reg.TrustLevel
			e.HubName = reg.Name
			out = append(out, e)
		}
	}
	return out, errs
}

func fetchRegistry(ctx context.Context, reg Registry) ([]Entry, error) {
	u, err := url.Parse(reg.URL)
	if err != nil {
		return nil, fmt.Errorf("bad url: %w", err)
	}
	if !skills.IsHostAllowed(u.Hostname()) {
		return nil, fmt.Errorf("host %q not allowed", u.Hostname())
	}
	body, err := fetchBytes(ctx, reg.URL)
	if err != nil {
		return nil, err
	}
	return parseIndex(body, reg.URL)
}

// parseIndex supports our native schema and the Anthropic marketplace.json
// schema (plugins[].skills[] of repo-relative paths resolved against the hub URL).
func parseIndex(body []byte, hubURL string) ([]Entry, error) {
	var native nativeIndex
	if err := json.Unmarshal(body, &native); err == nil && len(native.Skills) > 0 {
		return native.Skills, nil
	}
	var anth anthropicIndex
	if err := json.Unmarshal(body, &anth); err == nil && len(anth.Plugins) > 0 {
		owner, repo, ref, ok := githubCoordsFromRawURL(hubURL)
		if !ok {
			return nil, fmt.Errorf("cannot derive github owner/repo/ref from hub url")
		}
		var entries []Entry
		for _, p := range anth.Plugins {
			for _, rel := range p.Skills {
				path := strings.TrimPrefix(strings.TrimPrefix(rel, "./"), "/")
				slug := path
				if i := strings.LastIndex(path, "/"); i >= 0 {
					slug = path[i+1:]
				}
				entries = append(entries, Entry{
					Slug:        slug,
					Name:        plugTitle(p.Name, slug),
					Description: p.Description,
					Source:      fmt.Sprintf("github:%s/%s/%s@%s", owner, repo, path, ref),
					Tags:        p.Tags,
				})
			}
		}
		return entries, nil
	}
	return nil, fmt.Errorf("payload matched no supported registry schema (skills[] or plugins[])")
}

// githubCoordsFromRawURL extracts owner/repo/ref from a raw.githubusercontent.com URL:
// https://raw.githubusercontent.com/<owner>/<repo>/<ref>/<path...>
func githubCoordsFromRawURL(rawURL string) (owner, repo, ref string, ok bool) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", "", false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if u.Hostname() == "raw.githubusercontent.com" && len(parts) >= 3 {
		return parts[0], parts[1], parts[2], true
	}
	return "", "", "", false
}

func plugTitle(name, slug string) string {
	if name != "" {
		return name
	}
	return slug
}

// Search ranks entries by a BM25 query over name/description (reusing
// internal/skills' index), returning matching entries in score order.
func Search(entries []Entry, query string, maxResults int) []Entry {
	if strings.TrimSpace(query) == "" {
		if maxResults > 0 && len(entries) > maxResults {
			return entries[:maxResults]
		}
		return entries
	}
	idx := skills.NewIndex()
	infos := make([]skills.Info, 0, len(entries))
	bySlug := make(map[string]Entry, len(entries))
	for _, e := range entries {
		infos = append(infos, skills.Info{Name: e.Name, Slug: e.Slug, Description: e.Description})
		bySlug[e.Slug] = e
	}
	idx.Build(infos)
	results := idx.Search(query, maxResults)
	out := make([]Entry, 0, len(results))
	for _, r := range results {
		if e, ok := bySlug[r.Slug]; ok {
			out = append(out, e)
		}
	}
	return out
}

// FetchDetail resolves the SKILL.md for an entry from its source locator.
func FetchDetail(ctx context.Context, e Entry) (Detail, error) {
	src, err := skills.ParseSource(e.Source)
	if err != nil {
		return Detail{}, fmt.Errorf("parse source: %w", err)
	}
	if src.Type != "github" {
		return Detail{}, fmt.Errorf("unsupported source type %q", src.Type)
	}
	ref := src.Ref
	if ref == "" {
		ref = "main"
	}
	rawURL := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/%s/SKILL.md",
		src.Owner, src.Repo, ref, strings.Trim(src.Path, "/"))
	body, err := fetchBytes(ctx, rawURL)
	if err != nil {
		return Detail{}, err
	}
	content := string(body)
	name, desc, _, _ := skills.ParseSkillFrontmatter(content)
	if name != "" {
		e.Name = name
	}
	if desc != "" {
		e.Description = desc
	}
	return Detail{Entry: e, Content: content}, nil
}

func fetchBytes(ctx context.Context, rawURL string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("upstream status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, fetchMaxBytes))
}
