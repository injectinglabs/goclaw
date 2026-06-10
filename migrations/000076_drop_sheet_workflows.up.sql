-- 000076_drop_sheet_workflows.up.sql
--
-- Phase 4 cleanup: drop the sheet-workflow orchestrator's persistence
-- layer. The orchestrator Go code was removed in PR #334; the sheets-mcp
-- sidecar that fronted it was removed in injecting-ai-goclaw PR #264.
-- New path is purely skill + spawn + composio BULK_SHEET_WRITE — no
-- server-side state to persist, so these tables have zero readers and
-- writers in the running binary.
--
-- Order matters: child tables first, then parent, then the sidecar's
-- mcp_servers/grants rows (FK cascades may or may not be in place, so
-- we DELETE explicitly to be safe across environments).

BEGIN;

-- Per-cell run state (FK to sheet_workflow_runs).
DROP TABLE IF EXISTS sheet_workflow_cells;

-- Webhook idempotency cache (was workflow-trigger specific per 73.up).
DROP TABLE IF EXISTS webhook_idempotency;

-- Per-trigger runs (FK to sheet_workflows).
DROP TABLE IF EXISTS sheet_workflow_runs;

-- Workflow definitions (parent).
DROP TABLE IF EXISTS sheet_workflows;

-- sheets-mcp sidecar grants + global server row. Migration 74 backfilled
-- mcp_agent_grants onto every tenant's default agent; with the sidecar
-- gone those grants point at nothing. Clean both. mcp_user_grants for
-- sheets-mcp (if any) wiped too.
DELETE FROM mcp_agent_grants
WHERE server_id IN (SELECT id FROM mcp_servers WHERE name = 'sheets-mcp');

DELETE FROM mcp_user_grants
WHERE server_id IN (SELECT id FROM mcp_servers WHERE name = 'sheets-mcp');

DELETE FROM mcp_servers WHERE name = 'sheets-mcp';

COMMIT;
