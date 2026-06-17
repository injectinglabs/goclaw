-- Reverse 000074: remove sheets-mcp agent grants created by this migration.
-- Only grants whose granted_by tag matches are removed, leaving any
-- pre-existing or operator-issued grants untouched.

BEGIN;

DELETE FROM mcp_agent_grants
WHERE granted_by = 'migration_000074'
  AND server_id IN (
    SELECT id FROM mcp_servers
    WHERE name = 'sheets-mcp' AND tenant_id IS NULL
  );

COMMIT;
