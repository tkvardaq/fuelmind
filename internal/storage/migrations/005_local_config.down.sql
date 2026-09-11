-- 005_local_config.down.sql
DROP INDEX IF EXISTS idx_sync_log_status;
DROP INDEX IF EXISTS idx_sync_log_sent_at;
DROP TABLE IF EXISTS sync_log;
DROP INDEX IF EXISTS idx_local_config_sync;
DROP TABLE IF EXISTS local_config;