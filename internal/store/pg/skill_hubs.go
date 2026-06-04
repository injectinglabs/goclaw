package pg

import (
	"context"
	"database/sql"
	"sync"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// PGSkillHubStore is the Postgres-backed implementation of SkillHubStore.
//
// The hub list is global (no tenant column), admin-managed, and rarely
// changes — so the store keeps a short in-process cache to avoid hitting
// the DB on every GET /v1/skills/hubs (which the SPA calls on every
// Browse tab open). Invalidation: TTL only; admins editing rows via SQL
// will see the new state after `skillHubsCacheTTL`. That's intentional
// — no user-facing CRUD means there's no event to hook on.
type PGSkillHubStore struct {
	db *sql.DB

	mu       sync.RWMutex
	cache    []store.SkillHub
	cachedAt time.Time
}

const skillHubsCacheTTL = 60 * time.Second

func NewPGSkillHubStore(db *sql.DB) *PGSkillHubStore {
	return &PGSkillHubStore{db: db}
}

// ListEnabled implements SkillHubStore.
func (s *PGSkillHubStore) ListEnabled(ctx context.Context) ([]store.SkillHub, error) {
	s.mu.RLock()
	if !s.cachedAt.IsZero() && time.Since(s.cachedAt) < skillHubsCacheTTL {
		out := s.cache
		s.mu.RUnlock()
		return out, nil
	}
	s.mu.RUnlock()

	var rows []store.SkillHub
	// COALESCE description → '' so scanning into a non-pointer string works.
	// The DB allows NULL description; we just treat it as empty on read.
	if err := pkgSqlxDB.SelectContext(ctx, &rows,
		`SELECT id, url, name, COALESCE(description, '') AS description,
		        trust_level, enabled, created_at, updated_at
		 FROM skill_hubs WHERE enabled = true ORDER BY name`); err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.cache = rows
	s.cachedAt = time.Now()
	s.mu.Unlock()

	return rows, nil
}
