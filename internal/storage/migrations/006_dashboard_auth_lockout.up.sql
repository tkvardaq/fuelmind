-- 006_dashboard_auth_lockout.up.sql
--
-- Phase 9: Add failed attempt tracking for PIN lockout.
ALTER TABLE dashboard_users ADD COLUMN failed_attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE dashboard_users ADD COLUMN last_failed_at TIMESTAMP;
ALTER TABLE dashboard_users ADD COLUMN lock_until TIMESTAMP;