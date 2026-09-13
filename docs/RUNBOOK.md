# FuelMind Runbook

For whoever is on call (today: you). Everything here assumes you are on
the shop PC, or on a remote-desktop session into it, as administrator.

## Orientation: one command

```powershell
"C:\Program Files\FuelMind\fuelmind-setup.exe" status
```

Prints the station id, licence tier, hardware tier, data directory,
dashboard URL, POS folder counts (waiting / processed / failed), the last
POS file time, unknown product names, and the last heartbeat.

Service control:

```powershell
sc query FuelMindService
sc stop  FuelMindService
sc start FuelMindService
```

Logs: `C:\ProgramData\FuelMind\logs\core.log` and `launcher.log`
(each rotates at 10 MB into `.1`).

## "The dashboard shows zero sales but the POS has sales"

1. `fuelmind-setup status` — how many files are waiting or failed?
2. Files in `pos_drop\failed\`: open the newest one and check the header
   row. The core needs `pos_source_id, external_id, occurred_at,
   product_alias, quantity_liters, unit_price, total_amount,
   payment_method`. Case and a UTF-8 BOM do not matter; a missing column
   does. `core.log` names the line and the reason.
3. Files sitting in `pos_drop\` and never moving: the POS is probably
   holding the file open, or the service account cannot read it. Check
   `core.log` for `read:` errors.
4. Nothing in the folder at all: the POS export schedule is the problem,
   not FuelMind.
5. Sales exist but are older than a week: `fuelmind-setup status` shows
   them as ingested. Run
   `"C:\ProgramData\FuelMind\versions\<version>\FuelMindCore.exe" -rebuild-mart`
   to recompute every day from history.

## "One product is missing from the totals"

`fuelmind-setup status` lists unknown product names. The rows are safely
in the database; they only need an alias:

```powershell
sqlite3 C:\ProgramData\FuelMind\fuelmind.db ^
  "INSERT INTO product_aliases (alias_text, product_code) VALUES ('SUPER DIESEL','DIESEL');"
```

Within 15 minutes (or after the next export) the rows normalize by
themselves; no restart needed.

## "I can't log in"

- Five wrong PINs lock login for 15 minutes; the page says until when.
- Reset: `fuelmind-setup reset-pin`. It prompts twice, does not echo, and
  signs out every device.
- The first PIN of a fresh install can only be set from the shop PC
  itself (`http://localhost:8765/`), never from a phone.

## "The owner can't reach the dashboard from their phone"

1. Same network? The dashboard is LAN-only; there is no internet exposure.
2. `http://<shop-pc-name>:8765/` or the PC's IP.
3. The MSI adds a firewall rule for TCP 8765 scoped to the local subnet.
   If it was removed:
   `netsh advfirewall firewall add rule name="FuelMind dashboard" dir=in action=allow protocol=TCP localport=8765 profile=private`

## Updates and rollback

Normal flow: the core checks for updates every 30 minutes, downloads and
verifies the artifact, stages it, and asks the launcher to switch during
02:00–05:00. The launcher then checks `/healthz` for 90 seconds.

- A new version that does not answer `/healthz`, or crashes twice inside
  the two-hour probation, is rolled back automatically. The failure is
  reported in the next heartbeat.
- Force an update now: set `FUELMIND_UPDATE_WINDOW=any` for the service
  and restart it.
- Manual rollback:
  ```powershell
  sc stop FuelMindService
  # set current.json to the previous version
  notepad C:\ProgramData\FuelMind\current.json
  sc start FuelMindService
  ```
  Installed versions live in `C:\ProgramData\FuelMind\versions\`.

## Backups and recovery

- Nightly at 02:00 station time into `C:\ProgramData\FuelMind\backups\`,
  7 kept, written with SQLite `VACUUM INTO` so they are consistent even
  while the POS is writing.
- A copy is also taken automatically before any schema migration
  (`pre-migrate-*.db`).
- Restore: stop the service, replace `fuelmind.db` with the backup
  (delete any `fuelmind.db-wal` / `-shm` next to it), start the service.

## Reinstall or move to a new PC

1. Install the MSI on the new PC.
2. Stop the service, copy the old `fuelmind.db` into
   `C:\ProgramData\FuelMind\`, start the service. The station keeps its
   station id and API key because they live in that database.
3. Point the POS export at the new `pos_drop` folder.

## Privacy answers for owners

- Sales and customer data never leave the PC.
- With no cloud configured there are no outbound calls at all.
- With a cloud configured, each heartbeat sends the station id, the
  software version and a timestamp. Health data (hardware tier, database
  size, free disk, error counts, score, last POS file time) is only sent
  if the owner ticked the box at setup.

## Owner asks questions from their phone

"I turned it on but nothing answers."

1. `fuelmind-setup status` — is `remote questions: on`, and is a cloud URL configured?
2. Is the service running? `sc query FuelMindService`.
3. `logs\core.log` should show `remote questions enabled` at start-up and
   `remote question answered` per message.
4. On the control plane, check the webhook token in the provider's URL and
   that the station id in the query string matches the station.
5. The sender gets "your station is offline" when the station has not
   answered within ~25s: the station is down, has no internet, or the
   owner switched remote questions off.

Answers are logged on the station at the bottom of **Settings**, and in
`remote_questions` in the database.

## "The margin numbers look wrong"

Margin needs the purchase price the owner paid. Check **Settings -> what
you pay per litre**: a price applies from its date forward, so a price
entered today does not change last week unless a price is entered with
last week's date. Days with no price show a dash, never revenue-as-profit.

## "A customer paid but still shows as owing"

Payments are recorded per customer: **Credit -> the customer -> record a
payment**. The balance and the overdue counter update immediately (the
mart is recalculated for the last 90 days on save). If the customer's
phone number differs by formatting between the POS export and the payment,
they will be two different customers — fix the POS export.
