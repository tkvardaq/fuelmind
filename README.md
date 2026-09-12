# FuelMind

> Local-first fuel station management. The owner's data stays on the shop
> PC; the cloud is a thin control plane for licensing, updates and
> (opt-in) health monitoring.

FuelMind watches a folder for the CSV exports a POS already produces,
turns them into canonical transactions, pre-aggregates them into a small
data mart, and serves the owner a dashboard on the station network.
Everything works with the internet unplugged.

## What is in this repo

| Path | What it is |
|---|---|
| `cmd/fuelmind-core/` | the local core: ingest, normalize, mart, dashboard, heartbeat, updates |
| `cmd/fuelmind-launcher/` | the Windows service: supervises the core, applies updates, rolls back |
| `cmd/fuelmind-setup/` | support tool: `status`, `reset-pin`, `test-heartbeat` |
| `cmd/fuelmind-devcloud/` | the control plane as a runnable dev server |
| `internal/posadapter/` | the POS plug-in contract, registry and the `csv_watch` adapter |
| `internal/storage/` | SQLite (pure Go), versioned migrations, all queries |
| `internal/normalizer/`, `internal/mart/` | raw → canonical → data mart |
| `internal/web/` | the dashboard (server-rendered, no JS framework) |
| `internal/auth/` | PIN login (PBKDF2-SHA256, sessions, lockout) |
| `internal/sync/`, `internal/update/`, `internal/version/` | heartbeat, licence, unattended updates |
| `internal/llm/` | the intent router (deterministic answers first, Ollama for the rest) |
| `cloud/` | control-plane Postgres schema |
| `installer/` | WiX MSI source and `build.ps1` |

## Try it in five minutes

```powershell
$env:FUELMIND_DATA_DIR = "$env:USERPROFILE\.fuelmind"
go run ./cmd/fuelmind-core
```

Open <http://localhost:8765/>, set a PIN, then drop a sample export into
the watch folder:

```powershell
Copy-Item testdata\pos_samples\transactions_sample_01.csv $env:USERPROFILE\.fuelmind\pos_drop\
```

The file moves to `pos_drop\processed\` within a second and the numbers
appear on the dashboard. (The samples are dated 2026-09-07, so they show
up under Sales rather than Today.)

## Configuration

Everything has a working default; these override it.

| Variable | Default | Purpose |
|---|---|---|
| `FUELMIND_DATA_DIR` | `%ProgramData%\FuelMind` | database, logs, POS drop folder, versions |
| `FUELMIND_PORT` | `8765` | dashboard port |
| `FUELMIND_BIND` | `0.0.0.0` | listen address; `127.0.0.1` restricts to the shop PC |
| `FUELMIND_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `FUELMIND_CLOUD_URL` | *(unset)* | control plane; unset means no network calls at all |
| `FUELMIND_SYNC_CYCLE` | `30m` | heartbeat interval (1m–6h) |
| `FUELMIND_HARDWARE_TIER` | detected | `basic`, `standard`, `enhanced`, `pro` |
| `FUELMIND_OLLAMA_URL` | `http://127.0.0.1:11434` | local LLM for open-ended questions |
| `FUELMIND_UPDATE_WINDOW` | 02:00–05:00 | `any` applies updates as soon as they are staged |
| `FUELMIND_SUPERVISED` | *(set by the launcher)* | enables update handoff and stdin shutdown |

## Build

```powershell
go build ./...            # everything
go test ./...             # the whole suite
make windows              # dist\FuelMindCore.exe + launcher + setup
powershell -File installer\build.ps1 -Version 1.1.0
```

`build.ps1` produces the three binaries, the update artifact
(`fuelmind-core-<version>.zip` plus its SHA-256) and `FuelMind-<version>.msi`.

## How an install fits together

```
%ProgramFiles%\FuelMind\        fuelmind-launcher.exe (the service)
                                FuelMindCore.exe      (adopted on first start)
                                fuelmind-setup.exe    (support tool)
%ProgramData%\FuelMind\
  fuelmind.db                   the station's data
  pos_drop\                     POS exports land here (processed\, failed\)
  versions\<version>\           installed cores; the launcher picks one
  current.json previous.json pending.json
  backups\                      nightly copies, 7 kept
  logs\                         core.log, launcher.log (rotated at 10 MB)
```

The service runs as `NT AUTHORITY\LocalService`. The launcher starts the
core, restarts it with backoff, and after an update checks `/healthz` and
rolls back to the previous version if the new one does not answer.

## Testing the cloud path locally

```powershell
# 1. Register the station that is already on this PC and publish a release
go run ./cmd/fuelmind-devcloud -db $env:USERPROFILE\.fuelmind\fuelmind.db `
    -release 1.1.0=dist\fuelmind-core-1.1.0.zip -admin-token dev

# 2. Point the core at it
$env:FUELMIND_CLOUD_URL = "http://127.0.0.1:8080"
$env:FUELMIND_SYNC_CYCLE = "1m"
go run ./cmd/fuelmind-core
```

Heartbeats appear in the devcloud log. With `FUELMIND_UPDATE_WINDOW=any`
and the launcher running, the update is downloaded, verified, staged and
applied.

## Conventions

- **Licence:** Apache 2.0
- **Go:** 1.23+, pure Go only (no CGo), so the Windows build is one file
- **Migrations:** `NNN_name.up.sql` + `.down.sql`, applied in a
  transaction, with an automatic pre-migration database copy
- **Logging:** `log/slog` only
- **Before pushing:** `go test ./...`, `gofmt -s -l .`, `go vet ./...`
  (CI runs the same on Linux and Windows)

## Status

Phases 0–9 of [`FUELMIND_BUILD_PLAN.md`](./FUELMIND_BUILD_PLAN.md) are
implemented: ingest, normalization, mart, dashboard, station identity and
heartbeat, unattended updates with rollback, hardware tiering with the
intent router, the MSI, and the security pass (PIN lockout, LAN exposure
with loopback-only first-run setup, telemetry consent, least-privilege
service account).

Known gaps, all deliberate for v1: no inventory or cash feed (those score
components are shown as unmeasured), no purchase prices so margin is
revenue only, no expense entry, no WhatsApp bridge, and the production
cloud service is not built yet — only its schema and the dev server.
