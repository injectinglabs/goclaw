-- 000066_agents_is_locked.up.sql
--
-- Adds the `is_locked` flag to `agents`. Locked rows reject Update/Delete from
-- the public HTTP API regardless of caller role — they exist as the tenant's
-- canonical fallback agent (the seeded "default") and must never be deleted or
-- mutated by end users, including tenant owners. Other system-seeded templates
-- (researcher, writer, coder) are no longer tenant-shared — they're cloned
-- per-user during the user-bootstrap flow with is_locked=false and become
-- ordinary editable / deletable user agents.
--
-- Backfill: every existing tenant's `default` row (owner_id='system',
-- agent_key='default') is marked locked. Tenant-shared researcher/writer/coder
-- rows are left as-is by this migration — they'll be retired by the bootstrap
-- changes that follow (user-bootstrap re-creates them per-user; existing rows
-- stay only as legacy data and the UI hides them via owner_id='system').

ALTER TABLE agents
  ADD COLUMN IF NOT EXISTS is_locked boolean NOT NULL DEFAULT false;

UPDATE agents
SET is_locked = true
WHERE owner_id = 'system'
  AND agent_key = 'default'
  AND deleted_at IS NULL;

-- Cheap partial index: locked-agent lookups are rare but a query that asks
-- "is this id locked?" hits exactly one row, which is the worst-case shape
-- for a seq scan over a hot table.
CREATE INDEX IF NOT EXISTS agents_is_locked_idx
  ON agents (id)
  WHERE is_locked = true AND deleted_at IS NULL;
