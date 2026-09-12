-- 008_prices_payments_remote.down.sql
DROP INDEX IF EXISTS idx_remote_questions_time;
DROP TABLE IF EXISTS remote_questions;
DROP INDEX IF EXISTS idx_credit_payments_customer;
DROP TABLE IF EXISTS credit_payments;
DROP TABLE IF EXISTS fuel_purchase_prices;
