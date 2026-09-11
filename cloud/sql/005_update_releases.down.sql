-- 005_update_releases.down.sql
--
-- Drop the update mechanism tables (reverse of 005_update_releases.up.sql)

DROP TABLE IF EXISTS update_history;
DROP TABLE IF EXISTS update_releases;