-- 000072_skill_user_disables
--
-- Per-user override that hides a shared (visibility=public) skill from
-- the caller's catalog + agent's use_skill resolution. Decouples
-- "this skill exists in my workspace" (canonical row's enabled flag,
-- controlled by the row owner) from "I personally want it active"
-- (this table, controlled by each user independently).
--
-- Why a separate table instead of a column on skill_user_grants:
--   skill_user_grants encodes "I have explicit access to an
--   internal-visibility skill". Disabling a public skill for self has
--   no relationship to grants — it's an opt-out, not an opt-in. Keeping
--   the two concerns in different tables makes the predicates clearer
--   (the ListAccessible filter literally reads "AND NOT EXISTS
--   (skill_user_disables …)").
--
-- A row in this table means "the user does NOT want this skill active
-- for themselves." Re-enable = DELETE the row. (Idempotent: a no-op
-- DELETE is fine, and INSERT ON CONFLICT keeps the toggle reentrant.)

CREATE TABLE IF NOT EXISTS skill_user_disables (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    skill_id   uuid NOT NULL REFERENCES skills(id) ON DELETE CASCADE,
    user_id    varchar(255) NOT NULL,
    tenant_id  uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(skill_id, user_id)
);

-- Hot predicate: "is this skill disabled for this caller?" runs on every
-- ListAccessible (per-message during agent runs).
CREATE INDEX IF NOT EXISTS idx_skill_user_disables_user_skill
  ON skill_user_disables (user_id, skill_id);

-- Tenant filter for ops/admin queries.
CREATE INDEX IF NOT EXISTS idx_skill_user_disables_tenant
  ON skill_user_disables (tenant_id);
