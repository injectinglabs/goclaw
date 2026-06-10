-- 000077_restore_sheet_workflows.down.sql
--
-- Mirror of migration 76: drop the four sheet-workflow tables + sheets-mcp
-- grants. Use this only if rolling back to the skill+spawn path again.

BEGIN;

DROP TABLE IF EXISTS sheet_workflow_cells;
DROP TABLE IF EXISTS webhook_idempotency;
DROP TABLE IF EXISTS sheet_workflow_runs;
DROP TABLE IF EXISTS sheet_workflows;

DELETE FROM mcp_agent_grants
WHERE server_id IN (SELECT id FROM mcp_servers WHERE name = 'sheets-mcp');

DELETE FROM mcp_user_grants
WHERE server_id IN (SELECT id FROM mcp_servers WHERE name = 'sheets-mcp');

COMMIT;
