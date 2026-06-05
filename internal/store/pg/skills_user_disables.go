package pg

import (
	"context"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

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
	// userID passed twice as $1 + $4 — using a single $1 in both the
	// SELECT projection and the predicate triggers Postgres
	// SQLSTATE 42P08 ("inconsistent types deduced for parameter $1")
	// because the prepared-statement type inference can't pin one type
	// across both use sites with this driver version. Two distinct
	// parameters skip the deduction step entirely. Same fix in
	// ClearUserDisableBySlug below.
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO skill_user_disables (id, skill_id, user_id, tenant_id, created_at)
		 SELECT gen_random_uuid(), s.id, $1, s.tenant_id, NOW()
		   FROM skills s
		  WHERE s.tenant_id = $2 AND s.slug = $3 AND s.status != 'deleted'
		    AND (s.owner_id = $4 OR s.visibility = 'public')
		 ON CONFLICT (skill_id, user_id) DO NOTHING`,
		userID, tid, slug, userID)
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

