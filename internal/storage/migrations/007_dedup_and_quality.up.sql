-- 007_dedup_and_quality.up.sql
--
-- 1. Idempotency on the POS's own transaction id. A POS that re-exports
--    a day (often with corrections) must not double-count. Normalized
--    rows now carry (pos_source_id, external_id); the newest raw row for
--    a key wins and the older raw row is recorded in raw_superseded.
-- 2. Normalization failures are tracked per raw row so they are logged
--    once (not on every ingest) and can be shown on the dashboard.
-- 3. transactions.flags records data-quality flags on rows we accept
--    (e.g. total does not match quantity x price, negative quantity).

ALTER TABLE transactions ADD COLUMN pos_source_id TEXT;
ALTER TABLE transactions ADD COLUMN external_id TEXT;
ALTER TABLE transactions ADD COLUMN flags TEXT NOT NULL DEFAULT '';

UPDATE transactions SET
    pos_source_id = (SELECT r.pos_source_id FROM raw_pos_transactions r WHERE r.id = transactions.raw_transaction_id),
    external_id   = NULLIF(TRIM((SELECT json_extract(r.raw_payload, '$.external_id')
                                 FROM raw_pos_transactions r WHERE r.id = transactions.raw_transaction_id)), '');

CREATE TABLE raw_superseded (
    raw_transaction_id  INTEGER PRIMARY KEY REFERENCES raw_pos_transactions(id),
    superseded_by       INTEGER NOT NULL REFERENCES raw_pos_transactions(id),
    superseded_at       TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Collapse duplicates that were ingested before this migration: keep the
-- newest row per (pos_source_id, external_id).
CREATE TEMP TABLE _dups AS
SELECT t.id AS id,
       t.raw_transaction_id AS raw_id,
       (SELECT t2.raw_transaction_id FROM transactions t2
         WHERE t2.pos_source_id = t.pos_source_id AND t2.external_id = t.external_id
         ORDER BY t2.id DESC LIMIT 1) AS keep_raw
FROM transactions t
WHERE t.external_id IS NOT NULL
  AND t.id < (SELECT MAX(t3.id) FROM transactions t3
               WHERE t3.pos_source_id = t.pos_source_id AND t3.external_id = t.external_id);
INSERT INTO raw_superseded (raw_transaction_id, superseded_by) SELECT raw_id, keep_raw FROM _dups;
DELETE FROM transactions WHERE id IN (SELECT id FROM _dups);
DROP TABLE _dups;

CREATE UNIQUE INDEX idx_transactions_source_external
    ON transactions (pos_source_id, external_id)
    WHERE external_id IS NOT NULL;

CREATE TABLE raw_normalize_errors (
    raw_transaction_id  INTEGER PRIMARY KEY REFERENCES raw_pos_transactions(id),
    error               TEXT NOT NULL,
    attempts            INTEGER NOT NULL DEFAULT 1,
    first_seen_at       TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_attempt_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Raw rows that have neither been normalized nor superseded. The
-- occurred_date is the POS's own business date (station-local), which is
-- what the data-quality score is bucketed by.
CREATE VIEW raw_unresolved AS
SELECT r.id, r.pos_source_id, r.raw_payload, r.ingestion_batch_id, r.received_at,
       substr(json_extract(r.raw_payload, '$.occurred_at'), 1, 10) AS occurred_date,
       TRIM(json_extract(r.raw_payload, '$.product_alias')) AS product_alias
FROM raw_pos_transactions r
LEFT JOIN transactions t   ON t.raw_transaction_id = r.id
LEFT JOIN raw_superseded s ON s.raw_transaction_id = r.id
WHERE t.id IS NULL AND s.raw_transaction_id IS NULL;
