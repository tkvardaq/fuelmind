-- 010_data_sources.up.sql
--
-- FuelMind does not read a pump. It reads what another system exports,
-- which means the owner has to be able to say where that export lands —
-- and to put a sale in by hand when the export is late, wrong, or the
-- POS is down. Before this migration neither was possible: the watch
-- folder was a fixed path inside ProgramData and nothing in the
-- dashboard could add a transaction.
--
-- 1. pos_watch_folders — every folder the station watches for exports.
--    The owner points FuelMind at the folder their POS already writes
--    to, instead of copying files into ours.
-- 2. ingest_events — what arrived, from where, and what happened to it.
--    Without this the owner has no way to tell "no sales today" from
--    "the export stopped three days ago".

CREATE TABLE pos_watch_folders (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    path        TEXT NOT NULL UNIQUE,
    label       TEXT,                        -- what the owner calls it
    enabled     INTEGER NOT NULL DEFAULT 1,
    -- built_in marks the folder FuelMind creates for itself. It can be
    -- disabled but not deleted, so there is always somewhere to drop a
    -- file.
    built_in    INTEGER NOT NULL DEFAULT 0,
    last_error  TEXT,
    created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE ingest_events (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    occurred_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    source         TEXT NOT NULL,            -- watch_folder | upload | manual
    origin         TEXT,                     -- folder path, file name, or who typed it
    file_name      TEXT,
    rows_read      INTEGER NOT NULL DEFAULT 0,
    rows_inserted  INTEGER NOT NULL DEFAULT 0,
    rows_rejected  INTEGER NOT NULL DEFAULT 0,
    outcome        TEXT NOT NULL,            -- ok | partial | failed
    detail         TEXT
);

CREATE INDEX idx_ingest_events_time ON ingest_events (occurred_at DESC);
