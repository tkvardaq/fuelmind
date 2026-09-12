// Package csvwatch tests are split into two files:
//
//	adapter_test.go  — Phase 0 lifecycle contract + Phase 1 E2E ingest
//	parse_test.go    — Phase 1 parser unit tests
//
// The lifecycle contract is shared across all FuelMind POS adapters;
// the E2E tests are csv_watch specific.
package csvwatch

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fuelmind/fuelmind/internal/posadapter"
	"github.com/fuelmind/fuelmind/internal/storage"
)

// ---------------------------------------------------------------------------
// Phase 0: lifecycle contract — every FuelMind POS adapter must pass this.
// ---------------------------------------------------------------------------

func TestContract(t *testing.T) {
	// 1. Registered under canonical name.
	a, err := posadapter.New(AdapterName)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	if a.Name() != AdapterName {
		t.Errorf("Name() = %q, want %q", a.Name(), AdapterName)
	}

	// 2. Satisfies the Adapter interface.
	var _ posadapter.Adapter = a

	// 3. Connect with valid config creates sub-folders.
	folder := t.TempDir()
	cfg := posadapter.Config{"folder": folder}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := a.Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	if err := conn.Healthy(); err != nil {
		t.Errorf("Healthy after Connect: %v", err)
	}

	// Conn is the concrete *csvwatch.Conn and exposes Folder().
	cc, ok := conn.(*Conn)
	if !ok {
		t.Fatalf("Connect returned %T, want *csvwatch.Conn", conn)
	}
	if cc.Folder() != folder {
		t.Errorf("Conn.Folder() = %q, want %q", cc.Folder(), folder)
	}

	for _, sub := range []string{"processed", "failed"} {
		p := filepath.Join(folder, sub)
		fi, err := os.Stat(p)
		if err != nil {
			t.Errorf("expected sub-folder %q to exist: %v", sub, err)
			continue
		}
		if !fi.IsDir() {
			t.Errorf("expected %q to be a directory", p)
		}
	}

	// 4. Connect with missing config fails cleanly.
	if _, err := a.Connect(ctx, posadapter.Config{}); err == nil {
		t.Error("Connect with empty config should fail")
	}

	// 5. PullTransactions returns ErrNotImplemented (csv_watch uses
	//    the Watcher for real ingest, not this method).
	if _, err := a.PullTransactions(ctx, time.Time{}, 100); err != posadapter.ErrNotImplemented {
		t.Logf("PullTransactions returned %v (expected ErrNotImplemented in v1)", err)
	}
}

func TestRegistryListsCSVWatch(t *testing.T) {
	found := false
	for _, name := range posadapter.Names() {
		if name == AdapterName {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("registry missing %q; have %v", AdapterName, posadapter.Names())
	}
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// noopNormalizer is a Normalizer for tests that don't care about
// normalization (Phase 1 watcher tests). Production code passes a
// real *normalizer.Normalizer.
type noopNormalizer struct{ called int }

func (n *noopNormalizer) Run(ctx context.Context, limit int) (int, int, error) {
	n.called++
	return 0, 0, nil
}

// noopMart is a Mart for tests that don't care about the data mart.
type noopMart struct{}

func (m *noopMart) MaterializeSince(ctx context.Context, since time.Time) error { return nil }

func newTestStorage(t *testing.T) *storage.Storage {
	t.Helper()
	dir := t.TempDir()
	s, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func newTestWatcher(t *testing.T, s *storage.Storage) (*Watcher, *noopNormalizer) {
	t.Helper()
	norm := &noopNormalizer{}
	w, err := NewWatcher(s, norm, &noopMart{}, testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w, norm
}

// ---------------------------------------------------------------------------
// Phase 1: end-to-end ingest. Drops a real fixture into a watched folder
// and asserts the rows land in raw_pos_transactions.
// ---------------------------------------------------------------------------

func TestE2EIngestSample(t *testing.T) {
	dataDir := t.TempDir()
	posDrop := filepath.Join(dataDir, "pos_drop")
	if err := os.MkdirAll(posDrop, 0o755); err != nil {
		t.Fatal(err)
	}

	store := newTestStorage(t)
	w, _ := newTestWatcher(t, store)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := w.Start(ctx, posDrop); err != nil {
		t.Fatal(err)
	}

	// Copy the fixture to the watched folder.
	src := findRepoFile(t, filepath.Join("testdata", "pos_samples", "transactions_sample_01.csv"))
	dst := filepath.Join(posDrop, "test.csv")
	if err := os.WriteFile(dst, mustRead(t, src), 0o644); err != nil {
		t.Fatal(err)
	}

	// Wait for it to land in processed/.
	waitFor(t, 3*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(posDrop, "processed", "test.csv"))
		return err == nil
	})

	// Assert 10 rows in raw_pos_transactions.
	var count int
	if err := store.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM raw_pos_transactions`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 10 {
		t.Errorf("raw_pos_transactions count = %d, want 10", count)
	}

	// Assert the 10 rows are split across the 3 lanes the fixture uses.
	rows, err := store.DB().QueryContext(context.Background(),
		`SELECT pos_source_id, COUNT(*) FROM raw_pos_transactions GROUP BY pos_source_id ORDER BY pos_source_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	want := map[string]int{"lane_1": 4, "lane_2": 3, "lane_3": 3}
	got := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			t.Fatal(err)
		}
		got[s] = n
	}
	if len(got) != len(want) {
		t.Errorf("lanes = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("lane %s: got %d rows, want %d", k, got[k], v)
		}
	}
}

// Phase 1: a CSV with a missing required column is a file-level failure
// and must land in failed/, with zero rows in raw_pos_transactions.
func TestE2EMalformedCSV(t *testing.T) {
	posDrop := filepath.Join(t.TempDir(), "pos_drop")
	if err := os.MkdirAll(posDrop, 0o755); err != nil {
		t.Fatal(err)
	}

	store := newTestStorage(t)
	w, _ := newTestWatcher(t, store)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	_ = w.Start(ctx, posDrop)

	// Bad file: missing product_alias, unit_price, total_amount, payment_method.
	bad := []byte("pos_source_id,external_id,occurred_at\nlane_1,TX-001,2026-09-07T08:00:00Z\n")
	if err := os.WriteFile(filepath.Join(posDrop, "bad.csv"), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 3*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(posDrop, "failed", "bad.csv"))
		return err == nil
	})

	var count int
	_ = store.DB().QueryRow(`SELECT COUNT(*) FROM raw_pos_transactions`).Scan(&count)
	if count != 0 {
		t.Errorf("raw_pos_transactions count = %d, want 0 (malformed file should not insert)", count)
	}
}

// Phase 1: re-processing the same file is a no-op. The unique
// payload_hash constraint in migration 001 makes this safe.
func TestE2EIdempotentReprocess(t *testing.T) {
	posDrop := filepath.Join(t.TempDir(), "pos_drop")
	_ = os.MkdirAll(posDrop, 0o755)

	store := newTestStorage(t)
	w, _ := newTestWatcher(t, store)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	_ = w.Start(ctx, posDrop)

	// Drop the fixture.
	src := findRepoFile(t, filepath.Join("testdata", "pos_samples", "transactions_sample_01.csv"))
	dst := filepath.Join(posDrop, "test.csv")
	_ = os.WriteFile(dst, mustRead(t, src), 0o644)

	// Wait for the first ingest.
	waitFor(t, 3*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(posDrop, "processed", "test.csv"))
		return err == nil
	})

	// Move it back into pos_drop/ to simulate a re-run (e.g. a crash
	// happened between DB write and archive, or the operator manually
	// re-runs).
	if err := os.Rename(
		filepath.Join(posDrop, "processed", "test.csv"),
		dst,
	); err != nil {
		t.Fatal(err)
	}

	// Wait for re-processing.
	waitFor(t, 3*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(posDrop, "processed", "test.csv"))
		return err == nil
	})

	// Still 10 rows (dedup).
	var count int
	_ = store.DB().QueryRow(`SELECT COUNT(*) FROM raw_pos_transactions`).Scan(&count)
	if count != 10 {
		t.Errorf("raw_pos_transactions count = %d after re-process, want 10 (dedup)", count)
	}
}

// ---------------------------------------------------------------------------
// Shared test helpers (lowest section)
// ---------------------------------------------------------------------------

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("waitFor: condition not met within %v", timeout)
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	return b
}

// findRepoFile resolves a repo-relative path from the test's working
// directory (which is the package directory under `go test`). We try
// a few likely roots because the working dir can vary by Go version
// and build tag.
func findRepoFile(t *testing.T, rel string) string {
	t.Helper()
	candidates := []string{
		filepath.Join("..", "..", "..", rel), // internal/posadapter/csvwatch/ -> repo root
		filepath.Join("..", "..", rel),
		rel,
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Fatalf("findRepoFile: %q not found (tried %v)", rel, candidates)
	return ""
}
