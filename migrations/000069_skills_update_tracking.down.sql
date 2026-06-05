ALTER TABLE skills
  DROP COLUMN IF EXISTS update_available_sha,
  DROP COLUMN IF EXISTS update_available_ref,
  DROP COLUMN IF EXISTS last_update_check;
