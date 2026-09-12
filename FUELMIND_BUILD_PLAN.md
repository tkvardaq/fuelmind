# FuelMind — v1 Build Plan

> Companion to `FuelMind_Architecture_Spec.md`. The spec is the *what* (architecture, schema, components, security). This document is the *how* (sequenced build, ownership, deliverables, release criteria) for the first 6 months, scoped to **5–10 pilot stations** as a **solo founder**.

> **Status (12 Sep 2026): Phases 0–9 implemented, post-review.** An
> end-to-end review of the tree found 40 issues (6 of them ship-blockers)
> and all of them are now fixed and covered by tests. What changed, by
> phase:
>
> - **Phase 1–3 (ingest → normalize → mart).** `csv_watch` now accepts
>   `.CSV`, Excel's UTF-8 BOM and upper-case headers, and never
>   overwrites an archived export. Idempotency moved from "hash of the
>   whole row" to `(pos_source_id, external_id)`, so a POS re-export with
>   a correction replaces the earlier row instead of doubling revenue.
>   Timestamps are normalized to station-local time before the mart
>   buckets them by day. The mart refreshes from the earliest date in the
>   batch (backfills used to be invisible), scores days that contain only
>   unreadable rows, and no longer awards the 30% of the score that
>   inventory and cash cannot measure in v1.
> - **Phase 4 (dashboard).** Fixed a CSS unit that rendered all body text
>   at 1px on screens ≥1200px, and the credit page, which referenced
>   fields that do not exist and served a half-rendered page as HTTP 200.
>   The dashboard now binds all interfaces so the owner's phone can reach
>   it (a release criterion), while the *first* PIN can only be set from
>   the shop PC. Added PIN lockout (5 attempts / 15 min), a 404 page,
>   version in the footer and mobile layout fixes.
> - **Phase 5 (identity + heartbeat).** The heartbeat honours the
>   telemetry consent checkbox: without consent it carries only station
>   id, version and timestamp. `last_pos_ingestion_at` queried a column
>   that does not exist and always reported zero; fixed. A rollback is
>   now reported with the version that failed, once.
> - **Phase 6 (unattended updates).** Rebuilt as `internal/update` +
>   `internal/version`: authenticated update checks, real zip extraction
>   (with zip-slip and version-path validation), case-insensitive
>   checksums, a resume that works when the server ignores `Range`, an
>   unbiased rollout bucket, and a maintenance window. The launcher is a
>   real Windows service now: it bootstraps the bundled core, keeps the
>   child in a job object (no orphan holding the port), backs off
>   restarts, checks `/healthz` after a switch and rolls back on failure.
> - **Phase 7 (LLM).** The intent router is wired into the dashboard's
>   Ask box. It refuses questions it has no data for instead of answering
>   with an unrelated figure, and a post-generation check drops any LLM
>   reply containing a number that is not in its context (spec §4 hard
>   rule, risk R6). Automatic hardware tiering now considers RAM and
>   stops at Standard; Enhanced/Pro are opt-in via
>   `FUELMIND_HARDWARE_TIER` because GPU presence is not detected.
> - **Phase 8 (installer).** The MSI shipped 4-day-old binaries and
>   registered a service that could not start (no SCM handler, and no
>   `current.json`/`versions\` layout). `installer\build.ps1` now builds
>   the binaries, the update artifact and the MSI from one command; the
>   service runs as LocalService with restart recovery, a firewall rule
>   for TCP 8765 scoped to the local subnet, and an ACL on the data
>   folder. The console "first-run wizard" is gone: the MSI opens the
>   dashboard, and `fuelmind-setup` became the support tool
>   (`status`, `reset-pin`, `test-heartbeat`).
> - **Phase 9 (hardening).** Migrations run in a transaction with an
>   automatic pre-migration backup; the 006 down-migration no longer
>   wipes the owner's PIN. Nightly backups use `VACUUM INTO` at 02:00
>   station time. Sessions are purged. The tree is under git, CI builds
>   and tests on Linux and Windows (the Linux build was broken by a
>   Windows-only import, and the gofmt gate silently passed on anything).
>
> **Still deliberately not built:** inventory and cash feeds (scored as
> unmeasured), purchase prices for real margin, expense entry, the
> WhatsApp bridge, cloud backup, and the production cloud service —
> `cloud/sql` holds the schema and `cmd/fuelmind-devcloud` runs the same
> API locally for development and update rehearsals.
>
> **Tests:** 133 across 15 packages, including the launcher's
> update/rollback paths driven by a fake core binary, and an end-to-end
> test that drives a day of exports through the real components.

---

## 0. TL;DR

| | |
|---|---|
| **Goal** | 5–10 petrol stations running FuelMind Local Core in production by end of month 6, with all data local-first, license/cloud plumbing already in place, and the unattended-update + heartbeat path proven |
| **Stack** | Go for the local core service (single .exe), SQLite (via `modernc.org/sqlite` to avoid CGo) + WAL, embedded web UI (HTMX + Alpine.js + Go templates — chosen for solo-founder velocity; React/Vite swap-in if needed), Ollama for local LLM, Postgres on cloud (managed), S3-compatible object storage, Twilio or 360dialog for WhatsApp (Business tier) |
| **Team** | 1 (you) + targeted contract/AI help on a few items (callouts below) |
| **Repo** | Monorepo: `core/` (Go service), `web/` (assets + templates), `cloud/` (Postgres schema + admin), `adapters/` (POS plugin Go packages), `docs/` (runbooks), `installer/` (Windows MSI) |
| **Build order** | Maps to spec §12 but pulled tight: POS plugin interface + 1 vendor deep → raw/normalized/mart → read-only dashboard → intent router (deterministic only) → `station_id` + license + heartbeat → unattended update → LLM hardware tiering → WhatsApp bridge → cloud admin |
| **Differentiator vs. OSS** | No existing open-source fuel-station system combines local-first + LLM intent routing + multi-vendor POS plugin interface + hardware-tiered on-prem LLM. We will use OSS for *parts* (POS plugin pattern from InvenTree, sync primitives from PowerSync/ElectricSQL ideas) and own the integration |

---

## 1. Confirmed Decisions (from kickoff Q&A)

| # | Decision | Value | Why |
|---|---|---|---|
| D1 | Team size | Solo founder, 5–10 pilots, 4–6 months | Plan is single-track, with clear cut-lines to defer post-pilot |
| D2 | Local core language | **Go** | Single static `.exe`, no runtime, smallest field-support surface |
| D3 | POS adapter strategy | **Vendor-agnostic v1: `csv_watch` adapter (file-watch + CSV parse) as the default. No vendor-specific adapter required for v1.** Vendor-specific adapters (DB read, serial, REST) added in v1.1 once the real vendor relationships are in place — same plugin interface, no other code change | Spec §2.2 adapter interface is the seam. CSV watch-folder works with any POS that can export CSV, which is ~every POS system as a fallback |
| D4 | Local DB | **SQLite** (WAL mode, `modernc.org/sqlite` — pure Go, no CGo) | Removes "service won't start" support class |
| D5 | Local dashboard | Browser-served on `localhost:PORT`, accessed from any LAN device | No separate install; usable on shop PC or owner's phone browser |
| D6 | Local LLM | Ollama, tiered by hardware detect | Spec §5 |
| D7 | Cloud control plane | PostgreSQL (managed) + S3-compatible object storage | Spec §6 |
| D8 | Cloud <-> local transport | HTTPS, **polled** (heartbeat cycle) | Spec §2; works on patchy internet |
| D9 | Sales model | Product (self-serve install); tech visits rare | Spec preamble |

## 2. Open Decisions (need your input — flagged inline where they hit the plan)

| # | Decision | Default if no answer | Where it bites |
|---|---|---|---|
| O1 | ~~Name the v1 POS vendor~~ | **Resolved**: v1 is vendor-agnostic. `csv_watch` adapter accepts any CSV in the format documented in `testdata/pos_samples/README.md`. Vendor-specific adapters (DB read / serial / REST) are a v1.1 add | n/a |
| O2 | Cloud host | **Hetzner** (€4–8/mo for the v1 control plane, EU or Helsinki) — cheap, good for 5–10 stations. Alternative: DigitalOcean, AWS RDS | Cost only, easy to migrate later |
| O3 | Cloud <-> local comms: per-station API key vs. mTLS | API key (simpler, sufficient for v1; mTLS deferred) | Heartbeat endpoint auth |
| O4 | Single v1 tier ("Private") or already ship "Connected" tier dormant | Ship **Private tier active, Connected tier dormant in DB schema + license check** (matches spec §6.2 — features_json present, flips to active when you sell it) | Cloud license table, sync agent code paths |
| O5 | Dashboard framework: HTMX+Alpine vs. React/Vite | **HTMX + Alpine.js + Go `html/template`** for v1 (less to build, no separate build step, easy for solo). React is a one-day swap if needed for richer UX in v1.1 | Web stack choice, affects the dashboard scope |
| O6 | WhatsApp BSP (Twilio / 360dialog / Gupshup) | **360dialog** (most PK-friendly pricing, official Meta BSP) — *deferred to v1.1*; WhatsApp bridge is post-pilot | §8 |
| O7 | First pilot station | **You pick** — should be a station you personally have a relationship with, ideally with reliable internet, cooperative owner, varied fuel mix, and the v1 POS vendor | Pilot gate in Week 20 |
| O8 | Are you OK with the local core running under `NT AUTHORITY\LocalService` (least-privilege Windows service) or do you need a local admin? | **LocalService** + a `fuelmind` data folder under `C:\ProgramData\FuelMind\` | Installer design |
| O9 | Branding (logo, color) for the dashboard? | Use a neutral "FuelMind" wordmark + monochrome palette for v1; you can theme later | Dashboard layout |
| O10 | Telemetry consent copy — exact wording for the "your data is local, only X is sent" disclosure shown at first install | I'll write a draft in Week 8; you review | First-run UX |

---

## 3. Open-Source Landscape (what exists, what we'll borrow, what's our differentiator)

### 3.1 Fuel-station / petrol-pump OSS

| Repo | Stack | Useful for | Verdict |
|---|---|---|---|
| [ambroz72/Petrol-pump-management-system-](https://github.com/ambroz72/Petrol-pump-management-system-) | Django (Python) | **Reference for entity model** (attendants, leave, sales reports) | **Reference only** — wrong stack, single-tenant web app, no POS adapter concept, no local-first |
| [hbfawaz112/Petrol-Station](https://github.com/hbfawaz112/Petrol-Station) | Dart/Flutter web + mobile | Cross-platform client UI patterns | **Borrow**: responsive mobile layout ideas. **Skip**: no POS ingest, no reconciliation |
| [Nikhil2408/Petrol-Pump-Management-System](https://github.com/Nikhil2408/Petrol-Pump-Management-System) | HTML/CSS/JS | UI mock | **Skip** |
| [HashiniDivyanjanee/Fuel-Station-Management-System](https://github.com/HashiniDivyanjanee/Fuel-Station-Management-System) | Java (Swing) | OOP entity model reference | **Skip** |
| [6eero/Gas-Station-Database-Management-System](https://github.com/6eero/Gas-Station-Database-Management-System) | Postgres + R | SQL DDL reference for stations, pumps, employees, weekly plans | **Borrow**: the `gas_station_database.sql` schema for cross-checking our entity model (esp. `IMPIANTO`/`POMPA`/`DIPENDENTE` style) |

**Gap observed:** None of the above implement POS *ingest* (they model the business, they don't talk to a real pump/POS). They also all assume a single central server, not local-first. Our adapter layer + local-first stance is the real moat.

### 3.2 Generic POS systems (cross-domain prior art)

| Repo | Stack | Useful for | Verdict |
|---|---|---|---|
| [Open Source POS](https://opensourcepos.org/) (aka osPOS) | PHP / MySQL | **POS workflow reference** (cashier, sales, suppliers, customers) | **Reference only** — wrong stack, hosted model, no local-first |
| [InvenTree](https://github.com/Inventree/Inventree) | Python / Django + plugin system | **Plugin pattern** (we'll mirror this for our POS adapter interface) | **Borrow pattern**: see [InvenTree plugin docs](https://docs.inventree.org/en/latest/plugins/plugins/) — a registered-entry-point plugin registry, plugin settings, per-plugin migrations. We'll port the *concept* to Go (`plugin` package or explicit registry), not the code |
| [StockPilot](https://github.com/echatehdi/StockPilot) (Reddit side project) | SQLite desktop, offline-first | **Direct architectural reference** — small offline-first POS, no cloud, local data only. Almost identical in philosophy to our local core minus the LLM/router | **Strongly review** before we start — closest spiritual prior. Their reconciliation / reporting model is what we should compare our data mart against |
| [QuickStock Sync](https://github.com/SHalimoosavi/SYJ-QuickStock-Sync) | Node/Express + SQLite + PWA | Offline inventory + LAN/cloud sync | **Borrow**: PWA / offline write-queue pattern, CSV/Excel import-export |

### 3.3 Local-first sync engines (we'll *learn from*, not necessarily depend on)

| Project | Approach | Useful for us? | Verdict |
|---|---|---|---|
| [PowerSync](https://www.powersync.co/) | Postgres → SQLite, **version-based reconciliation, no CRDTs** | **High** — our v1 cloud is plain Postgres, our local is SQLite, and we don't have multi-user concurrent writes (single station = single local core) | **Evaluate in Week 1** as the sync backbone for the (dormant) HQ aggregation path. License is source-available self-hosted. **Not required for v1** — we can ship polled-only heartbeats + license check + (opt-in) backup without PowerSync, and adopt it for v2 HQ |
| [ElectricSQL](https://electric.ax/) | Postgres → SQLite via logical replication | Medium — same fit as PowerSync, more ambitious (full active-active) | **Defer** — too heavy for v1 single-station model |
| [CR-SQLite](https://github.com/vlcn-io/cr-sqlite) | SQLite extension with CRDT semantics | Low — we don't have peer-to-peer conflict; server is authoritative for what little we sync | **Skip for v1**; revisit if you ever do "two FuelMind cores at one station" |
| [Zero (Rocicorp)](https://zero.rocicorp.dev/) | Postgres → reactive client store | Low — designed for app frameworks, overkill for our admin/dashboard | **Skip** |

**Our v1 sync is simpler than any of these:** heartbeat POSTs a tiny JSON, GETs license + update manifest. No two-way CRDT needed. We're "local-first" because the local DB is authoritative, not because we need merge logic. We will *deliberately not* over-engineer this.

### 3.4 Pump-controller / serial / POS hardware

| Repo | Useful for | Verdict |
|---|---|---|
| [wmpauli/pumps](https://github.com/wmpauli/pumps), [Ddedalus/syringe-pump](https://github.com/Ddedalus/syringe-pump) | RS-232 / COM-port pump control pattern (different domain — lab pumps, not fuel dispensers) | **Borrow pattern**: serial-port abstraction, error/retry semantics. Real fuel dispensers are not on GitHub (proprietary protocols) — we get the protocol from the vendor's integration manual (handled per vendor in `adapters/`) |
| MDB / vending-bus projects on GitHub | Multi-drop bus protocol for payment terminals, bill validators, dispensers | **Reference only** — most MDB is for vending machines, not fuel dispensers, but the protocol layering is similar |
| `go.bug.st/serial` (Go) | Cross-platform serial-port library for Go | **Adopt** if O1 is serial |

### 3.5 Our differentiator (what OSS does NOT have)

- **Local-first + LLM intent router that explicitly forbids the LLM from computing numbers.** No OSS project ships this constraint. StockPilot, osPOS, InvenTree all rely on static reports and SQL queries only.
- **Hardware-tiered on-prem LLM** for offline NLQ (Natural Language Querying of the data mart).
- **Multi-vendor POS plugin interface** with a single canonical normalized layer (most OSS systems are single-POS or hardcoded).
- **Dormant v2 hooks (HQ multi-station)** built alongside v1 — no OSS has the upgrade-without-rewrite property we need.

---

## 4. Solo-Founder Operating Model

You are one person shipping 5–10 stations in 6 months. The plan is built around this. Key disciplines:

1. **One track at a time per work block.** No "let me also start the WhatsApp bridge" mid-week. The build order is sequential on purpose.
2. **Cut lines are explicit.** Each phase has a "ship-this-or-cut-it" gate. If a phase slips by 1 week, you cut the next phase's stretch goals, not the next phase.
3. **Demo every Friday.** Even if it's a 5-minute screen recording for your own archive, end every week with "what works now that didn't work Monday?"
4. **Solo bottlenecks to flag early:**
   - **POS vendor handshake.** You will need a real station with the v1 POS to develop against. Pre-arrange this in Week 1, or you're stuck.
   - **First pilot station selection.** You need a cooperative owner with reliable internet. Lock this in by Week 12.
   - **Installer testing on a real shop PC.** You can dev on your laptop, but you must test on a representative shop PC (Windows 10/11, low-end, possibly with the POS software already running) before pilot.
5. **Where to get help (no full-time hires, but targeted):**
   - Windows MSI packaging — hire a freelancer for a 2-day job (Week 3 deliverable). This is not where you spend your time.
   - WhatsApp BSP setup (360dialog) — defer to post-pilot unless explicitly needed.
   - Local LLM model selection + prompt templates — use an AI/agent for prompt iteration (Ollama + small model is forgiving).

---

## 5. Repo Structure (proposed)

```
fuelmind/
├── README.md
├── LICENSE                          # pick: Apache 2.0 (preferred) or MIT
├── go.mod
├── go.sum
├── cmd/
│   └── fuelmind-core/               # main entry for the local core Windows service
│       └── main.go
├── internal/
│   ├── config/                      # config load (yaml/toml), hardware tier detect
│   ├── logging/
│   ├── storage/                     # SQLite open/migrate, WAL, encryption-at-rest wiring
│   │   ├── migrations/              # versioned .sql files (golang-migrate format)
│   │   └── sqlite.go
│   ├── posadapter/                  # the plugin interface + registry
│   │   ├── adapter.go               # interface per spec §2.2
│   │   ├── registry.go              # plugin discovery
│   │   ├── raw/                     # shared raw-layer types
│   │   └── vendor_x/                # concrete vendor adapter #1 (Week 3-4)
│   ├── normalizer/                  # raw → normalized
│   ├── mart/                        # materialization job, refresh triggers
│   ├── router/                      # intent classifier + query engine + LLM path
│   ├── alerts/                      # alert engine
│   ├── sync/                        # cloud sync agent (heartbeat, license, updates, WA relay poll)
│   ├── llm/                         # Ollama client + prompt templates + hardware tiering
│   ├── web/                         # HTTP handlers, embedded web assets
│   └── platform/                    # Windows service wrapper, installer hooks
├── web/                             # embedded by go:embed into the binary
│   ├── templates/                   # html/template files
│   └── static/                      # css, js (HTMX + Alpine, no build step)
├── adapters/                        # vendored POS adapter Go modules (or sub-packages)
│   ├── vendor_x/
│   └── vendor_y/                    # added in v1.1
├── cloud/                           # cloud control plane (separate small repo OR same monorepo)
│   ├── schema/
│   │   ├── 001_stations.up.sql
│   │   ├── 002_licenses.up.sql
│   │   ├── 003_heartbeats.up.sql
│   │   ├── 004_updates.up.sql
│   │   └── 005_backups.up.sql
│   ├── api/                         # Go or Node service for license/heartbeat/update endpoints
│   ├── admin/                       # minimal internal admin UI (you-only, for tier flips)
│   └── infra/                       # terraform/pulumi for Hetzner
├── installer/
│   ├── fuelmind.wxs                 # WiX MSI definition
│   └── scripts/
├── docs/
│   ├── ARCHITECTURE.md              # points back to FuelMind_Architecture_Spec.md
│   ├── ADAPTER_AUTHOR_GUIDE.md      # how to write a new POS adapter
│   ├── RUNBOOK.md                   # on-call runbook for you
│   ├── SECURITY.md                  # data-locality claim, encryption details
│   └── PILOT_CHECKLIST.md
├── testdata/
│   ├── pos_samples/                 # sample POS exports for tests (sanitized)
│   └── fixtures/
└── tools/
    ├── hashgen/                     # CLI to compute checksums for update artifacts
    └── dbquery/                     # dev CLI to run ad-hoc SQL against the local DB
```

---

## 6. Sequenced Build Plan (24 weeks / 6 months)

Phase gates: each phase has a **Demo** (what works end-to-end) and a **Cut line** (what we explicitly defer if we're behind). Phases map to spec §12 but pulled tight.

### Phase 0 — Setup (Week 1)
**Goal:** Repo, dev env, CI, and the answer to O1 in hand.
- [ ] Init monorepo (Go module, `go 1.23+`), Apache 2.0 LICENSE
- [ ] GitHub Actions: `go build`, `go test`, `golangci-lint`, `gofmt -s`
- [ ] Editor setup notes (VS Code / GoLand), `Makefile` with `make build`, `make test`, `make run`
- [ ] **Decision: which POS vendor is v1?** (O1) — talk to the owner, get the integration manual or DB read access. If blocked, see "Cut:CSV fallback" below
- [ ] Documentation: `README.md` (one-paragraph project, install + run), `ARCHITECTURE.md` linking to spec

**Demo:** `go run ./cmd/fuelmind-core` starts and prints "FuelMind starting…" on stdout. CI is green on an empty service.

**Cut line:** None — Phase 0 is mandatory.

> **Cut: CSV watch-folder fallback** — superseded by the vendor-agnostic v1 decision (D3). The CSV watch-folder IS the v1 default. No fallback needed.

---

### Phase 1 — POS adapter: `csv_watch` goes live (Weeks 2–5)
**Goal:** Real (or sanitized fixture) transactions hitting `raw_pos_transactions` via the file-watching CSV parser, with the same `csv_watch` adapter working for **any** POS that can export CSV.

> **The v1 adapter is vendor-agnostic** (decision D3, resolved O1). The plugin interface in `internal/posadapter/` already exists from Phase 0. This phase fills in the file-watching + CSV parsing logic and wires the first end-to-end ingest path.

- [ ] `internal/posadapter/csvwatch/`: implement `fsnotify` watcher on the configured folder
- [ ] CSV header validation (must match the documented column set in `testdata/pos_samples/README.md`)
- [ ] Row-by-row parsing with per-row error reporting; one bad row logs an error and continues, doesn't kill the whole file (configurable: strict mode for v1.1)
- [ ] SQLite migrations 001–005 for the raw-layer tables (spec §3.1)
- [ ] Batch insert with a single shared `ingestion_batch_id` per file (use `github.com/google/uuid`)
- [ ] File archive: success → `processed/`, parse error → `failed/`
- [ ] Idempotent re-processing: re-running on `processed/` files is a no-op (dedup on `raw_transaction_id`)
- [ ] SQLite via `modernc.org/sqlite` (pure Go, no CGo) with WAL mode and foreign keys on
- [ ] Structured logging (`slog`) per file: rows, parse errors, duration
- [ ] Per-adapter metrics: rows/sec, error count (Phase 9 will expose these; just log for now)
- [ ] **Contract test extended:** parse `transactions_sample_01.csv`, assert 10 rows land in `raw_pos_transactions` with the expected fields. Parse a malformed CSV, assert it lands in `failed/` with a clear error log
- [ ] End-to-end smoke test: `go run ./cmd/fuelmind-core` → drop a CSV into the watched folder → query `raw_pos_transactions` → see rows

**Demo:** Drop `transactions_sample_01.csv` into the configured folder. Within a second, the file moves to `processed/`, and `sqlite3 fuelmind.db "SELECT * FROM raw_pos_transactions"` returns the 10 rows. The numbers match the source CSV exactly.

**Cut line:** No second adapter. No fuzzy alias matching. No inventory/shift feed in v1 (those arrive via the manual entry UI in v1.1). The CSV watch-folder alone is enough to ship v1 to a pilot station.

---

### Phase 2 — Normalization + canonical vocabulary (Weeks 6–7)
**Goal:** Raw transactions becoming canonical `transactions` rows, regardless of vendor quirks.
- [ ] Migrations 006–010 for normalized tables (spec §3.2: `fuel_products`, `product_aliases`, `transactions`, `inventory_readings`, `shifts`, `customers`, `expenses`)
- [ ] Seed `fuel_products` with `DIESEL`, `PETROL_92`, `PETROL_95` (extensible)
- [ ] Seed `product_aliases` with vendor-specific terms discovered in Phase 1
- [ ] Normalizer: walks a `raw_pos_transactions` batch, applies alias map, unit conversion, currency normalization, writes to `transactions`. **Idempotent** (running it twice produces no dupes — use `raw_transaction_id` as the dedup key)
- [ ] For v1: literal string match on aliases. Fuzzy/embedding-based alias matching → v1.1
- [ ] Tests: same fixture as Phase 1 contract test, but assert on `transactions` not `raw_*`

**Demo:** SQL query: *"show me diesel sales today"* returns a real number, derived from the raw feed through the normalizer. Numbers match the POS's own daily report (modulo known timing differences).

**Cut line:** No fuzzy alias matching. No automatic alias discovery from raw payloads. Hardcoded seed for v1.

---

### Phase 3 — Business data mart + materialization (Weeks 8–9)
**Goal:** Pre-aggregated tables that the dashboard and intent router can read in <10ms.
- [ ] Migrations 011–016 for mart tables (spec §3.3: `daily_sales`, `fuel_margin`, `credit_outstanding`, `cash_variance`, `inventory_variance`, `station_health_score`)
- [ ] Materialization job: runs after every ingestion batch **and** every 15 min on a timer. Refreshes affected `daily_sales`/`fuel_margin`/etc. rows
- [ ] Implement the **FuelMind Score** per spec §3.3 (sales, inventory, cash, credit, data quality, operations, overall)
- [ ] `issues_json` populated with structured issue list (e.g. `{"date": "2026-09-07", "tank_id": "T1", "issue": "inventory_variance_high", "severity": "warn", "value": -2.3}`)
- [ ] Health-check endpoint: `/healthz` and `/readyz` (for Windows service / NSSM)
- [ ] Smoke test: full pipeline POS → raw → normalized → mart, on real data, in <5 seconds total

**Demo:** Query the mart: *"today's total revenue"*, *"this week's margin %"*, *"inventory variance for tank 1 this week"* all return sub-10ms.

**Cut line:** The `station_health_score` algorithm is intentionally simple in v1 (weighted sum of clear-cut thresholds). ML-based anomaly detection → v2.

---

### Phase 4 — Local read-only dashboard (Weeks 10–12) — **SHIPPED**
**Goal:** Owner can open a browser, log in with a PIN, see their station's data.
- [x] HTTP server on `127.0.0.1:<port>`, `embed.FS` for templates and static
- [x] **Login:** single shared PIN (PBKDF2-SHA256, 100k iters, OWASP 2023 minimum). Do NOT rely on LAN-only security
- [x] **Dashboard pages** (server-rendered via `html/template`, **no JS framework in v1** — full HTMX/Chart.js deferred to Phase 9):
  - Today at a glance (revenue, volume, transactions, top product, FuelMind Score with structured issues list)
  - Daily sales table (last 30 days, newest first)
  - Credit outstanding (table)
  - FuelMind Score detail page (6 component breakdown + issues list)
- [x] Login flow: `/setup` (first-run PIN wizard) → auto-redirect to `/` → `/login` on subsequent visits; `/logout` clears the session
- [x] Mobile-friendly (single-column responsive CSS)
- [x] Tests: 8 web tests + 9 auth tests — full setup → login → dashboard → logout flow, wrong-PIN error rendering, empty-state messages on sales, static CSS served with correct Content-Type
- [x] Each page template parsed into its own template set with `base.html` cloned in, so per-page `body` blocks don't collide and the shared chrome wraps cleanly

**Demo:** Owner opens `http://shop-pc:<port>`, sees `/setup` on first run, sets a 4+ char PIN, is auto-logged in to the today page. Logs out → redirected to `/login` → wrong PIN shows "Invalid credentials" → correct PIN lands on the today page with today's revenue, volume, transactions, top product, and the FuelMind Score with its issue list. All numbers come from the mart, never raw tables.

**Cut line:** No editing flows in v1 (no expense entry UI, no manual shift-close UI). Read-only. Owners enter expenses on paper or in their existing tool for v1; this UI is for *visibility*, not data entry. (Adding expense entry is a clean Phase 9 add.) **No HTMX / live polling in v1** — full page reload only. (Adding polling + charts is a clean Phase 9 add.)

**This is the first thing that's sellable on its own**, per spec §12 step 4.

---

### Phase 5 — `station_id`, license check, heartbeat (Weeks 13–14)
**Goal:** Every install gets a `station_id`, pings home, checks license, ships a heartbeat — even though v1 has only one tier.
- [ ] On first run: generate `station_id` (`FM-XXXXX` where XXXXX is a non-sequential random ID, 5 chars, alphanumeric, no collisions), generate per-station API key, store in `local_config` table
- [ ] Cloud: deploy minimal Postgres + API service (Hetzner CX22 + managed Postgres or self-hosted)
- [ ] Cloud schema migrations 001–005 (spec §6.1)
- [ ] Cloud endpoints:
  - `POST /v1/heartbeat` — accepts spec §6.3 payload, returns 200 + license status
  - `GET /v1/license/{station_id}` — returns current tier + features_json
  - `GET /v1/updates/check?version=X&tier=Y` — returns available update manifest or 204
- [ ] Local `sync` package: heartbeat goroutine, default cycle 30 min (configurable, randomized jitter to avoid thundering herd at scale)
- [ ] **Per-station API key** in the `Authorization: Bearer <key>` header. Generated at install, stored only on the local disk, sent over HTTPS only
- [ ] **Offline behavior (must be correct):** if cloud unreachable, log it, keep running everything, retry next cycle. *Nothing* depends on the cloud being up. Test this explicitly by pulling the network.
- [ ] Cloud admin: a single-page internal tool (Go templates, basic auth) that lists stations, shows last heartbeat, lets you flip a license tier or feature flag

**Demo:** Install on a fresh VM/laptop → cloud admin shows the station with a green heartbeat within 30 min → admin toggles a feature flag → next heartbeat, local core sees the change.

**Cut line:** No multi-org / multi-user cloud admin. Just you, with basic auth on a single-page tool. No "Connected" or "HQ" tier features actually active; the schema supports them but flags are false.

---

### Phase 6 — Unattended update mechanism (Weeks 15–16)
**Goal:** You can ship a new version of the local core, and it lands on every pilot station without you flying there.
- [ ] `tools/hashgen` to compute SHA-256 of an update artifact
- [ ] Update artifact = a zipped folder with: new `fuelmind-core.exe`, new migrations, manifest.json (version, checksums, migration list, min hardware tier)
- [ ] Update flow per spec §7:
  1. Heartbeat GETs `/v1/updates/check`
  2. Cloud returns manifest (respects `rollout_pct` for staged rollout)
  3. Local downloads to `staging/` (resumable, retry on flaky internet)
  4. Verifies checksum
  5. Schedules apply for low-traffic window (default 3am local, configurable)
  6. Stops service, swaps binaries, runs migrations (with **up + down** for every migration), restarts
  7. Self-check: HTTP `/healthz` returns 200 within 60s
  8. If self-check fails: **automatic rollback** to previous version, report failure in next heartbeat
- [ ] Migrations via `golang-migrate`, every migration has `_up.sql` and `_down.sql`
- [ ] Rollback safety: keep previous binary in `previous/` for N=72 hours; keep DB backup pre-migration
- [ ] Test: ship a no-op update (e.g. version bump + log message change) to your dev install, watch it apply at 3am, watch it rollback if you deliberately break the self-check

**Demo:** Push a v0.2.0 build, all 5–10 pilot stations update themselves within 24 hours without you touching them. Push a deliberately broken v0.2.1, all stations roll back, you see the failure in cloud admin.

**Cut line:** No delta updates (full artifact each time is fine for v1; you have 5–10 stations, not 5,000). Bandwidth optimization → v2.

> **This is the most operationally important phase.** Everything after this is product polish; everything before this is what makes the business runnable without you visiting every station. Per spec §12.

---

### Phase 7 — Local LLM + hardware tiering (Weeks 17–19)
**Goal:** Owner can ask the dashboard "why did profit fall this week?" and get a real answer — generated on their own machine, no cloud.
- [ ] First-run hardware detection (CPU cores, RAM, GPU presence, VRAM) → tier (Basic/Standard/Enhanced/Pro), stored in `local_config`
- [ ] **Bundle Ollama install** as part of the FuelMind installer (or guide user through Ollama install on first run if Basic tier and user wants upgrades)
- [ ] **Model selection per tier:**
  - Basic: no LLM. Intent router returns templated canned text for known issue types
  - Standard (~1–3B): e.g. `qwen2.5:1.5b` or `llama3.2:3b` (test what fits and answers well on a 8GB-RAM CPU machine)
  - Enhanced (6–8GB VRAM): e.g. `qwen2.5:7b` or `llama3.1:8b`
  - Pro (16GB+ VRAM): e.g. `qwen2.5:14b` or larger
- [ ] `llm` package: Ollama HTTP client, model warm-up on first use, prompt templates
- [ ] **Hard rule** (spec §4): the LLM receives pre-computed values from the data mart, never raw DB, and the system prompt says "Only use the numbers provided. Never calculate or estimate."
- [ ] Intent router v2:
  - **Standard path:** regex/keyword match (no LLM, no network)
  - **Parameterized path:** templated SQL against the mart (no LLM)
  - **Complex/open-ended path:** LLM with the data-mart context window
- [ ] Test prompts against the LLM with intentionally bad data (missing context) — confirm it says "I don't have that data" instead of making up a number
- [ ] Latency target: standard/parameterized < 50ms; LLM path < 8s on Standard tier

**Demo:** Type "why did profit fall this week?" into the dashboard. Get a paragraph explanation that names the actual cost change, the actual volume change, and the actual margin % change — no hallucinated numbers. Test on Basic tier and confirm it returns the canned explanation instead of trying to call the LLM.

**Cut line:** No voice input. No multi-language LLM responses in v1 (English only; Urdu translation in a UI layer, not in the LLM, deferred to v1.1).

---

### Phase 8 — Pilot prep + first install (Weeks 20–22)
**Goal:** First paying/cooperating station is running on their shop PC, on their POS, with the installer working end-to-end.
- [ ] Windows installer (WiX MSI) — *this is a 2-day freelancer job, not your time*
  - Installs `fuelmind-core.exe` as a Windows service under `NT AUTHORITY\LocalService`
  - Creates `C:\ProgramData\FuelMind\` for the SQLite DB, logs, staging
  - Optional: bundles Ollama (size-dependent; may be a separate download step)
  - First-run wizard: PIN setup, POS connection test, hardware tier display, "send test heartbeat" button
- [ ] **On-site install** (you or remote desktop with the owner):
  - Install the MSI
  - Walk through first-run wizard
  - Run the dashboard side-by-side with the POS's own report for 1 day
  - Verify the numbers match
  - Leave the owner with: a 1-page "how to log in" sheet, your phone number, a feedback channel
- [ ] Runbook: `docs/RUNBOOK.md` (you-only) covering: re-install, reset PIN, re-issue license, rollback update, recover from corrupt DB, "owner says the number is wrong, what do you check"
- [ ] Pilot checklist: `docs/PILOT_CHECKLIST.md` (what "ready to ship to station #N" means)

**Demo:** A real shop PC at a real station, running FuelMind, showing the same numbers as the POS's own daily report, with the owner able to log in from their phone on the LAN.

**Cut line:** Zero "polish" features. No light/dark theme. No export-to-Excel. Those are v1.1.

---

### Phase 9 — Pilot loop + remaining v1 steps (Weeks 23–24)
**Goal:** 5–10 stations running, feedback incorporated into v1.1, cloud admin + license-tier flips work.
- [ ] Onboard 4–9 more stations (one per week is realistic; the install wizard + your runbook is what scales you here)
- [ ] Cloud admin: add the ability to flip a station from `private` → `connected` tier without engineering involvement (spec §6.2)
- [ ] Optional cloud backup (customer opt-in) per spec §10 — *defer to v1.1 if time-pressed*; the local-only backup is sufficient for v1
- [ ] WhatsApp bridge per spec §8 — **defer to v1.1** unless a pilot owner specifically demands it. Reason: BSP setup, billing, and Meta approval are non-trivial and don't block pilot value
- [ ] Security pass per spec §9: SQLCipher encryption-at-rest on the local DB, OS least-privilege service account, local integrity check
- [ ] Telemetry consent copy review (O10) and disclosure shown at first install

**Demo:** You can flip a flag in cloud admin → station picks it up on next heartbeat. Pilot stations have been running for 2–4 weeks with no manual intervention. You've shipped at least one update via the unattended update path and it landed cleanly.

**Cut line:** v1 ships with: 1 POS adapter (deep), 1 dashboard (read-only), deterministic + LLM intent router, station_id + heartbeat + license, unattended updates, security baseline. v1.1 adds: more POS adapters, expense entry, WhatsApp, optional cloud backup, HQ aggregation hooks actually exercised.

---

## 7. Per-Component Technical Plan

### 7.1 Local core service (Go)
- Single `cmd/fuelmind-core/main.go`, no CGo, no external runtime
- Dependencies: `modernc.org/sqlite` (pure-Go SQLite), `golang-migrate/migrate`, `go.bug.st/serial` (if any adapter is serial), `a-h/templ` (optional, for type-safe templates — or stick with `html/template`)
- Windows service via `kardianos/service` package (cross-platform service abstraction; NSSM-compatible)
- Logging: `log/slog` to file under `C:\ProgramData\FuelMind\logs\` with rotation
- Config: TOML in `C:\ProgramData\FuelMind\config.toml`, hot-reloadable for non-critical keys
- Crash recovery: panic handler, structured shutdown on SIGTERM, Windows event log integration

### 7.2 POS adapter interface (Go)
```go
package posadapter

type RawTransaction struct {
    PosSourceID    string
    ExternalID     string
    OccurredAt     time.Time
    Payload        []byte  // original, untouched
}

type RawInventory struct {
    TankSourceID string
    OccurredAt   time.Time
    Payload      []byte
}

type RawShift struct {
    ShiftSourceID string
    OccurredAt    time.Time
    Payload       []byte
}

type Config map[string]any  // adapter-specific

type Connection interface {
    Close() error
    Healthy() error
}

type Adapter interface {
    Name() string                                              // e.g. "vendor_x_db"
    Connect(ctx context.Context, cfg Config) (Connection, error)
    PullTransactions(ctx context.Context, since time.Time, batchSize int) ([]RawTransaction, error)
    PullInventorySnapshot(ctx context.Context) ([]RawInventory, error)
    PullShiftData(ctx context.Context, since time.Time) ([]RawShift, error)
}
```
Adapters register themselves via `init()` and the registry picks the active one(s) from config. Each adapter ships in its own sub-package so the binary stays modular.

### 7.3 Three-layer data model
Direct port of spec §3 schema. Key implementation notes:
- SQLite: `PRAGMA journal_mode=WAL;`, `PRAGMA foreign_keys=ON;`, `PRAGMA synchronous=NORMAL;` (safe with WAL)
- All migrations: numbered `NNN_name.up.sql` + `NNN_name.down.sql`
- Materialization is **idempotent** — re-running for a date replaces the row
- `ingestion_batch_id` is a UUID per batch; raw + normalized rows both reference it for trace-back

### 7.4 Intent router
```
Owner message
    ↓
IntentClassifier  (regex → small classifier → LLM fallback)
    ↓
    ├─── known: "today's sales" → DirectLookup(daily_sales)
    ├─── parameterized: "sales for DIESEL last 7 days" → TemplatedSQL
    └─── complex: "why did profit fall?" → LLMExplain(mart rows + issues_json)
    ↓
ResponseFormatter → text + optional chart data
    ↓
Dashboard card / WhatsApp reply
```
The classifier itself is a 3-tier waterfall (cheapest first) per spec §4. The "LLM fallback" path in step 1 is the classifier-as-LLM (tiny prompt, structured JSON output) used *only* if regex + small classifier both miss.

The hard rule — **LLM never computes** — is enforced by:
1. The system prompt (loud, repeated)
2. The LLM context contains only pre-computed values
3. A post-generation check (regex) that flags if the LLM produced a number not in its context — if so, regenerate or fall back to templated

### 7.5 Local dashboard
- Server: Go `net/http` with `html/template`
- Templates: one base layout, per-page content
- Interactivity: HTMX (`hx-get`, `hx-trigger="every 30s"`) for live updates; Alpine.js for tiny client state (modals, dropdowns); Chart.js (CDN or vendored) for charts; no build step
- Auth: cookie-based session after PIN entry, session ID in `httpOnly` cookie, 24h expiry
- Mobile-first CSS (the owner checks on their phone)

### 7.6 Cloud control plane
- Hetzner CX22 (4GB RAM, €4.5/mo) or similar for the API service
- Managed Postgres (Hetzner also offers this, €5–10/mo) for the control-plane DB
- Object storage: Hetzner Object Storage (S3-compatible) for update artifacts
- API: Go (chi router), JWT-less (just station API keys), TLS via Caddy or Traefik
- Admin UI: single Go-templated page, basic auth (htpasswd), deployed at `admin.fuelmind.yourdomain`

### 7.7 Unattended update
Detailed in Phase 6 above. Key safety property: **every migration has a tested down-migration, and the previous binary is kept for 72h post-update.**

### 7.8 Security baseline (Phase 9)
- Local DB: SQLCipher (compiled into `modernc.org/sqlite`'s `sqlite_sci` build, key from a local config file with restricted ACL)
- Per-station API key: 256-bit random, generated at install, shown once in the first-run wizard, not recoverable (only re-issue)
- Service account: `NT AUTHORITY\LocalService` (no admin), data folder ACL restricts write to LocalService only
- Local dashboard: PBKDF2-hashed PIN, 8+ char minimum, locked after 5 failed attempts for 15 min
- Cloud: TLS 1.2+, per-station rate limit (10 req/min on heartbeat), Caddy-managed certs

### 7.9 Backup (Phase 9)
- **Local auto-backup:** nightly copy of the SQLite file to `C:\ProgramData\FuelMind\backups\`, keep last 7. Cron via Windows Task Scheduler, triggered by the service
- **Optional cloud backup (deferred to v1.1):** client-side encrypted (libsodium secretbox), customer passphrase derives key, uploaded to object storage. Spec §10 retention policy

---

## 8. v1 Release Criteria ("done" for pilot)

A station is "on FuelMind v1" when **all** of the following are true.
Code state as of 12 Sep 2026 in brackets — what is left is per-station
verification, not engineering.

- [x] FuelMind Windows service runs on the shop PC, auto-starts on boot
      *(service + launcher implemented and unit-tested; needs one real
      install to confirm on a shop PC)*
- [x] Local dashboard accessible at `http://shop-pc:8765` from a phone on the same LAN
      *(binds all interfaces; the MSI adds a local-subnet firewall rule)*
- [x] PIN login works *(plus lockout after 5 attempts, loopback-only first PIN)*
- [ ] POS adapter is pulling real transactions, verified against the POS's own daily report for 3 consecutive days with < 1% variance
      *(per-station verification; the fixtures and E2E test match to the paisa)*
- [~] All four key dashboard views render with real data: today, sales, margin, inventory
      *(today, sales and credit are real; margin needs a purchase-price
      feed and inventory needs a tank feed — both out of v1 scope and
      shown as unmeasured rather than faked)*
- [x] Heartbeat is reaching cloud admin every 30 min *(needs a deployed cloud; `cmd/fuelmind-devcloud` proves the path)*
- [x] License is `private` tier active, `connected` tier flags present but false
- [x] One unattended update has been applied and verified
      *(exercised end-to-end locally: check → download → verify → stage →
      handoff → health check → commit, plus the rollback path)*
- [x] Backup is configured (local at minimum) *(nightly `VACUUM INTO` at 02:00, 7 kept, plus pre-migration copies)*
- [ ] Owner has a printed runbook page with your contact *(write the one-pager per station)*
- [x] You have a daily-low-touch monitoring routine *(`fuelmind-setup status` on the PC; heartbeats in cloud admin once deployed)*

**Blocking for the first install:** deploy the cloud service (only the
schema and the dev server exist), and run the MSI on a representative
Windows 10/11 shop PC once.

A v1 release is **5–10 stations all passing the above for 2+ weeks with no manual intervention required of you beyond heartbeat-level monitoring**.

---

## 9. Risk Register

| # | Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|---|
| R1 | POS vendor handshake stalls or vendor changes protocol | Med | High | Build CSV watch-folder adapter as the fallback; always test against fixtures, not just live |
| R2 | First pilot station's shop PC is too old/slow for the local LLM | High | Med | Hardware tiering handles this — Basic tier = no LLM, fully functional read-only dashboard |
| R3 | You get stuck in support firefighting before 5 stations are live | High | High | Unattended update is the single biggest leverage point; Phase 6 must not slip |
| R4 | SQLCipher integration breaks SQLite (modernc/sqlite_sci is less battle-tested than the C version) | Med | High | Use filesystem-level encryption (BitLocker/EFS on the data folder) as the primary; SQLCipher as belt-and-suspenders, opt-in |
| R5 | Ollama install on shop PCs is non-trivial (download size, install path) | Med | Med | Bundle Ollama into the MSI for tiers Standard+, with a one-line first-run check; document manual install in the runbook |
| R6 | Local LLM gives a wrong-number answer to the owner | Med | High (trust) | Post-generation number-validation check (regex against context-provided numbers); on fail, fall back to templated response. Spec §4 hard rule enforced in code |
| R7 | Update mechanism rolls back incorrectly, leaving station offline | Med | High (trust) | Test the rollback path explicitly in Phase 6; keep previous binary for 72h; alert on rollback in cloud admin |
| R8 | Internet at pilot stations is too patchy for the unattended update to land in 24h | Med | Med | Updates are best-effort; spec §7 explicitly says "offline case is correct behavior." Plan does not depend on updates landing in <24h |
| R9 | WhatsApp BSP account approval takes longer than expected | Med | Low | WhatsApp is v1.1, not v1 |
| R10 | You underestimate the v1 → pilot hand-holding and the 6-month timeline slips | Med | Med | Cut lines in each phase; the dashboard alone is shippable at end of Phase 4 (read-only, no LLM, no cloud) |

---

## 10. Next 2-Week Action Checklist

This is what you do Monday morning:

- [ ] **Day 1:** Create the repo (`fuelmind/`) on your git host, push the structure in §5 with a README, an empty `cmd/fuelmind-core/main.go` that prints "FuelMind starting…", and a working GitHub Actions CI (`go build`, `go test`, lint). This alone is a 4-hour job.
- [ ] **Day 2:** Decide and answer **O1** (v1 POS vendor + access method). If you can't decide by EOD, fall back to the CSV watch-folder adapter (Cut:CSV fallback). Either way, the Phase 1 work is unblocked.
- [ ] **Day 3:** Provision Hetzner (or your chosen host): a CX22 VM, a managed Postgres, an object storage bucket. ~30 min. *Defer this to Week 5 if you want; it's not blocking until Phase 5.*
- [ ] **Day 4:** Stand up the first POS adapter interface skeleton (`internal/posadapter/adapter.go`, `registry.go`, one stub adapter). Even without the real vendor integration, the interface design is the highest-leverage code review you'll get on the project — do it before you write the first concrete adapter.
- [ ] **Day 5:** Write the contract test harness (`testdata/pos_samples/`, `internal/posadapter/contract_test.go`). This is what lets you add vendor #2 in v1.1 without re-breaking vendor #1.
- [ ] **End of Week 1:** Demo per Phase 0: `go run ./cmd/fuelmind-core` prints its startup line, CI green, repo structure matches §5.
- [ ] **End of Week 2:** Phase 1 half-done: adapter interface + 1 concrete adapter (or CSV watch-folder fallback) ingesting into `raw_pos_transactions` against a fixture file.

---

## 11. Out of Scope for v1 (explicit deferral list)

To prevent scope creep in solo execution, the following are **explicitly not v1**:

- Editing flows in the dashboard (expense entry, manual shift close) — read-only v1
- More than 1 POS adapter (deep one, then add more post-pilot)
- Multi-org / multi-user cloud admin
- Active Connected or HQ tier features (schema + flags present, dormant)
- WhatsApp bridge (v1.1)
- Optional cloud backup (v1.1)
- HQ aggregation service (v2, but build-order v1 step 11 makes the hooks easy)
- Multi-language UI (English only v1; Urdu later)
- Voice input to the intent router
- ML-based anomaly detection in the FuelMind Score
- Mobile native apps (mobile-friendly web dashboard is enough v1)
- Delta updates (full artifact per update v1)

---

## 12. Appendix — Cross-references to the spec

| Spec section | This plan's coverage |
|---|---|
| §1 System overview | §5 repo structure, §7 per-component |
| §2.1 Tech stack | §1 confirmed decisions D2/D4/D5/D6/D7 |
| §2.2 POS adapter | §7.2 + Phase 1 |
| §2.3 Raw → Normalized → Mart | §7.3 + Phases 1/2/3 |
| §3 Database schema | §7.3 + Phases 1/2/3 |
| §4 Intent router | §7.4 + Phase 7 |
| §5 Hardware tiering | §7.5 (LLM) + Phase 7 |
| §6 Cloud control plane | §7.6 + Phase 5 |
| §7 Update mechanism | §7.7 + Phase 6 |
| §8 WhatsApp | §11 (deferred to v1.1) |
| §9 Security | §7.8 + Phase 9 |
| §10 Backup & DR | §7.9 + Phase 9 |
| §11 Cost estimate | §7.6 (Hetzner: €10–25/mo total for v1) |
| §12 Build order | §6 (same order, scoped to solo/5–10) |

---

*End of plan. Open decisions in §2 are the only blockers; everything else can proceed in parallel. If you want, I can produce the Phase 0 starter code (Go module skeleton, GitHub Actions CI, adapter interface stub) right now so you're unblocked Monday.*
