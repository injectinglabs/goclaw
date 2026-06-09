-- 000073_sheet_workflows (down)
--
-- Reverse order vs the up migration to honour FK dependencies:
-- webhook_idempotency → runs → workflows; cells → runs.

DROP TABLE IF EXISTS webhook_idempotency;
DROP TABLE IF EXISTS sheet_workflow_cells;
DROP TABLE IF EXISTS sheet_workflow_runs;
DROP TABLE IF EXISTS sheet_workflows;
