package pg

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// SetUserDisable records that a caller wants this skill hidden from their
// catalog + agent. Idempotent — re-disable is a no-op, re-enable then
// re-disable round-trips cleanly. Returns no error when the skill row is
// gone (DELETE CASCADE will clean up shortly anyway).
func (s *PGSkillStore) SetUserDisable(ctx context.Context, skillID uuid.UUID, userID string) error {
	if err := store.ValidateUserID(userID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO skill_user_disables (id, skill_id, user_id, tenant_id, created_at)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (skill_id, user_id) DO NOTHING`,
		store.GenNewID(), skillID, userID, tenantIDForInsert(ctx), time.Now())
	return err
}

// ClearUserDisable removes the per-user disable record so the shared
// skill becomes visible + agent-accessible again. No-op if no record
// exists; the caller can use this to "ensure enabled" without checking.
func (s *PGSkillStore) ClearUserDisable(ctx context.Context, skillID uuid.UUID, userID string) error {
	if err := store.ValidateUserID(userID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM skill_user_disables WHERE skill_id = $1 AND user_id = $2`,
		skillID, userID)
	return err
}

// IsDisabledForUser reports whether the caller has a personal disable
// record for this skill. Used by handlers that need to surface the
// per-user state separately from the canonical row's enabled flag.
func (s *PGSkillStore) IsDisabledForUser(ctx context.Context, skillID uuid.UUID, userID string) bool {
	var n int
	_ = s.db.QueryRowContext(ctx,
		`SELECT 1 FROM skill_user_disables WHERE skill_id = $1 AND user_id = $2 LIMIT 1`,
		skillID, userID).Scan(&n)
	return n == 1
}

// SetUserDisableBySlug records disable for EVERY skill row the caller
// can access with the given slug in their tenant — their own row (any
// visibility) plus rows shared by other users (visibility=public).
// Mirrors the cascade-by-slug semantic the SPA toggle uses: one click
// means "this skill is off for me", regardless of which row is
// currently surfaced by DISTINCT ON dedup.
//
// Caller must be authenticated (UserIDFromContext) — we use the tenant
// from context to scope the slug.
func (s *PGSkillStore) SetUserDisableBySlug(ctx context.Context, slug string) (int, error) {
	userID := store.UserIDFromContext(ctx)
	if userID == "" {
		return 0, nil
	}
	tid := tenantIDForInsert(ctx)
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO skill_user_disables (id, skill_id, user_id, tenant_id, created_at)
		 SELECT gen_random_uuid(), s.id, $1, s.tenant_id, NOW()
		   FROM skills s
		  WHERE s.tenant_id = $2 AND s.slug = $3 AND s.status != 'deleted'
		    AND (s.owner_id = $1 OR s.visibility = 'public')
		 ON CONFLICT (skill_id, user_id) DO NOTHING`,
		userID, tid, slug)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ClearUserDisableBySlug is the inverse: drop the caller's disable
// records for every row of this slug, re-enabling the skill end-to-end.
func (s *PGSkillStore) ClearUserDisableBySlug(ctx context.Context, slug string) (int, error) {
	userID := store.UserIDFromContext(ctx)
	if userID == "" {
		return 0, nil
	}
	tid := tenantIDForInsert(ctx)
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM skill_user_disables d
		 USING skills s
		 WHERE d.skill_id = s.id
		   AND d.user_id = $1
		   AND s.tenant_id = $2 AND s.slug = $3`,
		userID, tid, slug)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// IsSlugDisabledForUser is the read complement — used by ListSkillsForUser
// to surface the per-user state for the dedup'd row regardless of which
// specific row id won the DISTINCT ON tiebreaker.
func (s *PGSkillStore) IsSlugDisabledForUser(ctx context.Context, slug, userID string) bool {
	tid := store.TenantIDFromContext(ctx)
	var n int
	_ = s.db.QueryRowContext(ctx,
		`SELECT 1 FROM skill_user_disables d
		 JOIN skills s ON s.id = d.skill_id
		 WHERE d.user_id = $1 AND s.tenant_id = $2 AND s.slug = $3 LIMIT 1`,
		userID, tid, slug).Scan(&n)
	return n == 1
}
