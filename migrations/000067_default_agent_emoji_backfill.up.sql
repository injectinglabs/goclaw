-- 000067_default_agent_emoji_backfill.up.sql
--
-- Backfills the AOS brand emoji 🧬 (DNA helix) on every canonical default
-- agent that still has an empty `emoji` column. Tenants provisioned BEFORE
-- the auth-proxy change at injecting-ai-goclaw#229 saved the row without
-- an emoji, and the row is `is_locked=true` (migration 000066) — so a
-- PUT-style backfill from auth-proxy on next login is rejected by goclaw
-- with 409. Direct DB UPDATE is the only way to retro-fit the icon.
--
-- New tenants pick up the emoji from auth-proxy's `agentBody` at
-- provisioning time, so this migration is a one-shot retro-fit, not a
-- recurring concern.

UPDATE agents
SET emoji = U&'\D83E\DDEC'  -- 🧬 (U+1F9EC)
WHERE agent_key = 'default'
  AND owner_id  = 'system'
  AND COALESCE(emoji, '') = ''
  AND deleted_at IS NULL;
