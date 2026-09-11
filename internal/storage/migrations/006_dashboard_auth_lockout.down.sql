-- 006_dashboard_auth_lockout.down.sql
--
-- Revert to state before adding failed attempt tracking.
DROP TABLE IF EXISTS auth_sessions;
DROP TABLE IF EXISTS dashboard_users;
-- Recreate tables as per 004_dashboard.up.sql
CREATE TABLE dashboard_users (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    username      TEXT NOT NULL UNIQUE,
    pin_hash      TEXT NOT NULL,        -- PBKDF2-SHA256, base64
    pin_salt      TEXT NOT NULL,        -- 16 random bytes, base64
    pin_iters     INTEGER NOT NULL,     -- PBKDF2 iterations (100k+ in v1.1)
    is_active     INTEGER NOT NULL DEFAULT 1,
    created_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_login_at TIMESTAMP
);

-- Single shared user for v1. The installer (Phase 8) prompts for a
-- PIN on first run and creates this row. Default password is empty
-- so login is forced to set one. The default username 'owner' is
-- the only one the v1 dashboard ever shows.
INSERT INTO dashboard_users (username, pin_hash, pin_salt, pin_iters)
VALUES ('owner', '', '', 0);

CREATE TABLE auth_sessions (
    id            TEXT PRIMARY KEY,     -- random 32-byte hex
    user_id       INTEGER NOT NULL REFERENCES dashboard_users(id),
    created_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at    TIMESTAMP NOT NULL,   -- created_at + 24h
    user_agent    TEXT
);

CREATE INDEX idx_auth_sessions_user ON auth_sessions (user_id);
CREATE INDEX idx_auth_sessions_expiry ON auth_sessions (expires_at);