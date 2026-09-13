-- 010_data_sources.down.sql
DROP INDEX IF EXISTS idx_ingest_events_time;
DROP TABLE IF EXISTS ingest_events;
DROP TABLE IF EXISTS pos_watch_folders;
