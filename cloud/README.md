# Cloud control plane

The local core is authoritative for everything a station owner sees. The
cloud does three jobs and nothing else:

1. tell a station what licence it has,
2. tell it when an update is available,
3. collect a thin heartbeat so the fleet can be watched from one screen.

Nothing here is in the critical path: with `FUELMIND_CLOUD_URL` unset the
core makes no network calls at all, and when the cloud is unreachable the
station keeps working and retries on the next cycle.

## API

| Endpoint | Auth | Purpose |
|---|---|---|
| `POST /v1/heartbeat` | station API key (bearer) | spec §6.3 payload in, licence block out |
| `GET /v1/license/{station_id}` | station API key | current tier + `features_json` |
| `GET /v1/updates/check` | station API key | manifest for a newer release, or 204 |
| `PUT /v1/admin/stations/{id}/license` | admin token | flip tier, feature flags or status |

Query parameters for the update check: `station_id`, `version` (running),
`tier` (licence tier), `hardware_tier`. The server only offers a release
that is newer, that the station's hardware supports, and whose staged
rollout bucket includes the station.

## What the heartbeat carries

Always: `station_id`, `software_version`, `timestamp`, and a
`last_rollback` block if the previous update failed.

Only when the owner ticked the telemetry box during setup: hardware tier,
database size, free disk, error counts, active alerts, FuelMind Score and
the time of the last POS file. Never: sales, prices, customers or any
financial figure.

## Schema

`sql/001…005` is the Postgres schema (organizations, stations, licenses,
heartbeats, update_releases + update_history). Deploy in order.
`api_key_hash` stores SHA-256 of the station key; the raw key only ever
exists on the station.

## Running it locally

`cmd/fuelmind-devcloud` serves exactly this API from memory, which is
what the tests use too:

```powershell
go run ./cmd/fuelmind-devcloud `
  -db C:\ProgramData\FuelMind\fuelmind.db `
  -release 1.1.0=dist\fuelmind-core-1.1.0.zip `
  -admin-token dev
```

`-db` registers the station whose identity is in that database, and
artifacts are served from `/artifacts/`, so a release URL can be a plain
relative path. This is the rig to rehearse an update on before shipping
one to a real station.

## Production service

Not built yet. When it is, it should be the same handlers over Postgres
(see `internal/cloudctl` for the reference implementation), behind TLS,
with per-station rate limiting on the heartbeat endpoint and the admin
token in a secret store rather than a flag.
