-- 006_dashboard_auth_lockout.down.sql
--
-- Remove the lockout columns without touching the owner's PIN or
-- active sessions (SQLite 3.35+ supports DROP COLUMN).
ALTER TABLE dashboard_users DROP COLUMN lock_until;
ALTER TABLE dashboard_users DROP COLUMN last_failed_at;
ALTER TABLE dashboard_users DROP COLUMN failed_attempts;
