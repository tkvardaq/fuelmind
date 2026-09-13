-- 012_pin_resets.down.sql
DROP INDEX IF EXISTS idx_pin_reset_time;
DROP INDEX IF EXISTS idx_pin_reset_one_pending;
DROP TABLE IF EXISTS pin_reset_requests;
