package store

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// SkillHub is one row in the skill_hubs registry — a curated source URL
// (typically a GitHub-hosted marketplace.json / index.json) that the
// install flow can browse for installable skills.
type SkillHub struct {
	ID          uuid.UUID `json:"id" db:"id"`
	URL         string    `json:"url" db:"url"`
	Name        string    `json:"name" db:"name"`
	Description string    `json:"description,omitempty" db:"description"`
	TrustLevel  string    `json:"trust_level" db:"trust_level"`
	Enabled     bool      `json:"enabled" db:"enabled"`
	CreatedAt   time.Time `json:"created_at" db:"created_at"`
	UpdatedAt   time.Time `json:"updated_at" db:"updated_at"`
}

// SkillHubStore reads the curated hub list. There is no user-facing CRUD —
// rows are admin-managed via SQL (or future internal tooling). Listing is
// the only hot path so the interface stays narrow.
type SkillHubStore interface {
	// ListEnabled returns every row with enabled=true, ordered by name.
	// Used by GET /v1/skills/hubs.
	ListEnabled(ctx context.Context) ([]SkillHub, error)
}
