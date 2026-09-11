# csvwatch — the v1 vendor-agnostic POS adapter

This is the **default** POS adapter in v1. It works with any POS system
that can export to CSV in the format documented in
[`testdata/pos_samples/README.md`](../../testdata/pos_samples/README.md).
That includes the majority of regional POS systems, since CSV export is
a near-universal fallback.

## Why this is the v1 default

1. **Vendor-agnostic by design.** No integration manual, no protocol
   reverse-engineering, no vendor relationship required to ship v1.
2. **Trivial to test.** Fixtures are literal files in
   `testdata/pos_samples/`. The contract test runs in milliseconds.
3. **Trivial to operate.** The owner (or a one-time setup) points the
   POS export to a FuelMind-watched folder. After that, it's automatic.
4. **Doesn't block later vendor adapters.** When the real vendor
   relationship is in place (Phase 1.1+), the new adapter slots in via
   the same plugin interface without changing anything else.

## How it works (Phase 1+)

```
POS export ───►  pos_drop/  ───►  csvwatch watcher  ───►  raw_pos_transactions
                                       │
                                       ├── on success: move to pos_drop/processed/
                                       └── on parse error: move to pos_drop/failed/
```

The adapter:

1. Watches a configurable folder (`config["folder"]`).
2. On every new `.csv` file, parses each row into a `RawTransaction`.
3. Writes the rows to `raw_pos_transactions` (Phase 1) via a batch
   insert with a single shared `ingestion_batch_id` (UUID per file).
4. Moves the file to `processed/` on success, `failed/` on any row
   parse error.
5. Logs a structured record per file: rows, parse errors, duration.

## Phase 0 status

The interface, registry, and lifecycle are in place. The contract test
passes. The actual `fsnotify` watcher and CSV parsing are Phase 1 work.

## Phase 1 deliverables

- [ ] `fsnotify` watcher on the configured folder
- [ ] CSV header validation (must match the documented column set)
- [ ] Row-by-row parsing with per-row error reporting
- [ ] File archive (success → `processed/`, error → `failed/`)
- [ ] Idempotent re-processing of `processed/` files (re-running on
  crash recovery, deduped via `raw_transaction_id`)
- [ ] Real-data contract test: parse `transactions_sample_01.csv`,
  assert 10 rows into `raw_pos_transactions` with the expected fields
- [ ] Real-data error test: drop a malformed CSV, assert it lands in
  `failed/` with a clear error log
