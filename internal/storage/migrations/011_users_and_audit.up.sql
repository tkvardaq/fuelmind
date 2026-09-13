-- 011_users_and_audit.up.sql
--
-- Until now every person at the station shared one PIN and one account
-- called "owner". That was defensible when the dashboard only showed
-- figures. It stopped being defensible when the dashboard learned to
-- *change* them: a sale can be typed in, a credit repayment recorded, a
-- purchase price set, a watch folder added. Those all move money on the
-- books, and "who did this?" had no answer at all.
--
-- Two things change here:
--
-- 1. dashboard_users gains a display name and a role, so the station can
--    have the owner plus named staff, each with their own PIN. The
--    existing single account is migrated, not replaced — an install that
--    upgrades keeps working with the PIN it already has.
-- 2. audit_log records every change: who, what, when, and enough detail
--    to see what actually happened. It is append-only by convention and
--    nothing in the application deletes from it.

ALTER TABLE dashboard_users ADD COLUMN display_name TEXT;
ALTER TABLE dashboard_users ADD COLUMN role TEXT NOT NULL DEFAULT 'staff';

-- The account that already exists is the owner, and keeps every right it
-- had. Naming it "Owner" means the audit log reads sensibly from the
-- first entry, before anyone has renamed anything.
UPDATE dashboard_users
   SET role = 'owner',
       display_name = COALESCE(NULLIF(display_name, ''), 'Owner')
 WHERE username = 'owner';

CREATE TABLE audit_log (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    occurred_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    -- Who. user_id is kept for joining, but actor_name is stored as it
    -- was at the time: renaming or removing a person later must not
    -- rewrite what the log says they did.
    user_id      INTEGER REFERENCES dashboard_users(id),
    actor_name   TEXT NOT NULL,
    actor_role   TEXT NOT NULL,
    -- Where the change came from: dashboard | whatsapp | service.
    source       TEXT NOT NULL DEFAULT 'dashboard',
    -- What. action is a stable slug (sale.manual, credit.payment, ...),
    -- subject is what it was done to, and detail is the human sentence
    -- shown on the Activity page.
    action       TEXT NOT NULL,
    subject      TEXT,
    detail       TEXT,
    -- The client address, so a change made from a phone on the shop
    -- wi-fi can be told from one made at the counter.
    remote_addr  TEXT
);

CREATE INDEX idx_audit_log_time ON audit_log (occurred_at DESC);
CREATE INDEX idx_audit_log_actor ON audit_log (user_id, occurred_at DESC);
CREATE INDEX idx_audit_log_action ON audit_log (action, occurred_at DESC);
