-- 000073_sheet_workflows
--
-- Persistent "agentic spreadsheet" workflows over user-owned Google Sheets
-- (Paradigm-style, but the canvas is the user's actual Google Sheet rather
-- than a UI we own). A workflow binds:
--   • a target spreadsheet (id + tab + range),
--   • a typed column schema with per-column prompts and depends_on DAG,
--   • a set of triggers (manual / cron / webhook).
--
-- The four tables here form one cohesive subsystem and ship together:
--
--   sheet_workflows         — workflow definitions (rarely-mutated config).
--   sheet_workflow_runs     — per-trigger runs (one per manual/cron/webhook
--                             invocation), holds aggregate progress + billing
--                             totals; survives goclaw restarts so the recovery
--                             scanner can resume mid-flight runs.
--   sheet_workflow_cells    — per-(run × row × col) status. Metadata only
--                             (status / error / attempt / latency / tokens) —
--                             the cell *value* lives in Google Sheets (the
--                             source of truth). Stored so retry can pick up
--                             unfinished cells without re-reading the sheet.
--   webhook_idempotency     — last-24h cache of webhook idempotency keys so
--                             Zapier / HubSpot / Make retries don't enqueue
--                             duplicate runs (they retry on 5xx by default).
--
-- Cleanup story:
--   • Ephemeral workflows (no triggers) get a workflow row anyway so the
--     orchestrator path is uniform; a periodic vacuum deletes them after
--     30 days idle. Tracked via `last_run_at IS NULL OR last_run_at < now()-30d`.
--   • webhook_idempotency.expires_at drives a 1h sweep cron.

-- ── 1) sheet_workflows ──────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS sheet_workflows (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    -- Cognito sub (varchar(255), no FK — users live in web-backend RDS, not here).
    -- Same convention as skill_user_disables / skills.user_id.
    user_id         varchar(255) NOT NULL,
    name            text NOT NULL,
    description     text,

    spreadsheet_id  text NOT NULL,
    sheet_tab       text NOT NULL DEFAULT 'Sheet1',
    target_range    text NOT NULL DEFAULT 'A2:Z',  -- A1, excluding header row

    -- Column schema: [{id, name, prompt, type, depends_on:[col_id,...]}, ...]
    -- type ∈ text | number | url | email | checkbox | select | multi_select.
    -- Stored as jsonb so SchemaVersion bumps don't require migrations when
    -- we add per-column knobs (e.g. response model overrides).
    columns_json    jsonb NOT NULL,

    -- Triggers: [{type: cron, expr}, {type: webhook, token_hash, payload_map}, ...].
    -- Webhook tokens are SHA256(token) hex — raw token is shown ONCE at
    -- create / rotate, then unrecoverable (same pattern as API keys).
    triggers_json   jsonb NOT NULL DEFAULT '[]'::jsonb,

    visibility      varchar(20) NOT NULL DEFAULT 'personal'
        CHECK (visibility IN ('personal', 'team')),
    status          varchar(20) NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'paused', 'broken')),

    -- Used by ephemeral-cleanup vacuum (NULL until first run, set on
    -- run completion).
    last_run_at     timestamptz,

    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_sheet_workflows_tenant_status
  ON sheet_workflows (tenant_id, status);
CREATE INDEX IF NOT EXISTS idx_sheet_workflows_user
  ON sheet_workflows (user_id);
-- Vacuum predicate: ephemeral workflows (no triggers) older than 30 days
-- with no recent run. Partial index keeps it cheap on the active set.
CREATE INDEX IF NOT EXISTS idx_sheet_workflows_ephemeral_cleanup
  ON sheet_workflows (last_run_at)
  WHERE triggers_json = '[]'::jsonb;

-- ── 2) sheet_workflow_runs ──────────────────────────────────────────

CREATE TABLE IF NOT EXISTS sheet_workflow_runs (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workflow_id       uuid NOT NULL REFERENCES sheet_workflows(id) ON DELETE CASCADE,
    -- Denormalised for hot recovery + per-tenant rate-limit predicates
    -- without joining the parent workflow on every read.
    tenant_id         uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,

    triggered_by      varchar(20) NOT NULL
        CHECK (triggered_by IN ('manual', 'cron', 'webhook', 'retry')),
    trigger_payload   jsonb,  -- webhook body / cron context / manual NULL

    status            varchar(20) NOT NULL DEFAULT 'queued'
        CHECK (status IN ('queued', 'running', 'done', 'cancelled', 'error')),

    row_count         int NOT NULL DEFAULT 0,
    completed_count   int NOT NULL DEFAULT 0,
    error_count       int NOT NULL DEFAULT 0,

    error_message     text,  -- run-level error (auth lost, sheet deleted)
                             -- per-cell errors live in sheet_workflow_cells

    -- Billing totals — aggregated from cells on each progress flush.
    total_tokens_in   int NOT NULL DEFAULT 0,
    total_tokens_out  int NOT NULL DEFAULT 0,

    started_at        timestamptz,  -- NULL while queued
    finished_at       timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

-- Recent runs per workflow (bubble's "Last N runs" panel).
CREATE INDEX IF NOT EXISTS idx_sheet_workflow_runs_workflow_created
  ON sheet_workflow_runs (workflow_id, created_at DESC);
-- Recovery scanner: on boot, pick up any run still flagged running/queued
-- (its goclaw instance crashed) and resume the unfinished cells.
CREATE INDEX IF NOT EXISTS idx_sheet_workflow_runs_recovery
  ON sheet_workflow_runs (status)
  WHERE status IN ('queued', 'running');
-- Per-tenant concurrency cap predicate (count of running runs by tenant).
CREATE INDEX IF NOT EXISTS idx_sheet_workflow_runs_tenant_status
  ON sheet_workflow_runs (tenant_id, status);

-- ── 3) sheet_workflow_cells ─────────────────────────────────────────

CREATE TABLE IF NOT EXISTS sheet_workflow_cells (
    run_id        uuid NOT NULL REFERENCES sheet_workflow_runs(id) ON DELETE CASCADE,
    row_idx       int  NOT NULL,  -- 0-based offset within target_range
    col_idx       int  NOT NULL,  -- index into the workflow's columns array

    status        varchar(20) NOT NULL DEFAULT 'queued'
        CHECK (status IN ('queued', 'running', 'done', 'error', 'skipped')),
    error_message text,
    attempt       int  NOT NULL DEFAULT 0,  -- retry counter (cap = 3)

    tokens_in     int  NOT NULL DEFAULT 0,
    tokens_out    int  NOT NULL DEFAULT 0,
    latency_ms    int,

    updated_at    timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (run_id, row_idx, col_idx)
);

-- Progress aggregation + recovery scanning ("which cells still need work
-- in this resumed run?"). The PK already covers (run_id, *), so we only
-- add a status-filtered index for the recovery scan and progress queries.
CREATE INDEX IF NOT EXISTS idx_sheet_workflow_cells_run_status
  ON sheet_workflow_cells (run_id, status);

-- ── 4) webhook_idempotency ──────────────────────────────────────────

CREATE TABLE IF NOT EXISTS webhook_idempotency (
    key          text PRIMARY KEY,                  -- caller-provided X-Idempotency-Key
                                                    -- or sha256(body) hex if absent
    workflow_id  uuid NOT NULL REFERENCES sheet_workflows(id) ON DELETE CASCADE,
    -- run_id may be NULL if validation (auth / rate-limit / schema) failed
    -- before we got to enqueue — we still record the key so a retry of the
    -- bad payload doesn't re-trigger the same validation path.
    run_id       uuid REFERENCES sheet_workflow_runs(id) ON DELETE SET NULL,

    created_at   timestamptz NOT NULL DEFAULT now(),
    -- TTL = 24h. Sweeper deletes expired rows hourly.
    expires_at   timestamptz NOT NULL DEFAULT (now() + interval '24 hours')
);

CREATE INDEX IF NOT EXISTS idx_webhook_idempotency_workflow
  ON webhook_idempotency (workflow_id);
CREATE INDEX IF NOT EXISTS idx_webhook_idempotency_expires
  ON webhook_idempotency (expires_at);
