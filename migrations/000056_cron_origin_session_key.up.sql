-- origin_session_key records the session from which a cron job was scheduled.
-- Used to route cron-delivered messages back to that session (so the originating
-- chat keeps a durable record in DB rather than relying on in-memory client state)
-- and to let WS clients filter reminders by origin.
-- Empty string means "not set" (jobs created before this migration or via paths
-- that don't carry session context, e.g. HTTP API).
ALTER TABLE cron_jobs ADD COLUMN origin_session_key TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_cron_jobs_origin_session_key
    ON cron_jobs (origin_session_key)
    WHERE origin_session_key <> '';
