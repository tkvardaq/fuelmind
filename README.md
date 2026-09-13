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

## Getting data in

FuelMind does not read the pumps. It reads what the POS or back-office
system already exports, so the **Data** page in the dashboard is where an
owner connects the two. It offers three ways in, and they all end up in
the same place — one parse, one normalization, one mart — so a figure
never depends on which door its data came through.

- **Watch a folder.** Point FuelMind at the folder the POS already writes
  to and nothing has to be copied by hand. Any number of folders can be
  watched; each export is read once and moved into `processed\` beside
  it. A path that does not exist, is a file, or cannot be written to is
  refused when it is entered, with the reason — and a folder that stops
  being reachable later (a network share that goes down) is shown as
  **Not reachable** rather than quietly reading nothing.
- **Import a file.** For an export on a USB stick or in an email.
- **Enter a sale by hand.** For when the POS is down or a pump was
  written up in the day book. A hand-entered sale is stored under the
  `MANUAL` source, so it is always visible as such in the data.

A file whose rows are not all readable no longer costs the whole day: the
good rows are ingested and the rest are written out as
`<name>.rejected.csv` — the same columns plus a `fuelmind_error` column
saying what was wrong with each one. Correct those rows and drop the file
back in. Only a file-level problem (no header, or a missing required
column) sends everything to `failed\`.

The **What has arrived** table on the same page lists every ingestion —
when, from where, how many rows were read, added and rejected — so "no
sales today" can be told apart from "the export stopped three days ago".

## Asking the station a question

Every FuelMind station answers a fixed set of questions with no model and
no network at all: today's and yesterday's sales, the last 7 days, sales
by product, credit outstanding, fuel margin, the purchase price on
record, and the FuelMind Score. Send **help** to see the list.

Three places to ask, all sharing one answering service so they can never
disagree:

- **The Ask box** on the Today page.
- **The station's own WhatsApp.** Once a phone is paired in **Messages**,
  the owner can message the station's WhatsApp number and it answers from
  the shop PC. This needs no control plane, no SMS gateway and no inbound
  port — the station is already connected to WhatsApp to send the evening
  summary, and it listens on the same connection. Only the registered
  owner number is answered; a message from any other number is ignored
  (and logged, so the owner can see it happened).
- **A WhatsApp/SMS gateway through the control plane**, for a fleet.

### Questions in your own words

A question like "why were takings down yesterday?" needs a model.
**Settings → Answering questions in your own words** offers:

| Choice | What it means |
|---|---|
| Set questions only | The fixed answers. Nothing leaves the PC. The default on a Basic-tier PC. |
| A model on this PC (Ollama) | Answers in your own words, locally. Nothing leaves the shop. |
| OpenAI-compatible API | A hosted model, with an API key. Works with OpenAI, and with Groq, OpenRouter, Together, vLLM and others by changing the address. |
| Anthropic API | A hosted Claude model, with an API key. |

The key is stored in the station database (in the ACL-restricted data
folder), shown masked, and never sent to the control plane or included
in telemetry. Saving over plain `http://` to a remote host is refused,
because the key would travel in the clear. **Test** checks the key works
before the owner relies on it.

What a hosted provider receives is the question plus the pre-computed
figures needed to answer it — revenue, litres, credit total, margin.
Individual transactions and customer phone numbers are never sent. Every
answer still goes through the same grounding check: a model that states a
number it was not given has its answer thrown away rather than shown.

Changing any of this takes effect on the next question; no restart.

## Who did what

Every person at the station gets their own sign-in under **People**, with
their own PIN and one of two roles:

- **Owner** — everything, including Settings, People and the data sources.
- **Staff** — record what happened on their shift: enter a sale, record a
  credit repayment, see the figures.

**Activity** then answers "who made this change?": who entered a sale,
who recorded a repayment, who set a purchase price, who added a watch
folder, who signed in, and who got a PIN wrong. Entries keep the name the
person had at the time, so removing an account never rewrites what it
did. A station that upgrades from the single shared PIN keeps that PIN;
the existing account becomes the owner.

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
| `FUELMIND_UPDATE_INTERVAL` | `30m` | how often to check for a new version (1m–24h); shorten it to watch an update happen |
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
  pos_drop\                     the built-in drop folder (processed\, failed\);
                                more folders can be added on the Data page
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
POST https://<control plane>/v1/whatsapp/webhook?token=<webhook token>
```

It reads the usual `From` and `Body` form fields and replies with TwiML,
which the provider sends straight back to the sender.

Two separate things are checked, and both matter. The **token**
authenticates the gateway: it is shared by every station on the control
plane, so it says "this really came from our SMS provider" and nothing
more. The **sending phone number** authenticates the person, and it is
what selects the station. A number that is not registered to a station is
refused, and a `station_id` in the query is only ever cross-checked
against the number's own station, never used to choose one — a station id
is printed on a dashboard and handed to a provider, so it is an
identifier, not a secret. Inbound messages are also rate limited per
sender.

Register the owner's number with `-owner-number` (repeatable; use
`STATION_ID=NUMBER` when running more than one station). Numbers are
matched in canonical form, so a number registered as `0300 1234567`
matches a message from `+92 300 1234567`.

Support can ask through `POST /v1/admin/messages`. Every answered
question is logged on the station and shown at the bottom of Settings.

End to end on one machine:

```powershell
.\dist\fuelmind-devcloud.exe -db "$env:FUELMIND_DATA_DIR\fuelmind.db" `
  -admin-token admintok -webhook-token hooktok -owner-number +923001234567
.\dist\fuelmind-setup.exe remote-ask on
curl.exe -X POST "http://127.0.0.1:8080/v1/whatsapp/webhook?token=hooktok" `
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
- No expense entry, so "margin" is fuel margin, not net profit. Margin is
  only reported for days a purchase price is recorded for, and a weekly
  figure says how many of the seven days it actually covers rather than
  setting a whole week of revenue against part of a week of cost.
- One POS adapter (`csv_watch`). The plug-in contract is there for more.
  Every station whose POS can export CSV is covered by it — which is
  essentially all of them — and anything it cannot reach can be entered
  by hand on the Data page.
- The production control plane is not built: this repo has its Postgres
  schema and `fuelmind-devcloud`, which is the same handler set in memory.
- The MSI has been built and validated but not yet installed on a real
  shop PC.
