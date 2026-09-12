-- 007_dedup_and_quality.down.sql
DROP VIEW IF EXISTS raw_unresolved;
DROP TABLE IF EXISTS raw_normalize_errors;
DROP INDEX IF EXISTS idx_transactions_source_external;
DROP TABLE IF EXISTS raw_superseded;
ALTER TABLE transactions DROP COLUMN flags;
ALTER TABLE transactions DROP COLUMN external_id;
ALTER TABLE transactions DROP COLUMN pos_source_id;
