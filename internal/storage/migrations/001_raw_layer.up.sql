-- 001_raw_layer.up.sql
--
-- Raw layer (spec §3.1): append-only, untouched copies of what the POS
-- adapter gave us. This is the audit trail. Nothing ever deletes from
-- these tables; if a row is "wrong", the normalizer is fixed and re-run.
--
-- One deviation from spec §3.1: we add `payload_hash` (SHA-256 of
-- raw_payload) with a UNIQUE constraint. Reason: re-ingesting the same
-- file (e.g. after a crash between parse and archive) must be a no-op,
-- and the spec's schema only has pos_source_id + raw_payload + batch_id
-- — no clean dedup key. The hash is derivable from the payload, so it's
-- not new information, just a cached index.

CREATE TABLE raw_pos_transactions (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    pos_source_id       TEXT NOT NULL,
    raw_payload         TEXT NOT NULL,
    payload_hash        TEXT NOT NULL UNIQUE,
    received_at         TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    ingestion_batch_id  TEXT NOT NULL
);

CREATE INDEX idx_raw_pos_tx_batch  ON raw_pos_transactions (ingestion_batch_id);
CREATE INDEX idx_raw_pos_tx_source ON raw_pos_transactions (pos_source_id);

CREATE TABLE raw_inventory_snapshots (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    tank_source_id      TEXT NOT NULL,
    raw_payload         TEXT NOT NULL,
    received_at         TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    ingestion_batch_id  TEXT NOT NULL
);

CREATE TABLE raw_shift_data (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    shift_source_id     TEXT NOT NULL,
    raw_payload         TEXT NOT NULL,
    received_at         TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    ingestion_batch_id  TEXT NOT NULL
);
