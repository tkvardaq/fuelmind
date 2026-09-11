# FuelMind Architecture (local view)

This file is the in-repo pointer to the canonical architecture document
and a short summary of the local-core layout. The full engineering
specification lives in `FuelMind_Architecture_Spec.md` (companion
document); the build plan lives in `FUELMIND_BUILD_PLAN.md`.

## The one-line rule

> The local core must be able to ingest POS data, reconcile, run
> analytics, and answer owner queries with zero internet connectivity.
> Cloud is licensing, updates, monitoring, and (opt-in) backup/HQ —
> never in the critical path of "how much diesel did I sell today."

Every architectural decision in this repo serves that rule.

## Layers (in the local core)

```
                ┌────────────────────────────────────┐
                │       Dashboard (Phase 4)          │   served on
                │       http://localhost:8765        │   localhost
                └────────────┬───────────────────────┘
                             │ reads only
                ┌────────────▼───────────────────────┐
                │     Intent Router (Phase 7)         │   stdlib +
                │  regex → small classifier → LLM     │   Ollama
                └────────────┬───────────────────────┘
                             │ reads only
                ┌────────────▼───────────────────────┐
                │   Business Data Mart (Phase 3)      │   SQLite
                │   daily_sales, fuel_margin, …       │   (WAL)
                └────────────┬───────────────────────┘
                             │ writes
                ┌────────────▼───────────────────────┐
                │    Normalizer (Phase 2)             │
                │    raw → canonical vocabulary       │
                └────────────┬───────────────────────┘
                             │ writes
                ┌────────────▼───────────────────────┐
                │   POS Adapter Layer (Phase 1)       │   plugin
                │   csv_watch, vendor_x, vendor_y…    │   interface
                └────────────────────────────────────┘
                             │ raw vendor data
                          POS / pump
```

## Hard rules

1. **The LLM never computes numbers.** It only explains pre-computed
   values from the data mart. Enforced in code (post-generation regex
   check) and in the system prompt.
2. **Raw is immutable.** `raw_pos_transactions` rows are never updated
   or deleted by anything. The normalizer reads from raw and writes to
   normalized; if normalized data is wrong, the normalizer is re-run.
3. **Migrations have up + down.** Every schema change has a tested
   rollback. The update mechanism never ships a migration without one.
4. **The local core runs under `NT AUTHORITY\LocalService` (or the
   POSIX equivalent).** No admin-equivalent. Data folder ACL restricts
   write to the service account.
5. **The dashboard requires a PIN.** We do not rely on "it's on the
   local network" as access control.
6. **No `fmt.Println` outside `main.go` and tests.** Everything is
   `slog` so we can ship the same logs to stderr (dev) and a rotating
   file (production).

## Cross-references

- Spec: `FuelMind_Architecture_Spec.md` (12 sections, 570+ lines)
- Build plan: `FUELMIND_BUILD_PLAN.md` (24-week sequence, phase gates)
- POS adapter contract: `internal/posadapter/adapter.go`
- Author guide for new adapters: `docs/ADAPTER_AUTHOR_GUIDE.md`
- Wire format: `testdata/pos_samples/README.md`
