-- 008_prices_payments_remote.up.sql
--
-- Three things v1 could not answer honestly before:
--
-- 1. fuel_purchase_prices — what the station paid per litre. Without it
--    "margin" was revenue with a zero cost, which is not a margin.
--    Prices are entered by the owner and apply from a date forward.
-- 2. credit_payments — money customers pay back. Without it a credit
--    balance only ever grew.
-- 3. remote_questions — the audit trail of questions answered over the
--    cloud relay (WhatsApp / SMS / support), so the owner can see every
--    question that was asked about their station and what was answered.

CREATE TABLE fuel_purchase_prices (
    product_code    TEXT NOT NULL REFERENCES fuel_products(product_code),
    effective_date  TEXT NOT NULL,           -- YYYY-MM-DD, applies from this day on
    cost_per_liter  REAL NOT NULL CHECK (cost_per_liter >= 0),
    note            TEXT,
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (product_code, effective_date)
);

CREATE TABLE credit_payments (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    customer_phone  TEXT NOT NULL,
    paid_on         TEXT NOT NULL,           -- YYYY-MM-DD
    amount          REAL NOT NULL CHECK (amount > 0),
    method          TEXT NOT NULL DEFAULT 'CASH',
    note            TEXT,
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_credit_payments_customer ON credit_payments (customer_phone, paid_on);

CREATE TABLE remote_questions (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    message_id      TEXT NOT NULL UNIQUE,    -- the control plane's id
    asked_by        TEXT,                    -- phone number or support user
    question        TEXT NOT NULL,
    answer          TEXT NOT NULL,
    route           TEXT NOT NULL,           -- standard | llm | llm-rejected | ...
    answered_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_remote_questions_time ON remote_questions (answered_at DESC);
