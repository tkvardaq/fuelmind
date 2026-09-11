# Authoring a POS Adapter for FuelMind

This guide is for anyone (you, in v1.1; a vendor partner, in v2) who needs
to teach FuelMind how to talk to a new POS / pump / controller. The
`posadapter` package defines the contract; this guide explains how to
honor it correctly.

## Why this matters

The POS integration is FuelMind's moat. The LLM intent router, the
dashboard, the cloud control plane — none of it works without clean,
vendor-agnostic, raw transaction data flowing into the local DB. Every
adapter you write is a station you can serve that no competitor can.

## The contract (interface)

Every adapter implements `posadapter.Adapter`:

```go
type Adapter interface {
    Name() string
    Connect(ctx context.Context, cfg Config) (Connection, error)
    PullTransactions(ctx context.Context, since time.Time, batchSize int) ([]RawTransaction, error)
    PullInventorySnapshot(ctx context.Context) ([]RawInventory, error)
    PullShiftData(ctx context.Context, since time.Time) ([]RawShift, error)
}
```

Optionally implement `posadapter.Puller` for adapters that prefer a
push-style API (file watchers, message-queue consumers):

```go
type Puller interface {
    Start(ctx context.Context, out chan<- RawTransaction) error
}
```

## Step-by-step

### 1. Create a sub-package

```bash
mkdir -p internal/posadapter/myvendor
```

Package name: lowercase, short, and stable — it will appear in config
files, logs, and the cloud heartbeat. We use `snake_case` for names that
go in user-facing config and `camelCase` only inside Go identifiers.

### 2. Define the adapter

```go
package myvendor

import (
    "context"
    "time"
    "github.com/fuelmind/fuelmind/internal/posadapter"
)

const AdapterName = "myvendor_db"  // appears in config.toml

func init() {
    posadapter.Register(AdapterName, func() posadapter.Adapter { return New() })
}

type Adapter struct{}

func New() *Adapter { return &Adapter{} }
func (a *Adapter) Name() string { return AdapterName }

// Connect reads cfg["dsn"], opens the DB, returns a Connection.
func (a *Adapter) Connect(ctx context.Context, cfg posadapter.Config) (posadapter.Connection, error) {
    dsn, _ := cfg["dsn"].(string)
    if dsn == "" { return nil, errors.New("myvendor: config[\"dsn\"] required") }
    db, err := sql.Open("postgres", dsn)  // or your driver's package
    if err != nil { return nil, fmt.Errorf("myvendor: open: %w", err) }
    return &conn{db: db}, nil
}

type conn struct{ db *sql.DB }
func (c *conn) Close() error { return c.db.Close() }
func (c *conn) Healthy() error { return c.db.Ping() }

// PullTransactions queries the vendor's transactions table for rows
// newer than `since`. Returns RawTransaction; Payload is the original
// row bytes (JSON of the vendor's row, or a vendor-specific envelope).
// See "Payload hygiene" below.
func (a *Adapter) PullTransactions(ctx context.Context, since time.Time, batchSize int) ([]posadapter.RawTransaction, error) {
    rows, err := a.db.QueryContext(ctx, `
        SELECT lane_id, txn_id, txn_time, product_name, qty_l, unit_price, total, pay_method
        FROM vendor.transactions
        WHERE txn_time > $1
        ORDER BY txn_time ASC
        LIMIT $2
    `, since, batchSize)
    if err != nil { return nil, err }
    defer rows.Close()

    var out []posadapter.RawTransaction
    for rows.Next() {
        var (
            lane, txnID, product, payMethod string
            txnTime                          time.Time
            qty, unitPrice, total            float64
        )
        if err := rows.Scan(&lane, &txnID, &txnTime, &product, &qty, &unitPrice, &total, &payMethod); err != nil {
            return nil, err
        }
        // Vendor-specific row → JSON for the Payload. The normalizer (Phase 2)
        // knows how to parse it because it's in the documented vendor format.
        payload, _ := json.Marshal(struct{...}{lane, txnID, txnTime, product, qty, unitPrice, total, payMethod})
        out = append(out, posadapter.RawTransaction{
            PosSourceID: lane,
            ExternalID:  txnID,
            OccurredAt:  txnTime,
            Payload:     payload,
            PayloadKind: posadapter.PayloadJSON,
        })
    }
    return out, rows.Err()
}
```

### 3. Write the contract test

Copy `internal/posadapter/csvwatch/adapter_test.go` and adapt. The
contract test is the same for every adapter:

- It is registered under `AdapterName`
- `Name()` returns `AdapterName`
- It implements `posadapter.Adapter`
- `Connect(cfg)` with valid config creates whatever sub-resources it needs
- `conn.Healthy()` returns nil after Connect
- `Connect(empty config)` returns a clear error

Plus **your** own test that runs against a fixture (real or sanitized
from production) and asserts the parsed `RawTransaction` rows match
expected.

### 4. Document the config

Add a section to `docs/ADAPTER_AUTHOR_GUIDE.md` (or your own
`internal/posadapter/myvendor/README.md`) listing every config key:

| Key | Required | Example | Notes |
|---|---|---|---|
| `dsn` | yes | `host=10.0.0.5 user=readonly password=…` | Read-only DB user, scoped to the vendor's transactions schema |
| `poll_interval_seconds` | no (default 30) | `15` | How often PullTransactions is called |
| `lane_filter` | no | `["lane_1", "lane_2"]` | If set, only these lanes are pulled |

### 5. Wire it into the binary

The `csvwatch` adapter is imported by `cmd/fuelmind-core/main.go` via a
blank import (added in Phase 1). For your adapter, add the same:

```go
import _ "github.com/fuelmind/fuelmind/internal/posadapter/myvendor"
```

This triggers the `init()` registration. Without the blank import the
adapter is invisible to the binary.

## Payload hygiene (the most common bug source)

The normalizer (Phase 2) reads `RawTransaction.Payload` and turns it
into a normalized `transactions` row. For the normalizer to work, the
Payload **must be** the same shape every time for a given vendor. Pick
one:

- **Option A: vendor's native row, JSON-encoded.** Easiest, but the
  normalizer needs a vendor-specific parser. Maintain one parser per
  vendor.
- **Option B: a stable intermediate JSON shape per vendor.** Define one
  JSON schema per vendor (see `testdata/pos_samples/README.md` for the
  shape the normalizer already understands) and have your adapter emit
  that shape. The normalizer can then reuse one parser.
- **Option C: the universal CSV shape** (same as `csv_watch`). If your
  vendor can export CSV in that exact column order, the adapter just
  writes files to a folder and the existing `csv_watch` adapter picks
  them up. No new adapter needed.

For v1, **prefer Option C** (CSV drop) for any vendor that can do it.
Only write a custom adapter if the vendor has no CSV export.

## Idempotency

The normalizer dedupes on `RawTransaction.ExternalID` per
`PosSourceID`. Your adapter must produce a stable, unique
`ExternalID` for every transaction — typically the POS's own transaction
ID. Never invent IDs from timestamps alone (collisions across pumps).

## What never belongs in an adapter

- **No business logic.** The adapter emits raw rows; the normalizer
  resolves aliases, the data mart aggregates, the LLM explains. An
  adapter that "computes the daily total" is a bug.
- **No network calls outside the vendor's own protocol.** Don't have
  your adapter hit FuelMind cloud, Slack, email, etc.
- **No file writes outside its own state directory.** The runtime
  owns DB writes; the adapter owns its own small state (cursor
  position, last-poll timestamp).

## Testing tips

- **Fixtures over live systems.** Always test against sanitized
  exports. Real vendor databases change schema, get upgraded, and go
  down at 3am. Your tests must pass on a Tuesday afternoon in 2027.
- **Contract test first, then vendor-specific test.** The contract test
  catches interface breaks. Your test catches your bugs. Both must
  pass.
- **Test with empty input, with one row, and with thousands of rows.**
  Edge cases in CSV parsers (last row without newline, BOM, etc.) hide
  in the gap between these three.
