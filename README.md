# FuelMind

> Local-first fuel station management. The owner's data stays on the shop PC; the cloud is a thin control plane for licensing, updates, and (opt-in) HQ aggregation.

This is the v1 monorepo. It contains:

- `cmd/fuelmind-core/` — the local core Windows service (Go, single static binary)
- `internal/posadapter/` — the plug-in POS interface, the registry, and the v1 vendor-agnostic `csv_watch` adapter
- `internal/storage/` — embedded-SQLite storage with versioned migrations
- `internal/auth/` — PIN-based dashboard auth (PBKDF2-SHA256, session cookies)
- `internal/web/` — local HTTP dashboard on `127.0.0.1:<port>` (templates + handlers)
- `cloud/` — control-plane schema and admin (added in Phase 5)
- `installer/` — Windows MSI packaging (added in Phase 8)
- `docs/` — architecture, runbook, adapter author guide
- `testdata/` — sample POS exports for tests

The full build plan (24 weeks, solo founder, 5–10 pilot stations) lives in [`FUELMIND_BUILD_PLAN.md`](./FUELMIND_BUILD_PLAN.md). The architecture and engineering specification it implements lives in `FuelMind_Architecture_Spec.md` (companion document).

---

## Status: Phase 0 through Phase 5 shipped

### Phase 0 (Setup)

- [x] Go module skeleton (`go.mod`)
- [x] `cmd/fuelmind-core/main.go` — starts, logs, signals clean shutdown
- [x] `internal/posadapter/` — the plug-in interface, the registry, and the `csv_watch` adapter shell
- [x] `internal/posadapter/csvwatch/adapter_test.go` — contract test every adapter must pass
- [x] `testdata/pos_samples/` — sample POS CSV exports defining the v1 wire format
- [x] `docs/ADAPTER_AUTHOR_GUIDE.md` — how to write a new POS adapter
- [x] GitHub Actions CI: `go build` + `go test` + lint + cross-build on Ubuntu & Windows

### Phase 1 — `csv_watch` goes live

- [x] `internal/storage/` — pure-Go SQLite (`modernc.org/sqlite`, no CGo) with WAL mode, foreign keys, busy timeout
- [x] Embedded migrator — every migration is `NNN_name.up.sql` + `NNN_name.down.sql`, run on boot, idempotent, recorded in `schema_migrations`
- [x] Migration 001: raw layer (`raw_pos_transactions`, `raw_inventory_snapshots`, `raw_shift_data`) with a `payload_hash UNIQUE` column for idempotent re-ingest
- [x] `internal/posadapter/csvwatch/parse.go` — CSV parser with header validation, per-row error collection, **multi-format timestamp support** (RFC 3339, naive ISO, Excel-style space separator), JSON payload emission
- [x] `internal/posadapter/csvwatch/watcher.go` — `fsnotify`-based file watcher; reads `.csv` into memory + closes handle before rename (Windows-correct); writes to raw layer; archives to `processed/` (success) or `failed/` (any error); **groups by `pos_source_id` so multi-lane CSVs preserve their source**; auto-creates archive subdirs
- [x] `internal/posadapter/csvwatch/adapter.go` — `*conn` renamed to `*Conn` (exported) so the runtime can get the folder and hand it to the Watcher
- [x] E2E tests: drop a real fixture, assert 10 rows in `raw_pos_transactions` split across 3 lanes; drop a malformed file, assert it lands in `failed/`; re-process a file, assert dedup via `payload_hash`
- [x] `cmd/fuelmind-core/main.go` wired: open DB → migrate → start watcher → block on signals

### Phase 2 — Normalizer: raw → canonical

- [x] Migration 002: normalized layer (`fuel_products`, `product_aliases`, `transactions`) with `raw_transaction_id UNIQUE` for idempotent normalization
- [x] Seed data: 3 canonical products (`DIESEL`, `PETROL_92`, `PETROL_95`) + 16 alias mappings covering HSD/Hi-Speed Diesel/Diesel, PMG-92/P-92, etc.
- [x] `internal/normalizer/` — `Run(limit)` walks un-normalized rows, resolves `HSD`/`Hi-Speed Diesel`/`DIESEL`/`Diesel` to canonical `DIESEL` (case-insensitive fallback), parses decimals (with comma-thousands support), preserves the POS's payment method with a warning if unknown
- [x] Watcher auto-runs the normalizer after every successful ingest
- [x] Idempotency: re-running on already-normalized raw rows is a no-op via `INSERT OR IGNORE` on `raw_transaction_id`
- [x] Tests: alias resolution table, end-to-end row normalization, unknown alias rejection, decimal parsing (incl. comma thousands)

### Phase 3 — Business data mart

- [x] Migration 003: mart tables (`daily_sales`, `fuel_margin`, `credit_outstanding`, `station_health_score`) per spec §3.3
- [x] `internal/mart/` — `MaterializeSince(date)` and `MaterializeAfterIngest()` refresh all mart tables with idempotent UPSERT semantics
- [x] `daily_sales`: volume + revenue + count per (date, product_code), derived from `transactions` via a single `GROUP BY`
- [x] `fuel_margin`: revenue + cost_of_goods (v1 = 0, no purchase-price feed yet) + margin_amount + margin_pct
- [x] `credit_outstanding`: per-customer running balance, with `days_overdue` computed from `julianday()` math
- [x] `station_health_score`: the FuelMind Score — 6 components (sales 25%, inventory 15%, cash 15%, credit 15%, data_quality 15%, operations 15%) plus a structured `issues_json` list the Phase 7 LLM will read verbatim to explain the score in natural language
- [x] Watcher auto-runs the materializer after every successful ingest
- [x] 15-minute periodic refresh ticker in `main.go` (spec §3.3: "every 15 min")
- [x] Tests: end-to-end materialization, idempotent re-runs, credit-outstanding aggregation, score components (good day, empty day, high-credit warning)

### Phase 4 — Local dashboard (THIS TURN)

- [x] Migration 004: `dashboard_users` (default `owner` row) + `auth_sessions` (id, user_id, expires_at, user_agent)
- [x] `internal/storage/` extensions: mart read helpers (`TodayTotals`, `RecentDailySales`, `LatestScore`, `RecentCredit`) + dashboard user/session CRUD + `ErrNotFound` / `ErrInvalidSession` sentinels
- [x] `internal/auth/` — PBKDF2-SHA256 (100k iters, OWASP 2023 minimum), 16-byte salt, 24h session TTL, single shared owner PIN for v1 (per spec §9), `ErrInvalidPIN` / `ErrInvalidSession` sentinels
- [x] `internal/web/` — HTTP server on `127.0.0.1:<port>`, `embed.FS` for templates + CSS, `requireAuth` middleware, session cookie `fuelmind_session`, request logger, template funcs (`formatMoney` PKR, `formatLiters`, `formatDate`, `severityClass`)
- [x] Handlers: `/setup` (first-run PIN wizard), `/login`, `/logout`, `/` (today dashboard), `/sales`, `/credit`, `/score`, `/healthz`, `/static/`
- [x] Templates: `base.html` (shared chrome), `login.html`, `setup.html`, `dashboard.html`, `sales.html`, `credit.html`, `score.html`, `static/app.css` — server-rendered, no JS framework, mobile-friendly
- [x] Template engine: each page parsed into its own template set with `base.html` cloned in, so per-page `body` blocks don't collide and the shared chrome wraps cleanly
- [x] `cmd/fuelmind-core/main.go` wired: builds auth + web.Server, blocks on `ListenAndServe(ctx)`
- [x] Tests: 32 passing across 6 packages — auth (9: PBKDF2 determinism + wrong/right PIN + session lifecycle), web (8: healthz, redirect, full setup→login→dashboard→logout flow, wrong-PIN error, empty sales, static CSS), mart/normalizer/csvwatch/storage unchanged and still green

### What's still to come (later phases)

- Phase 5 — `station_id`, license check, heartbeat (cloud control plane)
- Phase 6 — Unattended update mechanism (the operational unlock for solo founder)
- Phase 7 — Local LLM + hardware tiering (Ollama, Basic/Standard/Enhanced/Pro)
- Phase 8 — Windows MSI installer + first pilot station
- Phase 9 — Pilot loop, security pass, license-tier admin
- v1.1 — More POS adapters, expense entry, WhatsApp bridge, optional cloud backup

---

## Build & Test (after you install Go)

```powershell
# 1. Install Go 1.23+ from https://go.dev/dl/

# 2. From the repo root:
go mod tidy
go build ./...
go test ./...
```

For a manual smoke test, point the data dir somewhere writable (the
default `C:\ProgramData\FuelMind\` needs admin; the installer handles
that in Phase 8):

```powershell
$env:FUELMIND_DATA_DIR = "$env:USERPROFILE\.fuelmind"
go run ./cmd/fuelmind-core
```

You should see:

```
time=... level=INFO msg="fuelmind starting" version=dev ...
time=... level=INFO msg="storage ready" path=...
time=... level=INFO msg="pos watcher started" folder=...\pos_drop
```

Then drop a sample CSV into the watched folder from another terminal:

```powershell
Copy-Item testdata\pos_samples\transactions_sample_01.csv $env:USERPROFILE\.fuelmind\pos_drop\
```

Within ~100ms the file moves to `$env:USERPROFILE\.fuelmind\pos_drop\processed\` and the rows land in the DB. To verify with the SQLite CLI (install from https://www.sqlite.org/download.html if you don't have it):

```powershell
sqlite3 $env:USERPROFILE\.fuelmind\fuelmind.db "SELECT COUNT(*) FROM raw_pos_transactions;"
# 10
```

Then `Ctrl+C` in the running service for clean shutdown.

## Cross-compile for Windows (the actual deployment target)

```powershell
$env:GOOS = "windows"
$env:GOARCH = "amd64"
go build -ldflags "-s -w" -o dist/fuelmind-core.exe ./cmd/fuelmind-core
```

## Make targets (if you have `make`)

```bash
make build    # go build ./...
make test     # go test ./...
make run      # go run ./cmd/fuelmind-core
make windows  # cross-compile Windows .exe into dist/
make tidy     # go mod tidy
make clean    # remove dist/
```

---

## Repo conventions

- **License:** Apache 2.0 (see `LICENSE`)
- **Go version:** 1.23+
- **Pure-Go only.** No CGo, so the cross-compile stays single-binary. SQLite is via `modernc.org/sqlite`.
- **Migrations:** every migration has `NNN_name.up.sql` + `NNN_name.down.sql` (per spec §7). Bump version on every schema change.
- **Linting:** `go vet ./...`, `gofmt -s -l .`, and `golangci-lint run` (config in `.golangci.yml`) must be clean before commit
- **Tests:** every adapter has a contract test in its own package; E2E tests use real fixtures from `testdata/`
- **Logging:** `log/slog` only. No `fmt.Println` outside `main.go` and test helpers
- **Errors:** wrap with `fmt.Errorf("...: %w", err)`. Never `panic` outside `init()` for programmer errors

---

## What's next

After `go test ./...` is green on your machine, the next concrete deliverable is **Phase 4, Week 10–12**: the local read-only HTTP dashboard. A Go `net/http` server on `localhost:8765`, server-rendered HTML via `html/template` + `embed.FS`, HTMX for live polling on the "today" page, PIN-based login. Reads only from the mart — never touches raw or normalized tables directly. See the spec §3.3 and the build plan §6 Phase 4.
