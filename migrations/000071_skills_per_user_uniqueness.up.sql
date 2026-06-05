-- 000071_skills_per_user_uniqueness
--
-- Pivots skills to per-user ownership: every user installs their own
-- row, identified by (tenant_id, slug, owner_id). Sharing is a
-- visibility flag on the OWNER's row — other users see it as "shared by
-- X" but they don't get a copy of the row or its files. Delete only
-- affects the caller's own row.
--
-- Why this design (after considering grant-shared and single-canonical):
--   * Grant-shared (one row + skill_user_grants) had a footgun: the
--     first-installer became the canonical owner. If admin installed
--     first then member, admin's delete cascaded to member's grant —
--     "I deleted my skill and now Maria's gone too" surprise.
--   * Single-canonical-row also forced an ownership-transfer story
--     when public ("admin now controls everyone's access"), which made
--     the team-admin role load-bearing for every delete confirmation.
--   * Per-user is the only model where "the delete button only ever
--     touches my own data" is intrinsically true — no cascades, no
--     warns, no admin override. Same semantics integrations already
--     use (per-(org, user)).
--
-- Storage cost is bounded (a typical skill is ~100KB; 5 users × 10
-- skills = 5MB per tenant). Update propagation is per-user too:
-- check-updates flags everyone with the same source_url that a new
-- SHA is upstream, each user re-installs on their own row.
--
-- Migration is non-destructive: every existing row already has owner_id
-- set, so the new (tenant, slug, owner) uniqueness is a superset of the
-- old (tenant, slug) — no collisions.

DROP INDEX IF EXISTS idx_skills_tenant_slug;

-- New unique index. Status partial so a soft-deleted row of the same
-- (tenant, slug, owner) doesn't block a reinstall, matching the
-- soft-delete patterns used elsewhere in the schema.
CREATE UNIQUE INDEX idx_skills_tenant_slug_owner
  ON skills (tenant_id, slug, owner_id)
  WHERE status != 'deleted';

-- Hot lookup index: "find this user's row for this slug" runs on every
-- install (ON CONFLICT) and every use_skill resolution.
CREATE INDEX IF NOT EXISTS idx_skills_owner_slug
  ON skills (owner_id, slug)
  WHERE status != 'deleted';
