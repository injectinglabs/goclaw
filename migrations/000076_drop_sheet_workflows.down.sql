-- 000076_drop_sheet_workflows.down.sql
--
-- Reverse of 76.up — recreating the sheet-workflow tables and the
-- sheets-mcp grants is non-trivial (the original schema lives in
-- migrations 73 and 74). For a real rollback, re-run migrations 73 +
-- 74 directly via `migrate up -path migrations/`. This down-migration
-- intentionally does nothing rather than half-recreate the schema.

SELECT 'down-migration is a no-op — re-run migrations 73 + 74 to restore the sheet-workflow tables' AS notice;
