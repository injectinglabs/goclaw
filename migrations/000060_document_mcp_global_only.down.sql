-- 000060_document_mcp_global_only.down.sql
--
-- Roll back the document-mcp consolidation. We don't try to re-create
-- the per-tenant rows — they were value-empty placeholders, and an
-- accurate restoration would need the per-tenant agent_grants snapshot
-- captured before the up migration, which we never persisted.
--
-- Removing the global row sends document-mcp back to "no server
-- registered" until an operator re-runs the seed step. Acceptable for
-- a roll-back since the up migration is itself an opt-in cleanup; if
-- you're rolling back you're explicitly saying "I want the previous
-- broken state back" and re-seeding per-tenant rows by hand.

BEGIN;

DELETE FROM mcp_servers
WHERE name = 'document-mcp'
  AND tenant_id IS NULL;

COMMIT;
