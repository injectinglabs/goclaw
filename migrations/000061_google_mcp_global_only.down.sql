-- 000061_google_mcp_global_only.down.sql
--
-- Roll back the Google/Slack MCP consolidation. Like 000060, we don't
-- re-create the per-tenant rows: an accurate restoration would need the
-- per-tenant server/grant/credential snapshot captured before the up
-- migration, which we never persisted.
--
-- Removing the global rows sends these sidecars back to "no server
-- registered" until an operator re-seeds. Acceptable for a roll-back:
-- the up migration is an opt-in cleanup, so rolling back is an explicit
-- "I want the previous per-tenant state back" — re-seed by hand.

BEGIN;

DELETE FROM mcp_servers
WHERE name IN (
    'gmail-mcp', 'calendar-mcp', 'sheets-mcp',
    'docs-mcp', 'drive-mcp', 'slack-mcp'
)
  AND tenant_id IS NULL;

COMMIT;
