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
                │       http://shop-pc:8765          │   LAN
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
   values from the data mart. Enforced twice: the system prompt says so,
   and `llm.ungroundedNumbers` drops any reply containing a number that
   is not in the context block it was given.
2. **Raw is immutable.** `raw_pos_transactions` rows are never updated or
   deleted. When a POS re-exports a transaction, the *normalized* row is
   replaced and the superseded raw row is recorded in `raw_superseded`;
   both raw rows stay for audit.
3. **Migrations have up + down, and run in a transaction.** A migration
   that fails part-way leaves no schema behind, and an existing database
   is copied to `backups\pre-migrate-*.db` before the first pending
   migration runs.
4. **The service runs under `NT AUTHORITY\LocalService`.** No admin
   rights. The data folder ACL grants LocalService, Administrators and
   SYSTEM only.
5. **The dashboard requires a PIN**, locks after five wrong attempts, and
   only lets the *first* PIN be set from the shop PC itself. It listens
   on the LAN because the owner uses a phone; "it is on the local
   network" is never the access control.
6. **The station works with the internet unplugged.** Cloud calls are
   licensing, updates and (opt-in) health only. Health data is sent only
   with the owner's consent.
7. **No `fmt.Println` outside `main` packages and tests.** Everything is
   `slog`, so the same logs go to stderr in dev and to
   `logs\core.log` under the service.

## Layout on a station

```
%ProgramFiles%\FuelMind\   launcher (service), core, setup tool
%ProgramData%\FuelMind\    fuelmind.db, pos_drop\, versions\, backups\, logs\
                           current.json / previous.json / pending.json
```

The launcher supervises the core, swaps versions on request (core exit
code 42), verifies the new version with `/healthz`, and rolls back on a
failed self-check or a crash loop.

## Cross-references

- Spec: `FuelMind_Architecture_Spec.md` (12 sections, 570+ lines)
- Build plan: `FUELMIND_BUILD_PLAN.md` (24-week sequence, phase gates)
- POS adapter contract: `internal/posadapter/adapter.go`
- Author guide for new adapters: `docs/ADAPTER_AUTHOR_GUIDE.md`
- Wire format: `testdata/pos_samples/README.md`
