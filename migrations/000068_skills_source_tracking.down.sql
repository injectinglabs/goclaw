DROP TABLE IF EXISTS skill_install_events;
ALTER TABLE skills
  DROP COLUMN IF EXISTS source_url,
  DROP COLUMN IF EXISTS source_sha,
  DROP COLUMN IF EXISTS source_ref,
  DROP COLUMN IF EXISTS installed_by,
  DROP COLUMN IF EXISTS installed_at;
