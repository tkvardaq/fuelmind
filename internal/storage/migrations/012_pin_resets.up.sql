-- 012_pin_resets.up.sql
--
-- Forgetting a PIN is the most ordinary failure there is, and until now
-- the only way out was a command line on the shop PC run as
-- administrator. That is the right escape hatch for the last owner, but
-- it is a terrible answer for a member of staff who cannot get to the
-- till on a Sunday.
--
-- So: a person who has forgotten their PIN can raise a request, and an
-- owner clears it by setting them a new one. The request itself grants
-- nothing at all — it is a note on a spike. Everything that matters
-- (choosing the new PIN) still requires an owner who is already signed
-- in, which is what keeps a stranger on the shop wi-fi from letting
-- themselves in by asking nicely.

CREATE TABLE pin_reset_requests (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id      INTEGER NOT NULL REFERENCES dashboard_users(id) ON DELETE CASCADE,
    requested_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    -- Where the request came from, so an owner looking at it can tell
    -- "Bilal at the counter" from something arriving off the wi-fi.
    remote_addr  TEXT,
    -- What the person typed, if anything: "left my phone at home".
    note         TEXT,
    -- pending | done | dismissed. Resolved rows are kept so the Activity
    -- page and this table tell the same story.
    status       TEXT NOT NULL DEFAULT 'pending',
    resolved_at  TIMESTAMP,
    resolved_by  INTEGER REFERENCES dashboard_users(id)
);

-- One pending request per person: asking ten times must not fill the
-- owner's screen with ten rows.
CREATE UNIQUE INDEX idx_pin_reset_one_pending
    ON pin_reset_requests (user_id) WHERE status = 'pending';

CREATE INDEX idx_pin_reset_time ON pin_reset_requests (requested_at DESC);
