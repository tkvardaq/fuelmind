# Cloud control plane (Phase 5)

The local core (everything in `/cmd/fuelmind-core` and `/internal/*`)
ships with a thin cloud control plane that does one job: **let the
founder run a fleet of 5–10 stations without babysitting them**.

## What lives here

- `sql/001_organizations.up.sql` … `005_update_releases.up.sql` —
  the Postgres schema per spec §6.1. Deploy in order; each migration
  is idempotent (`IF NOT EXISTS` where Postgres needs it; otherwise
  the migration tracker handles it).
- `cmd/cloud/` — the deployable HTTP service (Phase 5 stretch goal;
  main wiring happens alongside Phase 5 ship). Exposes:
  - `POST /v1/heartbeat` (spec §6.3 payload in, license block out)
  - `GET  /v1/license/{station_id}` (license block out)
  - Plus a single-page admin tool for you (basic auth) so you can
    flip a station from `private` → `connected` without engineering.

## What's NOT here

- The cloud never sees transaction data. Heartbeats carry zero
  financial detail (spec §6.3).
- The cloud never sends commands back. License flips and update
  manifests are advisory; the install always keeps its last known
  good config and works offline.

## Local development

When the local core runs with `FUELMIND_CLOUD_URL` unset, the sync
agent is disabled entirely. To test the round-trip on a laptop:

```powershell
# 1. Start the in-memory cloud (test-only; this lives in the
#    internal/cloudctl package, not in /cloud).
go test -count=1 -run TestHeartbeat ./internal/sync/

# 2. Or run the agent against the test server in your own code:
$env:FUELMIND_CLOUD_URL = "http://127.0.0.1:8080"
go run ./cmd/fuelmind-core
```

The real cloud binary (Phase 5 stretch) lives in `/cloud/cmd/cloud/`.
That code will follow the same handler structure as the in-memory
test server (see `internal/cloudctl/cloudctl.go`) but talks to
Postgres instead of in-memory maps.