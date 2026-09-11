// Package posadapter defines the plug-in contract every POS / pump-controller
// integration must implement. This is FuelMind's moat — invest here more than
// in the AI layer (per FuelMind_Architecture_Spec §2.2).
//
// A vendor adapter package implements Adapter, then registers itself with
// the package-level registry at init() time (see registry.go). The active
// adapter is selected via runtime config.
package posadapter

import (
	"context"
	"errors"
	"time"
)

// Config is adapter-specific. Each adapter documents its own keys. The
// shape is intentionally open: adapters can require DB DSNs, serial port
// paths, REST credentials, file-watch folders, or anything else.
//
// Example (csv_watch): {"folder": "C:\\ProgramData\\FuelMind\\pos_drop"}
type Config map[string]any

// Connection represents an open link to a POS / pump / controller. It must
// be safe to call Healthy() concurrently with the adapter's Pull* methods.
type Connection interface {
	Close() error
	Healthy() error
}

// Adapter is the interface every POS integration must implement. Concrete
// implementations live in sub-packages (e.g. csvwatch, vendor_x_db).
//
// Lifecycle:
//
//	adapter := posadapter.New("csv_watch")
//	conn, err := adapter.Connect(ctx, cfg)
//	defer conn.Close()
//	rows, err := adapter.PullTransactions(ctx, sinceTime, 1000)
type Adapter interface {
	// Name returns the stable adapter name used in config, logs, and the
	// cloud heartbeat. Examples: "csv_watch", "vendor_x_db", "vendor_y_serial".
	Name() string

	// Connect establishes a connection. cfg is adapter-specific.
	Connect(ctx context.Context, cfg Config) (Connection, error)

	// PullTransactions returns transactions with OccurredAt > since.
	// batchSize is a hint; adapters may return fewer per call.
	// MUST be idempotent: re-pulling the same range must be safe (the
	// normalizer dedupes on raw_transaction_id, but adapters that page
	// through vendor APIs should not lose or duplicate rows on retry).
	PullTransactions(ctx context.Context, since time.Time, batchSize int) ([]RawTransaction, error)

	// PullInventorySnapshot returns the most recent inventory state.
	// For watch-folder adapters this may be a no-op (return nil, nil).
	PullInventorySnapshot(ctx context.Context) ([]RawInventory, error)

	// PullShiftData returns shifts with OccurredAt > since.
	// For watch-folder adapters this may be a no-op (return nil, nil).
	PullShiftData(ctx context.Context, since time.Time) ([]RawShift, error)
}

// Puller is an optional interface an adapter may implement to expose a
// continuous-push entry point. The runtime calls Start once at boot if
// the active adapter supports it; otherwise the runtime falls back to
// timed calls to PullTransactions. csv_watch is the natural user of this.
type Puller interface {
	Start(ctx context.Context, out chan<- RawTransaction) error
}

// ErrNotImplemented is returned by Pull* methods of stub adapters that
// are still being built. The contract test in this package explicitly
// allows this error; production code should never see it.
var ErrNotImplemented = errors.New("posadapter: not implemented in this build")
