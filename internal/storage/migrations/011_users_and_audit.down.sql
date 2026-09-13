-- 011_users_and_audit.down.sql
DROP INDEX IF EXISTS idx_audit_log_action;
DROP INDEX IF EXISTS idx_audit_log_actor;
DROP INDEX IF EXISTS idx_audit_log_time;
DROP TABLE IF EXISTS audit_log;

-- SQLite has supported DROP COLUMN since 3.35; the pure-Go driver in use
-- is well past that.
ALTER TABLE dashboard_users DROP COLUMN role;
ALTER TABLE dashboard_users DROP COLUMN display_name;
