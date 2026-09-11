// Package csvwatch is the vendor-agnostic v1 POS adapter. It watches a
// directory for CSV files dropped by any POS system that can export to
// CSV (which is essentially all of them). One file = one or more
// transactions. After successful ingestion, the file is moved to a
// "processed/" sub-folder; on parse error, to "failed/".
//
// Expected CSV columns are documented in testdata/pos_samples/README.md.
// Required columns: pos_source_id, external_id, occurred_at, product_alias,
// quantity_liters, unit_price, total_amount, payment_method.
//
// This file is the shell: it implements the posadapter.Adapter
// interface (for compliance / registry / "fuelmind adapters list"
// tooling) and exposes Conn so the runtime can hand the folder to a
// real Watcher (watcher.go) that does the actual fsnotify-driven work.
package csvwatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/fuelmind/fuelmind/internal/posadapter"
)

// AdapterName is the stable identifier used in config and logs.
const AdapterName = "csv_watch"

// Compile-time check that *Adapter satisfies posadapter.Adapter. This
// will fail to build if the interface ever changes incompatibly.
var _ posadapter.Adapter = (*Adapter)(nil)

func init() {
	posadapter.Register(AdapterName, func() posadapter.Adapter { return New() })
}

// Adapter is a vendor-agnostic CSV watch-folder adapter.
type Adapter struct{}

// New returns a fresh CSV-watch adapter. Factories are pure so callers
// can construct multiple instances (e.g. for tests that watch multiple
// folders in parallel).
func New() *Adapter { return &Adapter{} }

// Name returns the adapter's stable identifier.
func (a *Adapter) Name() string { return AdapterName }

// Connect creates the expected sub-folders and returns a Conn. The
// actual file watching is in NewWatcher + Watcher.Start (watcher.go).
func (a *Adapter) Connect(ctx context.Context, cfg posadapter.Config) (posadapter.Connection, error) {
	folder, _ := cfg["folder"].(string)
	if folder == "" {
		return nil, errors.New(`csvwatch: config["folder"] is required`)
	}
	if err := os.MkdirAll(folder, 0o755); err != nil {
		return nil, fmt.Errorf("csvwatch: mkdir %q: %w", folder, err)
	}
	for _, sub := range []string{"processed", "failed"} {
		if err := os.MkdirAll(filepath.Join(folder, sub), 0o755); err != nil {
			return nil, fmt.Errorf("csvwatch: mkdir %q: %w", filepath.Join(folder, sub), err)
		}
	}
	return &Conn{folder: folder}, nil
}

// Conn is an open connection to a csv_watch folder. Returned by
// Adapter.Connect. The runtime uses Folder() to hand the same path to
// csvwatch.NewWatcher.
type Conn struct {
	folder string
}

// Close is a no-op; the Conn holds no resources itself. The Watcher
// created from this folder is closed separately.
func (c *Conn) Close() error { return nil }

// Healthy returns nil if the watch folder still exists and is a
// directory. Used by the runtime's health-check loop and by tests.
func (c *Conn) Healthy() error {
	fi, err := os.Stat(c.folder)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("csvwatch: %q is not a directory", c.folder)
	}
	return nil
}

// Folder returns the configured watch folder. Used by the runtime to
// hand the same path to NewWatcher.
func (c *Conn) Folder() string { return c.folder }

// PullTransactions is not the way csv_watch delivers data — it watches
// a folder and pushes. The Watcher (watcher.go) is the real
// implementation. This method returns ErrNotImplemented to satisfy the
// contract; production code should never reach it.
func (a *Adapter) PullTransactions(ctx context.Context, since time.Time, batchSize int) ([]posadapter.RawTransaction, error) {
	return nil, posadapter.ErrNotImplemented
}

// PullInventorySnapshot is a no-op for CSV watch (no inventory feed in
// the v1 file format). Inventory arrives via the manual entry UI in
// v1.1 or via a vendor-specific adapter in v1.1+.
func (a *Adapter) PullInventorySnapshot(ctx context.Context) ([]posadapter.RawInventory, error) {
	return nil, nil
}

// PullShiftData is a no-op for CSV watch (no shift feed in the v1
// file format). Same future path as inventory.
func (a *Adapter) PullShiftData(ctx context.Context, since time.Time) ([]posadapter.RawShift, error) {
	return nil, nil
}
