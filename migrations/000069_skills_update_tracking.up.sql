ALTER TABLE skills
  ADD COLUMN IF NOT EXISTS update_available_sha varchar(64),
  ADD COLUMN IF NOT EXISTS update_available_ref text,
  ADD COLUMN IF NOT EXISTS last_update_check   timestamptz;

CREATE INDEX IF NOT EXISTS skills_update_check_idx
  ON skills (last_update_check)
  WHERE status <> 'deleted' AND source_url IS NOT NULL;
