-- 005_local_config.up.sql
--
-- Phase 5: per-station identity for the cloud control plane.
--
-- Every install gets a station_id and a per-station API key on first
-- run (see internal/ident). These are the credentials the local core
-- uses to authenticate with the cloud control plane's heartbeat /
-- license endpoints (spec §6).
--
-- We persist them in a generic key/value `local_config` table rather
-- than as dedicated columns so future keys (hardware tier override,
-- rollback-pin, last-cloud-sync timestamps, telemetry opt-in flags)
-- don't require a schema migration every time. The first few rows
-- are seeded by the storage package on Migrate(); the rest of the
-- keys live as JSON blobs in config_json for grouping.
--
-- No PII or financial data is ever stored here. The cloud only sees
-- the values marked `synchronized=true` and even then only the
-- station_id + api_key + heartbeat payload (spec §6.3) — never any
-- of these config rows.

CREATE TABLE local_config (
    -- Single-row config keys. We use TEXT as the PK so a future
    -- "add a new key" is just an INSERT, not a schema change.
    key              TEXT PRIMARY KEY,
    value            TEXT NOT NULL,
    -- Optional structured companion. Used for keys whose value
    -- alone is insufficient (e.g. license cache includes tier +
    -- features_json + valid_until + last_seen_at).
    config_json      TEXT,
    -- Marks whether this row is meant to be synced to the cloud
    -- control plane. Default 0 (local-only); set to 1 only for
    -- rows that must reach the cloud.
    synchronized     INTEGER NOT NULL DEFAULT 0,
    created_at       TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at       TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_local_config_sync ON local_config (synchronized);

-- Heartbeat log: one row per outbound heartbeat attempt. Useful for
-- debugging "why didn't my station check in?" and for the local
-- "last sync" UI affordance in v1.1. Kept tight (no payload) — full
-- payloads live in the cloud's heartbeats table.
--
-- status: ok | failed
-- reason: when failed, the short error category (timeout, http_4xx,
--         http_5xx, dns, parse). Free-form, but bounded vocabulary
--         so the dashboard can group them.
CREATE TABLE sync_log (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    sent_at         TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    status          TEXT NOT NULL,
    reason          TEXT,
    http_status     INTEGER,
    round_trip_ms   INTEGER
);

CREATE INDEX idx_sync_log_sent_at ON sync_log (sent_at);
CREATE INDEX idx_sync_log_status ON sync_log (status);