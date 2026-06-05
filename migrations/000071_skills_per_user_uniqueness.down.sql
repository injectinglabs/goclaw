-- 000071_skills_per_user_uniqueness — DOWN
--
-- Restores the pre-71 single-row-per-(tenant, slug) constraint. NOTE:
-- if any (tenant, slug) has duplicate rows after the up migration ran
-- (e.g. two users in the same tenant installed the same skill), this
-- DROP+CREATE will fail at index creation. That is by design — we want
-- the rollback to noisily refuse rather than silently collapse user
-- data. Run a manual SELECT to find conflicting rows first.

DROP INDEX IF EXISTS idx_skills_owner_slug;
DROP INDEX IF EXISTS idx_skills_tenant_slug_owner;
CREATE UNIQUE INDEX idx_skills_tenant_slug ON skills (tenant_id, slug);
