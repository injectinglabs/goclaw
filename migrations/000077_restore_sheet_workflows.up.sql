-- 000077_restore_sheet_workflows.up.sql
--
-- Restore the sheet-workflow orchestrator's persistence layer after the
-- Phase 4 deletion in migration 76. We're going back to the server-side
-- orchestrator path (sheets_enrich_run + sheets-mcp sidecar) because the
-- skill+spawn+composio_BULK_SHEET_WRITE replacement turned out slower
-- in practice (wall-clock dominated by per-subagent web_search timeouts
-- and LLM per-turn tool-call truncation when fanning out > 5 spawns).
--
-- This migration is a re-apply of the original tables from migration 73
-- + the grant-backfill DO-block from migration 74. Both are idempotent
-- (IF NOT EXISTS + ON CONFLICT DO NOTHING) so re-running is a no-op on
-- environments that somehow already have the tables, and a clean restore
-- on environments that ran migration 76 to drop them.
--
-- NOT in scope: re-creating the `mcp_servers` row for sheets-mcp. That
-- row carries encrypted `headers` (service token under GOCLAW_ENCRYPTION_KEY)
-- and was historically provisioned out-of-band, not by a migration.
-- A separate one-shot encrypt-and-INSERT step runs alongside the deploy.

BEGIN;

-- ── 1) sheet_workflows ──────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS sheet_workflows (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    user_id         varchar(255) NOT NULL,
    name            text NOT NULL,
    description     text,

    spreadsheet_id  text NOT NULL,
    sheet_tab       text NOT NULL DEFAULT 'Sheet1',
    target_range    text NOT NULL DEFAULT 'A2:Z',

    columns_json    jsonb NOT NULL,
    triggers_json   jsonb NOT NULL DEFAULT '[]'::jsonb,

    visibility      varchar(20) NOT NULL DEFAULT 'personal'
        CHECK (visibility IN ('personal', 'team')),
    status          varchar(20) NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'paused', 'broken')),

    last_run_at     timestamptz,

    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_sheet_workflows_tenant_status
  ON sheet_workflows (tenant_id, status);
CREATE INDEX IF NOT EXISTS idx_sheet_workflows_user
  ON sheet_workflows (user_id);
CREATE INDEX IF NOT EXISTS idx_sheet_workflows_ephemeral_cleanup
  ON sheet_workflows (last_run_at)
  WHERE triggers_json = '[]'::jsonb;

-- ── 2) sheet_workflow_runs ──────────────────────────────────────────

CREATE TABLE IF NOT EXISTS sheet_workflow_runs (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workflow_id       uuid NOT NULL REFERENCES sheet_workflows(id) ON DELETE CASCADE,
    tenant_id         uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,

    triggered_by      varchar(20) NOT NULL
        CHECK (triggered_by IN ('manual', 'cron', 'webhook', 'retry')),
    trigger_payload   jsonb,

    status            varchar(20) NOT NULL DEFAULT 'queued'
        CHECK (status IN ('queued', 'running', 'done', 'cancelled', 'error')),

    row_count         int NOT NULL DEFAULT 0,
    completed_count   int NOT NULL DEFAULT 0,
    error_count       int NOT NULL DEFAULT 0,

    error_message     text,

    total_tokens_in   int NOT NULL DEFAULT 0,
    total_tokens_out  int NOT NULL DEFAULT 0,

    started_at        timestamptz,
    finished_at       timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_sheet_workflow_runs_workflow_created
  ON sheet_workflow_runs (workflow_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_sheet_workflow_runs_recovery
  ON sheet_workflow_runs (status)
  WHERE status IN ('queued', 'running');
CREATE INDEX IF NOT EXISTS idx_sheet_workflow_runs_tenant_status
  ON sheet_workflow_runs (tenant_id, status);

-- ── 3) sheet_workflow_cells ─────────────────────────────────────────

CREATE TABLE IF NOT EXISTS sheet_workflow_cells (
    run_id        uuid NOT NULL REFERENCES sheet_workflow_runs(id) ON DELETE CASCADE,
    row_idx       int  NOT NULL,
    col_idx       int  NOT NULL,

    status        varchar(20) NOT NULL DEFAULT 'queued'
        CHECK (status IN ('queued', 'running', 'done', 'error', 'skipped')),
    error_message text,
    attempt       int  NOT NULL DEFAULT 0,

    tokens_in     int  NOT NULL DEFAULT 0,
    tokens_out    int  NOT NULL DEFAULT 0,
    latency_ms    int,

    updated_at    timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (run_id, row_idx, col_idx)
);

CREATE INDEX IF NOT EXISTS idx_sheet_workflow_cells_run_status
  ON sheet_workflow_cells (run_id, status);

-- ── 4) webhook_idempotency ──────────────────────────────────────────

CREATE TABLE IF NOT EXISTS webhook_idempotency (
    key          text PRIMARY KEY,
    workflow_id  uuid NOT NULL REFERENCES sheet_workflows(id) ON DELETE CASCADE,
    run_id       uuid REFERENCES sheet_workflow_runs(id) ON DELETE SET NULL,

    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL DEFAULT (now() + interval '24 hours')
);

CREATE INDEX IF NOT EXISTS idx_webhook_idempotency_workflow
  ON webhook_idempotency (workflow_id);
CREATE INDEX IF NOT EXISTS idx_webhook_idempotency_expires
  ON webhook_idempotency (expires_at);

-- ── 5) sheets-mcp grant backfill ────────────────────────────────────
--
-- Re-apply the grant-backfill from migration 74. Skips gracefully if the
-- sheets-mcp global mcp_servers row hasn't been provisioned yet (the
-- separate one-shot script runs after this migration).

DO $$
DECLARE
    sheets_id   UUID;
    document_id UUID;
    granted_n   INT;
BEGIN
    SELECT id INTO sheets_id
    FROM mcp_servers
    WHERE name = 'sheets-mcp' AND tenant_id IS NULL;

    SELECT id INTO document_id
    FROM mcp_servers
    WHERE name = 'document-mcp' AND tenant_id IS NULL;

    IF sheets_id IS NULL THEN
        RAISE NOTICE 'sheets-mcp global row missing — grant backfill skipped (run provisioning script post-migration)';
        RETURN;
    END IF;

    IF document_id IS NULL THEN
        RAISE NOTICE 'document-mcp global row missing — grant backfill skipped';
        RETURN;
    END IF;

    INSERT INTO mcp_agent_grants (
        server_id, agent_id, tenant_id, enabled, tool_allow, tool_deny,
        config_overrides, granted_by
    )
    SELECT
        sheets_id,
        g.agent_id,
        g.tenant_id,
        TRUE,
        g.tool_allow,
        g.tool_deny,
        g.config_overrides,
        'migration_000077'
    FROM mcp_agent_grants g
    WHERE g.server_id = document_id
      AND g.enabled = TRUE
    ON CONFLICT (server_id, agent_id) DO NOTHING;

    GET DIAGNOSTICS granted_n = ROW_COUNT;
    RAISE NOTICE 'sheets-mcp backfill: % new agent grants', granted_n;
END $$;

COMMIT;
