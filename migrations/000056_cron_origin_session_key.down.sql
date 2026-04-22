DROP INDEX IF EXISTS idx_cron_jobs_origin_session_key;
ALTER TABLE cron_jobs DROP COLUMN origin_session_key;
