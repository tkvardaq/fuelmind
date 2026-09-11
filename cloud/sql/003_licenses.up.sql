-- 003_licenses.up.sql
--
-- Licenses drive the feature-flag / upgrade mechanism (spec §6.2).
-- v1 ships with one tier ('private') active for all pilot stations
-- and the rest of the flags dormant. The schema already supports
-- flipping a station to 'connected' or 'connected_hq' — the admin
-- just sets the row and the next heartbeat picks it up.
--
-- features_json is JSONB so adding a new flag is a data change,
-- not a schema change. Existing rows simply default to false for
-- any new flag (the install's IsFeatureEnabled helper handles this).

CREATE TABLE licenses (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    station_id       TEXT NOT NULL REFERENCES stations(station_id),
    tier             TEXT NOT NULL
        CHECK (tier IN ('private', 'connected', 'connected_hq')),
    features_json    JSONB NOT NULL DEFAULT '{}'::jsonb,
    valid_from       TIMESTAMP NOT NULL DEFAULT now(),
    valid_until      TIMESTAMP,
    status           TEXT NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'suspended', 'expired'))
);

CREATE UNIQUE INDEX idx_licenses_station_active
    ON licenses (station_id)
    WHERE status = 'active';