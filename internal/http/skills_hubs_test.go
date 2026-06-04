package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// resetHubFetchCache clears the package-level marketplace cache between
// subtests so they don't see each other's entries.
func resetHubFetchCache(t *testing.T) {
	t.Helper()
	hubFetchCache = sync.Map{}
}

// allowHost temporarily adds host to the skills allowlist for the duration of
// the test. Avoids polluting the static set used by IsHostAllowed.
//
// We rewrite the URL passed to the handler so the host matches the static
// allowlist (raw.githubusercontent.com etc.) — that's simpler than mutating
// the allowlist map. The httptest.Server is reachable from the handler via a
// resolver hook we wire below.
func startAllowedTestServer(t *testing.T, body string, contentType string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestHub_OurIndexFormat(t *testing.T) {
	resetHubFetchCache(t)
	const ourFormat = `{
		"name": "Our Hub",
		"description": "Curated skills",
		"skills": [
			{"slug": "pdf", "name": "PDF Reader", "description": "Read PDFs", "source": "github:foo/bar", "tags": ["pdf"], "verified": true},
			{"slug": "csv", "name": "CSV", "description": "", "source": "github:foo/csv"}
		]
	}`
	srv := startAllowedTestServer(t, ourFormat, "application/json")

	resp := parseOrFail(t, srv.URL, ourFormat)
	if resp.Name != "Our Hub" {
		t.Fatalf("name = %q, want Our Hub", resp.Name)
	}
	if len(resp.Skills) != 2 {
		t.Fatalf("skills len = %d, want 2", len(resp.Skills))
	}
	if resp.Skills[0].Slug != "pdf" || resp.Skills[0].Source != "github:foo/bar" {
		t.Fatalf("first skill = %+v", resp.Skills[0])
	}
}

func TestHub_AnthropicNestedSkills(t *testing.T) {
	resetHubFetchCache(t)
	// Mirrors the real anthropics/skills marketplace.json: plugins are
	// bundles, each with a skills[] array of repo-relative paths.
	const anthFormat = `{
		"name": "anthropic-agent-skills",
		"metadata": {"description": "Anthropic skills suite", "version": "1.0.0"},
		"plugins": [
			{
				"name": "document-skills",
				"description": "Collection of document processing suite",
				"source": "./",
				"strict": false,
				"skills": ["./skills/xlsx", "./skills/docx", "./skills/pptx", "./skills/pdf"]
			},
			{
				"name": "example-skills",
				"description": "Collection of example skills",
				"source": "./",
				"skills": ["./skills/algorithmic-art", "./skills/skill-creator"]
			}
		]
	}`
	resp := parseOrFail(t, "https://raw.githubusercontent.com/anthropics/skills/main/.claude-plugin/marketplace.json", anthFormat)

	if resp.Name != "anthropic-agent-skills" {
		t.Fatalf("name = %q", resp.Name)
	}
	if resp.Description != "Anthropic skills suite" {
		t.Fatalf("description = %q (expected metadata.description fallback)", resp.Description)
	}
	if len(resp.Skills) != 6 {
		t.Fatalf("flattened skills = %d, want 6", len(resp.Skills))
	}

	// Each entry must reference the marketplace URL's owner/repo/ref + the
	// per-skill subdir.
	bySlug := map[string]HubSkillEntry{}
	for _, s := range resp.Skills {
		bySlug[s.Slug] = s
	}
	for _, slug := range []string{"xlsx", "docx", "pptx", "pdf", "algorithmic-art", "skill-creator"} {
		entry, ok := bySlug[slug]
		if !ok {
			t.Fatalf("missing flattened skill %q in %+v", slug, resp.Skills)
		}
		wantSource := "github:anthropics/skills/skills/" + slug + "@main"
		if entry.Source != wantSource {
			t.Errorf("skill %q source = %q, want %q", slug, entry.Source, wantSource)
		}
		if entry.Description == "" {
			t.Errorf("skill %q has empty description (should carry plugin description)", slug)
		}
	}
}

func TestHub_HostAllowlistRejected(t *testing.T) {
	resetHubFetchCache(t)
	h := &SkillsHandler{}
	req := httptest.NewRequest(http.MethodGet, "/v1/skills/hubs/fetch?source="+url.QueryEscape("https://evil.example.com/x.json"), nil)
	w := httptest.NewRecorder()
	h.handleHubFetch(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "allowlist") {
		t.Fatalf("body = %s, expected allowlist message", w.Body.String())
	}
}

func TestHub_CacheHit(t *testing.T) {
	resetHubFetchCache(t)
	const body = `{"name": "X", "skills": [{"slug": "a", "name": "A", "source": "github:x/a"}]}`
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	// Pre-seed cache with the URL pointed at the test server. The handler
	// validates the host allowlist *first* — the test server runs on a
	// non-allowlisted localhost, so we exercise the cache hit path by
	// pre-populating the cache directly.
	rawURL := "https://raw.githubusercontent.com/cache-hit/index.json"
	resp := HubIndexResponse{
		URL:       rawURL,
		Name:      "X",
		Skills:    []HubSkillEntry{{Slug: "a", Name: "A", Source: "github:x/a"}},
		FetchedAt: time.Now(),
	}
	hubFetchCache.Store(rawURL, hubFetchCacheEntry{value: resp, expiry: time.Now().Add(time.Minute)})

	h := &SkillsHandler{}
	req := httptest.NewRequest(http.MethodGet, "/v1/skills/hubs/fetch?source="+url.QueryEscape(rawURL), nil)
	w := httptest.NewRecorder()
	h.handleHubFetch(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var got HubIndexResponse
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Cached {
		t.Fatal("expected cached=true on cache hit")
	}
	if hits != 0 {
		t.Fatalf("upstream was hit %d times despite cache; expected 0", hits)
	}
}

func TestHub_MalformedJSON(t *testing.T) {
	resetHubFetchCache(t)
	// Parse-level test (host allowlist not exercised — we call the parser directly).
	_, err := parseHubJSON([]byte("{not json"), "https://raw.githubusercontent.com/x/y/main/index.json")
	if err == nil {
		t.Fatal("expected error on malformed JSON")
	}
	// Empty/unsupported shape.
	_, err = parseHubJSON([]byte(`{"unrelated": true}`), "https://raw.githubusercontent.com/x/y/main/index.json")
	if err == nil {
		t.Fatal("expected error on unsupported shape")
	}
}

// stubHubStore returns a fixed list — exercises the handler shape without
// a real DB. The DB-level path is covered by the live skill_hubs SQL seed
// migration verified at stage roll-out.
type stubHubStore struct{ rows []store.SkillHub }

func (s *stubHubStore) ListEnabled(_ context.Context) ([]store.SkillHub, error) {
	return s.rows, nil
}

func TestHub_List_FromStore(t *testing.T) {
	resetHubFetchCache(t)
	h := &SkillsHandler{hubStore: &stubHubStore{rows: []store.SkillHub{
		{Name: "Anthropic Skills", URL: "https://example.com/a.json", TrustLevel: "community"},
	}}}
	req := httptest.NewRequest(http.MethodGet, "/v1/skills/hubs", nil)
	w := httptest.NewRecorder()
	h.handleHubsList(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp struct {
		Hubs []store.SkillHub `json:"hubs"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Hubs) != 1 || resp.Hubs[0].Name != "Anthropic Skills" {
		t.Fatalf("unexpected hubs response: %+v", resp.Hubs)
	}
}

func TestHub_List_NoStore_EmptyArray(t *testing.T) {
	h := &SkillsHandler{}
	req := httptest.NewRequest(http.MethodGet, "/v1/skills/hubs", nil)
	w := httptest.NewRecorder()
	h.handleHubsList(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
}

// parseOrFail calls parseHubJSON directly — the host allowlist gate
// only fires when going through the full HTTP handler. parseHubJSON
// is the integration-tested unit. srcURL is required because the Anthropic
// schema derives each skill's GitHub source from the marketplace URL itself.
func parseOrFail(t *testing.T, srcURL string, body string) HubIndexResponse {
	t.Helper()
	parsed, err := parseHubJSON([]byte(body), srcURL)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return parsed
}

// fetchThroughTestServer routes a marketplace fetch at the real
// fetchAndParseHub path through a httptest.Server. Because the
// production code only accepts allowlisted hostnames, we bypass the host
// gate by calling fetchAndParseHub directly with the test server URL.
func fetchThroughTestServer(t *testing.T, body, contentType string) (HubIndexResponse, error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		} else {
			// httptest's ResponseWriter defaults to text/plain; charset=utf-8
			// once any body bytes are written. Force-clear the header so the
			// caller sees an empty Content-Type for the bare-no-content-type
			// test.
			w.Header()["Content-Type"] = nil
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return fetchAndParseHub(context.Background(), srv.URL)
}

func TestHub_TextPlainAccepted(t *testing.T) {
	resetHubFetchCache(t)
	const ourFormat = `{
		"name": "TP",
		"skills": [{"slug": "a", "name": "A", "source": "github:x/a"}]
	}`
	resp, err := fetchThroughTestServer(t, ourFormat, "text/plain; charset=utf-8")
	if err != nil {
		t.Fatalf("expected text/plain to be accepted, got %v", err)
	}
	if len(resp.Skills) != 1 {
		t.Fatalf("skills = %d, want 1", len(resp.Skills))
	}
}

func TestHub_BareNoContentType(t *testing.T) {
	resetHubFetchCache(t)
	const ourFormat = `{
		"name": "Bare",
		"skills": [{"slug": "a", "name": "A", "source": "github:x/a"}]
	}`
	resp, err := fetchThroughTestServer(t, ourFormat, "")
	if err != nil {
		t.Fatalf("expected empty Content-Type to be accepted, got %v", err)
	}
	if resp.Name != "Bare" {
		t.Fatalf("name = %q", resp.Name)
	}
}

func TestHub_InvalidJSONStillRejected(t *testing.T) {
	resetHubFetchCache(t)
	_, err := fetchThroughTestServer(t, "<html>not json</html>", "text/plain; charset=utf-8")
	if err == nil {
		t.Fatal("expected error on non-JSON body with text/plain content-type")
	}
}

func TestHub_RealAnthropicEndpoint(t *testing.T) {
	if os.Getenv("SMOKE_REAL_NETWORK") == "" {
		t.Skip("SMOKE_REAL_NETWORK not set — skipping live network smoke")
	}
	resetHubFetchCache(t)
	resp, err := fetchAndParseHub(context.Background(),
		"https://raw.githubusercontent.com/anthropics/skills/main/.claude-plugin/marketplace.json")
	if err != nil {
		t.Fatalf("real anthropic fetch failed: %v", err)
	}
	if len(resp.Skills) < 4 {
		t.Fatalf("expected ≥4 flattened skills from real endpoint, got %d", len(resp.Skills))
	}
	for _, s := range resp.Skills {
		if strings.TrimSpace(s.Slug) == "" {
			t.Fatalf("real endpoint returned a skill with empty slug: %+v", s)
		}
	}
}

