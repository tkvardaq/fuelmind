-- 004_heartbeats.up.sql
--
-- One row per heartbeat POST. Payload shape per spec §6.3 (no
-- financial detail). The id is bigserial so the admin can page
-- through recent heartbeats without a timestamp scan, and so we
-- can age out old rows cheaply.
--
-- We retain heartbeats for 90 days by default (Phase 5 cut line);
-- a cron job in /ops purges older rows on a nightly schedule.
-- 90 days is enough to investigate "station X went silent on
-- Tuesday" without holding data forever.

CREATE TABLE heartbeats (
    id              BIGSERIAL PRIMARY KEY,
    station_id      TEXT NOT NULL REFERENCES stations(station_id),
    received_at     TIMESTAMP NOT NULL DEFAULT now(),
    payload_json    JSONB NOT NULL
);

CREATE INDEX idx_heartbeats_station_received
    ON heartbeats (station_id, received_at DESC);
CREATE INDEX idx_heartbeats_received
    ON heartbeats (received_at DESC);