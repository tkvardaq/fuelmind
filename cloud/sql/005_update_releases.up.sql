-- 005_update_releases.up.sql
--
-- Update manifest (Phase 6 + spec §7). The cloud ships a manifest
-- per release; the install's update agent downloads from
-- artifact_url after verifying checksum_sha256.
--
-- rollout_pct enables staged rollouts (Phase 6 cut-line
-- capability). When rollout_pct < 100 the cloud only advertises
-- the update to a deterministic hash-based subset of stations.

CREATE TABLE update_releases (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    version             TEXT NOT NULL UNIQUE,
    release_notes       TEXT,
    artifact_url        TEXT NOT NULL,
    checksum_sha256     TEXT NOT NULL,
    min_hardware_tier   TEXT,
    released_at         TIMESTAMP NOT NULL DEFAULT now(),
    rollout_pct         INTEGER NOT NULL DEFAULT 100
        CHECK (rollout_pct BETWEEN 0 AND 100)
);

CREATE TABLE update_history (
    station_id      TEXT NOT NULL REFERENCES stations(station_id),
    release_id      UUID NOT NULL REFERENCES update_releases(id),
    applied_at      TIMESTAMP,
    status          TEXT NOT NULL
        CHECK (status IN ('pending', 'downloading', 'applied', 'failed')),
    PRIMARY KEY (station_id, release_id)
);

CREATE INDEX idx_update_history_station_applied
    ON update_history (station_id, applied_at DESC);