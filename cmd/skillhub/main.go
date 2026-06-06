// Command skillhub is the thin, public, read-only catalog API behind the AOS
// Skills Hub (hub.injecting.ai). It aggregates skill registries (Git/static
// index files) into one searchable catalog tagged by trust tier. No DB, no auth
// — it's a stateless aggregator in front of public registries. Install happens
// in the AOS (the hub frontend deep-links there), not here.
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/skillhub"
)

const catalogTTL = 5 * time.Minute

// defaultRegistries: AOS official first (wins slug dedupe), Anthropic as community.
var defaultRegistries = []skillhub.Registry{
	{URL: "https://raw.githubusercontent.com/injectinglabs/aos-skills/main/index.json", Name: "AOS Skills", TrustLevel: "official"},
	{URL: "https://raw.githubusercontent.com/anthropics/skills/main/.claude-plugin/marketplace.json", Name: "Anthropic Skills", TrustLevel: "community"},
}

type server struct {
	registries []skillhub.Registry
	mu         sync.Mutex
	cache      []skillhub.Entry
	cacheExp   time.Time
}

func main() {
	port := getenv("PORT", "8088")
	regs := defaultRegistries
	if raw := os.Getenv("SKILLHUB_REGISTRIES"); raw != "" {
		var parsed []skillhub.Registry
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			slog.Error("skillhub: invalid SKILLHUB_REGISTRIES json", "err", err)
			os.Exit(1)
		}
		if len(parsed) > 0 {
			regs = parsed
		}
	}
	s := &server{registries: regs}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /catalog", s.handleCatalog)
	mux.HandleFunc("GET /search", s.handleSearch)
	mux.HandleFunc("GET /skill/{slug}", s.handleSkill)

	slog.Info("skillhub listening", "port", port, "registries", len(regs))
	if err := http.ListenAndServe(":"+port, cors(mux)); err != nil {
		slog.Error("skillhub: server error", "err", err)
		os.Exit(1)
	}
}

// catalog returns the aggregated catalog, refreshing the cache on expiry.
func (s *server) catalog(ctx context.Context) []skillhub.Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Now().Before(s.cacheExp) && s.cache != nil {
		return s.cache
	}
	entries, errs := skillhub.FetchCatalog(ctx, s.registries)
	for _, e := range errs {
		slog.Warn("skillhub: registry fetch failed", "err", e)
	}
	// Cache even partial results; a flapping registry shouldn't cause a refetch storm.
	if entries != nil {
		s.cache = entries
		s.cacheExp = time.Now().Add(catalogTTL)
	}
	return entries
}

func (s *server) handleCatalog(w http.ResponseWriter, r *http.Request) {
	entries := s.catalog(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"skills": entries, "tiers": tiers(entries)})
}

func (s *server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	results := skillhub.Search(s.catalog(r.Context()), q, limit)
	writeJSON(w, http.StatusOK, map[string]any{"skills": results})
}

func (s *server) handleSkill(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	for _, e := range s.catalog(r.Context()) {
		if e.Slug == slug {
			detail, err := skillhub.FetchDetail(r.Context(), e)
			if err != nil {
				slog.Warn("skillhub: detail fetch failed", "slug", slug, "err", err)
				writeJSON(w, http.StatusBadGateway, map[string]string{"error": "could not load skill detail"})
				return
			}
			writeJSON(w, http.StatusOK, detail)
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "skill not found"})
}

// tiers returns the distinct trust tiers present (for the frontend's tabs).
func tiers(entries []skillhub.Entry) []string {
	order := []string{"official", "verified", "community"}
	present := map[string]bool{}
	for _, e := range entries {
		present[e.Tier] = true
	}
	var out []string
	for _, t := range order {
		if present[t] {
			out = append(out, t)
		}
	}
	return out
}

// cors allows public GET access from the hub frontend (and dev). Read-only API.
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func getenv(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}
