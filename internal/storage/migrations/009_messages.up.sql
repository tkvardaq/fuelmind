-- 009_messages.up.sql
--
-- The outbox. Until now FuelMind only ever spoke when spoken to: the
-- owner had to open the dashboard, or ask a question over the relay.
-- This table lets the station start the conversation — a closing summary
-- every evening, and an alert when something needs the owner's attention
-- before they would otherwise notice.
--
-- Everything queued here is composed from mart figures that are already
-- computed, so a message can never contain a number the dashboard does
-- not also show.
--
-- dedup_key is what stops a restart, a clock change or a second core
-- process from sending the same evening summary twice: it is the message
-- kind plus the day it is about, and it is UNIQUE.

CREATE TABLE messages (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    kind        TEXT NOT NULL,                       -- daily_summary | score_drop | credit_overdue | ingest_stalled | test
    dedup_key   TEXT NOT NULL UNIQUE,                -- e.g. "daily_summary:2026-09-13"
    channel     TEXT NOT NULL DEFAULT 'whatsapp',
    recipient   TEXT NOT NULL,
    body        TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'queued'
                CHECK (status IN ('queued', 'sent', 'failed')),
    attempts    INTEGER NOT NULL DEFAULT 0,
    last_error  TEXT,
    created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    sent_at     TIMESTAMP
);

CREATE INDEX idx_messages_queue ON messages (status, id);
CREATE INDEX idx_messages_time ON messages (created_at DESC);
