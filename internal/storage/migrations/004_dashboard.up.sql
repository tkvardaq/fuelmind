-- 004_dashboard.up.sql
--
-- Phase 4: local dashboard. We need:
--   * A single dashboard_users table with a PBKDF2-hashed PIN
--   * A auth_sessions table for session cookies (httpOnly, 24h expiry)
--
-- Spec §9 calls for "single shared PIN (hashed, stored locally)" in
-- v1 — this is the minimum viable auth for the dashboard. v1.1 will
-- add per-user accounts with named roles; v1 ships with a single
-- owner account created at first install.

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
