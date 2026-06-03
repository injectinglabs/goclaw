-- 000067_default_agent_emoji_backfill.down.sql
--
-- Reverts the 🧬 backfill on canonical default agents. Only touches rows
-- whose emoji is still exactly 🧬 — user-set emojis (rare on locked
-- defaults, but possible via direct DB manipulation pre-lock) stay intact.

UPDATE agents
SET emoji = ''
WHERE agent_key = 'default'
  AND owner_id  = 'system'
  AND emoji = U&'\D83E\DDEC'
  AND deleted_at IS NULL;
