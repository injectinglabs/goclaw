-- Dedicated reminders table — independent of cron_jobs / cron_run_logs.
-- Each cron job that delivers to an internal channel (ws/browser) also
-- writes a row here so the extension's inbox has a durable, canonical
-- source that survives one-shot job deletion and client logout/login.
--
-- Intentionally NO foreign key to cron_jobs(id) — one-shot "at" jobs get
-- auto-deleted after firing and we don't want cascade to nuke the
-- reminder row with them. job_id is kept only as provenance metadata.
CREATE TABLE reminders (
    id                   UUID NOT NULL PRIMARY KEY,
    tenant_id            UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    user_id              TEXT NOT NULL,
    job_id               TEXT,                -- best-effort provenance, not FK-enforced
    job_name             TEXT NOT NULL DEFAULT '',
    origin_session_key   TEXT NOT NULL,       -- chat where cron was scheduled; used by client to route
    channel              TEXT NOT NULL,
    content              TEXT NOT NULL,
    delivered_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    read_at              TIMESTAMPTZ          -- null = unread
);

CREATE INDEX idx_reminders_tenant_user_delivered ON reminders (tenant_id, user_id, delivered_at DESC);
CREATE INDEX idx_reminders_origin_session_key ON reminders (origin_session_key) WHERE origin_session_key <> '';
