-- 002_normalized_layer.up.sql
--
-- Normalized layer (spec §3.2): canonical vocabulary, units, and
-- well-typed numeric columns. This is where `HSD` / `Hi-Speed Diesel`
-- / `DIESEL` / `Diesel` collapse to `DIESEL`, and where the CSV's
-- string decimals become REAL.
--
-- Spec deviations:
--   * DECIMAL -> REAL. SQLite has no native DECIMAL type; storage is
--     IEEE-754 double. For the financial values in this table (liters,
--     PKR amounts) the precision of double is sufficient for v1
--     (single station, no aggregation at sub-paisa level). Phase 9
--     will revisit this with integer-paisa if we find a real bug.
--     When the mart moves to Postgres in v2/HQ, the column types
--     become NUMERIC(12,2) and this is no longer a concern.
--   * Spec's `quantity_liters DECIMAL(10,3)` and `unit_price
--     DECIMAL(10,2)` and `total_amount DECIMAL(12,2)` are all REAL
--     here. Same reasoning.
--   * No `shifts` / `customers` / `expenses` tables in v1 — they
--     belong to the v1.1 manual-entry UI and the unbuilt shift/inventory
--     feeds. Listed in the down-migration so a future v1.1 migration
--     can add them in the right order.
--
-- The `raw_transaction_id` FK is the idempotency anchor. Re-running
-- the normalizer over the same raw row must be a no-op; we use
-- `INSERT OR IGNORE` with a UNIQUE constraint on `raw_transaction_id`.

CREATE TABLE fuel_products (
    product_code   TEXT PRIMARY KEY,
    display_name   TEXT NOT NULL,
    unit           TEXT NOT NULL DEFAULT 'LITER'
);

CREATE TABLE product_aliases (
    alias_text     TEXT PRIMARY KEY,
    product_code   TEXT NOT NULL REFERENCES fuel_products(product_code)
);

CREATE TABLE transactions (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    raw_transaction_id  INTEGER NOT NULL UNIQUE REFERENCES raw_pos_transactions(id),
    product_code        TEXT NOT NULL REFERENCES fuel_products(product_code),
    quantity_liters     REAL NOT NULL,
    unit_price          REAL NOT NULL,
    total_amount        REAL NOT NULL,
    payment_method      TEXT NOT NULL,
    customer_phone      TEXT,
    pump_id             TEXT,
    attendant           TEXT,
    transaction_time    TIMESTAMP NOT NULL,
    created_at          TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_transactions_time    ON transactions (transaction_time);
CREATE INDEX idx_transactions_product ON transactions (product_code, transaction_time);
CREATE INDEX idx_transactions_payment ON transactions (payment_method, transaction_time);

-- Seed the canonical products. v1 covers the three we see in the
-- fixtures. The installer can extend this in Phase 8.
INSERT INTO fuel_products (product_code, display_name) VALUES
    ('DIESEL',    'Diesel'),
    ('PETROL_92', 'Petrol 92'),
    ('PETROL_95', 'Petrol 95');

-- Seed the alias table. The entries here are exactly the strings we
-- see in the fixture CSVs. A real station's POS may add more — v1.1
-- will add a /admin/aliases page to extend this without a migration.
INSERT INTO product_aliases (alias_text, product_code) VALUES
    ('DIESEL',          'DIESEL'),
    ('Diesel',          'DIESEL'),
    ('diesel',          'DIESEL'),
    ('HSD',             'DIESEL'),
    ('Hi-Speed Diesel', 'DIESEL'),
    ('Hi Speed Diesel', 'DIESEL'),
    ('PETROL_92',       'PETROL_92'),
    ('P-92',            'PETROL_92'),
    ('PMG-92',          'PETROL_92'),
    ('Petrol 92',       'PETROL_92'),
    ('petrol_92',       'PETROL_92'),
    ('PETROL_95',       'PETROL_95'),
    ('P-95',            'PETROL_95'),
    ('PMG-95',          'PETROL_95'),
    ('Petrol 95',       'PETROL_95'),
    ('petrol_95',       'PETROL_95');
