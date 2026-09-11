package storage_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/fuelmind/fuelmind/internal/storage"
)

func TestOpenAndMigrate(t *testing.T) {
	dir := t.TempDir()
	s, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Re-running is a no-op (idempotency is the whole point).
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate (second call): %v", err)
	}

	// Verify all expected tables exist.
	var count int
	err = s.DB().QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM sqlite_master
		WHERE type='table' AND name IN (
			'raw_pos_transactions',
			'raw_inventory_snapshots',
			'raw_shift_data',
			'schema_migrations'
		)
	`).Scan(&count)
	if err != nil {
		t.Fatalf("count tables: %v", err)
	}
	if count != 4 {
		t.Errorf("expected 4 tables, got %d", count)
	}
}

func TestWriteRawTransactions(t *testing.T) {
	dir := t.TempDir()
	s, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	ctx := context.Background()
	n, err := s.WriteRawTransactions(ctx, "batch-001", "lane_1",
		[][]byte{[]byte(`{"a":1}`), []byte(`{"a":2}`)},
		[]string{"hash1", "hash2"},
	)
	if err != nil {
		t.Fatalf("WriteRawTransactions: %v", err)
	}
	if n != 2 {
		t.Errorf("first insert: expected 2 inserted, got %d", n)
	}

	// Re-insert same hashes: dedup, 0 new rows.
	n, err = s.WriteRawTransactions(ctx, "batch-002", "lane_1",
		[][]byte{[]byte(`{"a":1}`), []byte(`{"a":2}`)},
		[]string{"hash1", "hash2"},
	)
	if err != nil {
		t.Fatalf("WriteRawTransactions (dedup): %v", err)
	}
	if n != 0 {
		t.Errorf("dedup insert: expected 0 inserted, got %d", n)
	}

	// Insert a mix: 1 new + 1 dup = 1 new.
	n, err = s.WriteRawTransactions(ctx, "batch-003", "lane_1",
		[][]byte{[]byte(`{"a":3}`), []byte(`{"a":1}`)},
		[]string{"hash3", "hash1"},
	)
	if err != nil {
		t.Fatalf("WriteRawTransactions (mixed): %v", err)
	}
	if n != 1 {
		t.Errorf("mixed insert: expected 1 inserted, got %d", n)
	}
}

func TestLocalConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Miss returns ErrNotFound.
	if _, err := s.GetLocalConfig(ctx, "station_id"); err != storage.ErrNotFound {
		t.Errorf("miss: got %v, want ErrNotFound", err)
	}

	// Set + Get.
	if err := s.SetLocalConfig(ctx, "station_id", "FM-12345", "", false); err != nil {
		t.Fatal(err)
	}
	row, err := s.GetLocalConfig(ctx, "station_id")
	if err != nil {
		t.Fatal(err)
	}
	if row.Value != "FM-12345" {
		t.Errorf("Value = %q", row.Value)
	}
	if row.Synchronized {
		t.Error("Synchronized should be false")
	}

	// Upsert: set with config_json + synchronized=true.
	if err := s.SetLocalConfig(ctx, "station_id", "FM-12345", `{"foo":"bar"}`, true); err != nil {
		t.Fatal(err)
	}
	row, _ = s.GetLocalConfig(ctx, "station_id")
	if !row.Synchronized {
		t.Error("Synchronized should be true after upsert")
	}
	if row.ConfigJSON == "" {
		t.Error("ConfigJSON should be set")
	}

	// Delete.
	if err := s.DeleteLocalConfig(ctx, "station_id"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetLocalConfig(ctx, "station_id"); err != storage.ErrNotFound {
		t.Errorf("after delete: got %v, want ErrNotFound", err)
	}
}

func TestSyncLogRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Empty → ErrNotFound.
	if _, err := s.LatestSyncAttempt(ctx); err != storage.ErrNotFound {
		t.Errorf("empty: got %v, want ErrNotFound", err)
	}

	// Successful attempt.
	if err := s.RecordSyncAttempt(ctx, "ok", "", 0, 0); err != nil {
		t.Fatal(err)
	}
	a, err := s.LatestSyncAttempt(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != "ok" {
		t.Errorf("Status = %q", a.Status)
	}

	// Failed attempt (more recent).
	if err := s.RecordSyncAttempt(ctx, "failed", "timeout", 0, 4500); err != nil {
		t.Fatal(err)
	}
	a, _ = s.LatestSyncAttempt(ctx)
	if a.Status != "failed" || a.Reason != "timeout" || a.RoundTripMs != 4500 {
		t.Errorf("got %+v", a)
	}
}

func TestMigration005Applied(t *testing.T) {
	dir := t.TempDir()
	s, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('local_config','sync_log')`,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("expected local_config + sync_log, got %d tables", count)
	}
}

func TestRecentCreditAsOfDateFilter(t *testing.T) {
	dir := t.TempDir()
	s, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Seed credit_outstanding with two snapshots for the same customer
	_, err = s.DB().ExecContext(ctx, `
		INSERT INTO credit_outstanding (customer_phone, as_of_date, outstanding_amount, transaction_count, days_overdue)
		VALUES 
			('03001234567', '2026-09-01', 5000.0, 2, 5),
			('03001234567', '2026-09-02', 7500.0, 3, 0),
			('03009876543', '2026-09-02', 3000.0, 1, 0)
	`)
	if err != nil {
		t.Fatal(err)
	}

	rows, err := s.RecentCredit(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 distinct customers from latest as_of_date, got %d", len(rows))
	}
	if rows[0].CustomerPhone != "03001234567" || rows[0].OutstandingAmount != 7500.0 {
		t.Errorf("expected customer 03001234567 to have latest amount 7500.0, got %v", rows[0])
	}
}
