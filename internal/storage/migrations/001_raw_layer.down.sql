-- 001_raw_layer.down.sql
--
-- Rollback for 001_raw_layer. Per spec §7 ("Never auto-migrate without a
-- rollback path") and the unattended update mechanism in Phase 6, every
-- migration must have a tested down. This one drops all three raw tables.

DROP TABLE IF EXISTS raw_pos_transactions;
DROP TABLE IF EXISTS raw_inventory_snapshots;
DROP TABLE IF EXISTS raw_shift_data;
