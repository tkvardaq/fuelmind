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
| `cmd/fuelmind-setup/` | support tool: `status`, `reset-pin`, `test-heartbeat`, `remote-ask on|off` |
| `cmd/fuelmind-devcloud/` | the control plane as a runnable dev server |
| `internal/posadapter/` | the POS plug-in contract, registry and the `csv_watch` adapter |
| `internal/storage/` | SQLite (pure Go), versioned migrations, all queries |
| `internal/normalizer/`, `internal/mart/` | raw → canonical → data mart |
| `internal/web/` | the dashboard (server-rendered, no JS framework) |
| `internal/auth/` | PIN login (PBKDF2-SHA256, sessions, lockout) |
| `internal/sync/`, `internal/update/`, `internal/version/` | heartbeat, licence, unattended updates |
| `internal/llm/` | the intent router (deterministic answers first, Ollama for the rest) |
| `internal/ask/` | one answering service shared by the dashboard and the relay |
| `internal/relay/` | answers questions sent from the owner's phone (WhatsApp / SMS) |
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

For a dashboard with something in it, generate a week of sales ending
today — three products, the usual cash/card/credit mix, two rush hours,
and one credit customer who stopped paying:

```powershell
go run ./tools/gendata -out $env:USERPROFILE\.fuelmind\pos_drop -days 7
```

Add `-bad` for a file of deliberately broken rows, to watch them land in
`pos_drop\failed\` with a reason. The generator is seeded, so the same
flags always produce the same figures and you can check a number on the
dashboard against the CSV that produced it.

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

## Asking from your phone (WhatsApp or SMS)

The dashboard has an Ask box on the Today page. Away from the station,
the owner can ask the same questions by WhatsApp or text message.

Nothing listens on an inbound port. The station polls the control plane,
answers from its own data, and posts the answer back. It is off until the
owner turns it on in **Settings -> Ask from your phone** (or with
`fuelmind-setup remote-ask on`), because the answer — today's revenue, for
example — travels through the control plane.

Point a WhatsApp/SMS provider (Twilio, 360dialog) at:

```
POST https://<control plane>/v1/whatsapp/webhook?station_id=FM-XXXXXXX&token=<webhook token>
```

It reads the usual `From` and `Body` form fields and replies with TwiML,
which the provider sends straight back to the sender. Support can ask the
same way through `POST /v1/admin/messages`. Every answered question is
logged on the station and shown at the bottom of Settings.

End to end on one machine:

```powershell
.\dist\fuelmind-devcloud.exe -db "$env:FUELMIND_DATA_DIR\fuelmind.db" `
  -admin-token admintok -webhook-token hooktok
.\dist\fuelmind-setup.exe remote-ask on
curl.exe -X POST "http://127.0.0.1:8080/v1/whatsapp/webhook?station_id=FM-XXXXXXX&token=hooktok" `
  --data-urlencode "From=whatsapp:+923001234567" --data-urlencode "Body=how much did we sell today?"
```

## Messages the station sends you

The station can also start the conversation. Turn it on in **Messages**,
enter the number, and it sends:

- an **evening summary** at the hour you pick — revenue, litres, sales
  count, yesterday for comparison, credit outstanding and the score;
- **alerts**, at most one of each a day: the score dropped below 70, a
  credit balance has not moved in 30 days, or the POS has not exported
  anything for six hours. The last one matters most — every other figure
  goes quietly stale when the export stops.

It is off until you switch it on, it only ever writes to the one number
you entered, and every message it sends is listed on the same page with
its delivery status.

Delivery uses [whatsmeow](https://github.com/tulir/whatsmeow), the
open-source Go implementation of the WhatsApp Web protocol. Press **Link
my phone** and scan the QR code with WhatsApp -> Settings -> Linked
devices, the same way WhatsApp Web is linked. That avoids Meta business
verification, a BSP account and per-message billing, which is what kept
this feature out of v1. The trade-off is that whatsmeow is an unofficial
client: WhatsApp does not support it, and an account that sends
unsolicited volume can be banned. FuelMind messages one number about that
owner's own station, which is ordinary personal use.

Figures in a message come from the same pre-computed mart context the
dashboard and the Ask box use, so a message can never disagree with the
page. When the phone is not linked, or the connection is down, messages
wait in the outbox and go out when it recovers — nothing is lost and
nothing is invented.

## What the owner enters by hand

Two numbers cannot come from a POS export, so the dashboard asks for them:

- **Settings -> what you pay per litre.** Margin is revenue minus what the
  fuel cost. Without a purchase price the Margin page says so instead of
  showing revenue as profit. A price applies from its date forward and
  past days are recalculated.
- **Credit -> customer -> record a payment.** Credit balances go down when
  a customer pays, and the overdue counter follows the oldest sale that is
  still unpaid.

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

Margin, credit repayments and remote questions have since been added:
the owner enters purchase prices and payments in the dashboard, and can
ask by WhatsApp or SMS through the relay.

Known gaps, all deliberate for v1:

- No tank or shift feed, so the inventory and cash parts of the FuelMind
  Score are shown as unmeasured and left out of the total rather than
  scored as perfect.
- No expense entry, so "margin" is fuel margin, not net profit.
- One POS adapter (`csv_watch`). The plug-in contract is there for more.
- The production control plane is not built: this repo has its Postgres
  schema and `fuelmind-devcloud`, which is the same handler set in memory.
- The MSI has been built and validated but not yet installed on a real
  shop PC.
