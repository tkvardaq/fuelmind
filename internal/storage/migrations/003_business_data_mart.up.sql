-- 003_business_data_mart.up.sql
--
-- Business data mart (spec §3.3): pre-aggregated, query-ready tables
-- that the dashboard and the intent router read from. Nothing above
-- this layer ever touches raw POS quirks.
--
-- All tables in this file are REFRESHED by the materialization job in
-- `internal/mart/` after every ingestion batch and on a 15-minute
-- timer. They are NOT computed on the fly per query.
--
-- Spec deviations (same as migration 002): DECIMAL -> REAL. SQLite has
-- no native DECIMAL; v1 precision is good enough. v2/HQ on Postgres
-- will use NUMERIC and this becomes a non-issue.
--
-- v1 simplification: the full FuelMind Score algorithm is
-- deliberately a simple weighted sum of clear-cut thresholds. ML
-- anomaly detection is v2. See `internal/mart/score.go`.

CREATE TABLE daily_sales (
    date                TEXT NOT NULL,
    product_code        TEXT NOT NULL REFERENCES fuel_products(product_code),
    volume_liters       REAL NOT NULL DEFAULT 0,
    revenue             REAL NOT NULL DEFAULT 0,
    transaction_count   INTEGER NOT NULL DEFAULT 0,
    updated_at          TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (date, product_code)
);

CREATE INDEX idx_daily_sales_date ON daily_sales (date);

-- fuel_margin requires a purchase-price feed which v1 does NOT have
-- from the POS (CSV is sales-only). For v1 we leave the cost columns
-- at 0 and document the limitation; v1.1 adds a manual "today's
-- purchase price" UI in the dashboard. The structure is in place.
CREATE TABLE fuel_margin (
    date                TEXT NOT NULL,
    product_code        TEXT NOT NULL REFERENCES fuel_products(product_code),
    revenue             REAL NOT NULL DEFAULT 0,
    cost_of_goods       REAL NOT NULL DEFAULT 0,
    margin_amount       REAL NOT NULL DEFAULT 0,
    margin_pct          REAL NOT NULL DEFAULT 0,
    updated_at          TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (date, product_code)
);

-- credit_outstanding: per-customer running balance of credit sales
-- minus payments. v1 only sees credit sales (no payment feed yet);
-- the amount grows over time until the operator records a payment
-- in the v1.1 manual UI.
CREATE TABLE credit_outstanding (
    customer_phone      TEXT NOT NULL,
    as_of_date          TEXT NOT NULL,
    outstanding_amount  REAL NOT NULL DEFAULT 0,
    transaction_count   INTEGER NOT NULL DEFAULT 0,
    days_overdue        INTEGER NOT NULL DEFAULT 0,
    updated_at          TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (customer_phone, as_of_date)
);

CREATE INDEX idx_credit_outstanding_phone ON credit_outstanding (customer_phone);
CREATE INDEX idx_credit_outstanding_date ON credit_outstanding (as_of_date);

-- The FuelMind Score, computed daily per spec §3.3. The v1 algorithm
-- is a simple weighted sum; see internal/mart/score.go.
CREATE TABLE station_health_score (
    date                  TEXT PRIMARY KEY,
    sales_score           INTEGER NOT NULL,
    inventory_score       INTEGER NOT NULL,
    cash_score            INTEGER NOT NULL,
    credit_score          INTEGER NOT NULL,
    data_quality_score    INTEGER NOT NULL,
    operations_score      INTEGER NOT NULL,
    overall_score         INTEGER NOT NULL,
    issues_json           TEXT NOT NULL DEFAULT '[]',
    updated_at            TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
