DROP INDEX IF EXISTS idx_cron_run_logs_origin_session_key;

-- Restore the CASCADE FK + NOT NULL. Any rows with job_id = NULL (from runs
-- whose parent job was already deleted while this migration was up) must be
-- handled first — drop them to avoid NOT NULL violations on rollback.
DELETE FROM cron_run_logs WHERE job_id IS NULL;
ALTER TABLE cron_run_logs DROP CONSTRAINT IF EXISTS cron_run_logs_job_id_fkey;
ALTER TABLE cron_run_logs ALTER COLUMN job_id SET NOT NULL;
ALTER TABLE cron_run_logs
    ADD CONSTRAINT cron_run_logs_job_id_fkey
    FOREIGN KEY (job_id) REFERENCES cron_jobs(id) ON DELETE CASCADE;

ALTER TABLE cron_run_logs DROP COLUMN job_name;
ALTER TABLE cron_run_logs DROP COLUMN origin_session_key;
