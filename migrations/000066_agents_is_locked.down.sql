-- 000066_agents_is_locked.down.sql

DROP INDEX IF EXISTS agents_is_locked_idx;
ALTER TABLE agents DROP COLUMN IF EXISTS is_locked;
