-- Snapshot origin_session_key + job_name into cron_run_logs so the reminder
-- inbox can query the full history — even for one-shot jobs whose cron_jobs
-- row was deleted after firing. JOIN-at-read would lose that info; snapshot
-- at INSERT preserves it.
ALTER TABLE cron_run_logs ADD COLUMN origin_session_key TEXT NOT NULL DEFAULT '';
ALTER TABLE cron_run_logs ADD COLUMN job_name TEXT NOT NULL DEFAULT '';

-- Relax the FK so deleting a one-shot cron_jobs row after completion doesn't
-- CASCADE-wipe its run log. Keep the reference for analytics (still joinable
-- when the job exists) but permit the job_id to become NULL after deletion.
ALTER TABLE cron_run_logs ALTER COLUMN job_id DROP NOT NULL;
ALTER TABLE cron_run_logs DROP CONSTRAINT IF EXISTS cron_run_logs_job_id_fkey;
ALTER TABLE cron_run_logs
    ADD CONSTRAINT cron_run_logs_job_id_fkey
    FOREIGN KEY (job_id) REFERENCES cron_jobs(id) ON DELETE SET NULL;

-- Partial index — queries for a user's reminders filter by non-empty origin.
CREATE INDEX IF NOT EXISTS idx_cron_run_logs_origin_session_key
    ON cron_run_logs (origin_session_key, ran_at DESC)
    WHERE origin_session_key <> '';
