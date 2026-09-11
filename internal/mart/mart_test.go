package mart

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fuelmind/fuelmind/internal/storage"
)

func newTestMart(t *testing.T) (*Mart, *storage.Storage) {
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
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	return New(s, logger), s
}

// seedTransactions writes n normalized transactions for the given
// (date, product_code, payment_method) tuple directly, bypassing the
// normalizer. Each transaction gets a unique external_id so the
// UNIQUE constraint on raw_transaction_id doesn't collide. For
// mart tests we don't care about the raw layer.
func seedTransactions(t *testing.T, s *storage.Storage, date, product, payMethod string, n int, totalEach float64) {
	t.Helper()
	for i := 0; i < n; i++ {
		// Insert a raw row first (FK target).
		rawID, err := s.DB().ExecContext(context.Background(), `
			INSERT INTO raw_pos_transactions (pos_source_id, raw_payload, payload_hash, ingestion_batch_id)
			VALUES ('lane_1', '{}', ?, 'test-batch')
		`, uniqueHash(date, product, i, payMethod))
		if err != nil {
			t.Fatal(err)
		}
		rawIDInt, _ := rawID.LastInsertId()
		// Then the normalized row.
		_, err = s.DB().ExecContext(context.Background(), `
			INSERT INTO transactions
				(raw_transaction_id, product_code, quantity_liters, unit_price, total_amount, payment_method, transaction_time)
			VALUES (?, ?, 1, ?, ?, ?, ?)
		`, rawIDInt, product, totalEach, totalEach, payMethod, date+"T12:00:00Z")
		if err != nil {
			t.Fatal(err)
		}
	}
}

var hashCounter int

func uniqueHash(date, product string, i int, pay string) string {
	hashCounter++
	return "h-" + date + "-" + product + "-" + pay + "-" + itoa(i) + "-" + itoa(hashCounter)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for n > 0 {
		pos--
		b[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(b[pos:])
}

func TestRefreshDailySales(t *testing.T) {
	m, s := newTestMart(t)

	// 3 transactions for DIESEL on 2026-09-07, total 100 each.
	seedTransactions(t, s, "2026-09-07", "DIESEL", "CASH", 3, 100)
	// 2 transactions for PETROL_92 on the same day, total 50 each.
	seedTransactions(t, s, "2026-09-07", "PETROL_92", "CASH", 2, 50)

	if err := m.MaterializeSince(context.Background(), parseDate(t, "2026-09-07")); err != nil {
		t.Fatal(err)
	}

	// Assert daily_sales for 2026-09-07.
	var (
		dieselLiters, dieselRev float64
		dieselCount             int
	)
	err := s.DB().QueryRowContext(context.Background(),
		`SELECT volume_liters, revenue, transaction_count FROM daily_sales
		 WHERE date = '2026-09-07' AND product_code = 'DIESEL'`,
	).Scan(&dieselLiters, &dieselRev, &dieselCount)
	if err != nil {
		t.Fatalf("diesel: %v", err)
	}
	if dieselCount != 3 {
		t.Errorf("diesel count = %d, want 3", dieselCount)
	}
	if dieselRev != 300 {
		t.Errorf("diesel revenue = %v, want 300", dieselRev)
	}

	var p92Count int
	var p92Rev float64
	err = s.DB().QueryRowContext(context.Background(),
		`SELECT revenue, transaction_count FROM daily_sales
		 WHERE date = '2026-09-07' AND product_code = 'PETROL_92'`,
	).Scan(&p92Rev, &p92Count)
	if err != nil {
		t.Fatalf("petrol_92: %v", err)
	}
	if p92Count != 2 || p92Rev != 100 {
		t.Errorf("petrol_92: count=%d rev=%v, want count=2 rev=100", p92Count, p92Rev)
	}
}

func TestMaterializeIsIdempotent(t *testing.T) {
	m, s := newTestMart(t)
	seedTransactions(t, s, "2026-09-07", "DIESEL", "CASH", 5, 100)

	// Run twice.
	if err := m.MaterializeSince(context.Background(), parseDate(t, "2026-09-07")); err != nil {
		t.Fatal(err)
	}
	if err := m.MaterializeSince(context.Background(), parseDate(t, "2026-09-07")); err != nil {
		t.Fatal(err)
	}

	// Should still be 5 transactions, not 10.
	var n int
	_ = s.DB().QueryRowContext(context.Background(),
		`SELECT transaction_count FROM daily_sales WHERE date = '2026-09-07' AND product_code = 'DIESEL'`,
	).Scan(&n)
	if n != 5 {
		t.Errorf("transaction_count = %d, want 5 (idempotent)", n)
	}
}

func TestRefreshCreditOutstanding(t *testing.T) {
	m, s := newTestMart(t)
	// One credit customer with 3 transactions of 200 each = 600 outstanding.
	seedTransactions(t, s, "2026-09-07", "DIESEL", "CASH", 1, 100)
	seedCreditTransactions(t, s, "2026-09-07", "+923001234567", 3, 200)

	if err := m.MaterializeSince(context.Background(), parseDate(t, "2026-09-07")); err != nil {
		t.Fatal(err)
	}

	var amt float64
	var cnt int
	err := s.DB().QueryRowContext(context.Background(),
		`SELECT outstanding_amount, transaction_count FROM credit_outstanding
		 WHERE customer_phone = '+923001234567' ORDER BY as_of_date DESC LIMIT 1`,
	).Scan(&amt, &cnt)
	if err != nil {
		t.Fatalf("credit_outstanding: %v", err)
	}
	if amt != 600 || cnt != 3 {
		t.Errorf("credit: amount=%v count=%d, want 600/3", amt, cnt)
	}
}

func seedCreditTransactions(t *testing.T, s *storage.Storage, date, phone string, n int, totalEach float64) {
	t.Helper()
	for i := 0; i < n; i++ {
		rawID, err := s.DB().ExecContext(context.Background(), `
			INSERT INTO raw_pos_transactions (pos_source_id, raw_payload, payload_hash, ingestion_batch_id)
			VALUES ('lane_1', '{}', ?, 'test-batch')
		`, uniqueHash(date, "credit", i, phone))
		if err != nil {
			t.Fatal(err)
		}
		rawIDInt, _ := rawID.LastInsertId()
		_, err = s.DB().ExecContext(context.Background(), `
			INSERT INTO transactions
				(raw_transaction_id, product_code, quantity_liters, unit_price, total_amount,
				 payment_method, customer_phone, transaction_time)
			VALUES (?, 'DIESEL', 1, ?, ?, 'CREDIT', ?, ?)
		`, rawIDInt, totalEach, totalEach, phone, date+"T12:00:00Z")
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestRefreshScores(t *testing.T) {
	m, s := newTestMart(t)
	// A day with good data: 20 transactions, no credit issues.
	seedTransactions(t, s, "2026-09-07", "DIESEL", "CASH", 20, 1000)

	if err := m.MaterializeSince(context.Background(), parseDate(t, "2026-09-07")); err != nil {
		t.Fatal(err)
	}

	var overall, sales, credit int
	err := s.DB().QueryRowContext(context.Background(),
		`SELECT overall_score, sales_score, credit_score FROM station_health_score
		 WHERE date = '2026-09-07'`,
	).Scan(&overall, &sales, &credit)
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if sales != 100 {
		t.Errorf("sales_score = %d, want 100", sales)
	}
	if credit != 100 {
		t.Errorf("credit_score = %d, want 100", credit)
	}
	if overall < 90 {
		t.Errorf("overall_score = %d, want >= 90 for a good day", overall)
	}
}

func TestRefreshScoresWithIssues(t *testing.T) {
	m, s := newTestMart(t)
	// A day with no transactions: sales score = 50.
	if err := m.MaterializeSince(context.Background(), parseDate(t, "2026-09-07")); err != nil {
		t.Fatal(err)
	}
	// The mart won't create a score row for a day with no
	// transactions (it only iterates dates-with-activity). So we
	// test the computeScore function directly instead.

	sdb := s.DB()
	row := sdb.QueryRowContext(context.Background(), `
		SELECT date FROM station_health_score WHERE date = '2026-09-07'
	`)
	var d string
	if err := row.Scan(&d); err == nil {
		t.Errorf("expected no score row for empty day, got %q", d)
	}
}

func TestComputeScoreOnEmptyDay(t *testing.T) {
	_, s := newTestMart(t)
	sdb := s.DB()
	// Directly call computeScore on a day with no transactions.
	sc, err := computeScore(context.Background(), sdb, "2026-09-07")
	if err != nil {
		t.Fatal(err)
	}
	if sc.sales != 50 {
		t.Errorf("empty-day sales_score = %d, want 50", sc.sales)
	}
	if len(sc.issues) == 0 {
		t.Error("expected at least one issue for empty day")
	}
	found := false
	for _, is := range sc.issues {
		if is.Code == "no_transactions" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected no_transactions issue, got %+v", sc.issues)
	}
}

func TestComputeScoreWithHighCredit(t *testing.T) {
	m, s := newTestMart(t)
	// One credit customer with 100,000 outstanding.
	seedCreditTransactions(t, s, "2026-09-07", "+923001234567", 1, 100000)
	if err := m.MaterializeSince(context.Background(), parseDate(t, "2026-09-07")); err != nil {
		t.Fatal(err)
	}
	var credit int
	_ = s.DB().QueryRowContext(context.Background(),
		`SELECT credit_score FROM station_health_score WHERE date = '2026-09-07'`,
	).Scan(&credit)
	if credit != 60 {
		t.Errorf("credit_score with 100k outstanding = %d, want 60", credit)
	}
}

func parseDate(t *testing.T, s string) time.Time {
	t.Helper()
	tt, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatal(err)
	}
	return tt
}

func TestMaterializeEmpty(t *testing.T) {
	m, _ := newTestMart(t)
	// No data at all. Should not error, should not crash.
	if err := m.MaterializeSince(context.Background(), parseDate(t, "2026-09-07")); err != nil {
		t.Errorf("empty mart: %v", err)
	}
	if err := m.MaterializeAll(context.Background()); err != nil {
		t.Errorf("empty MaterializeAll: %v", err)
	}
}

func TestMaterializeAllWithPastData(t *testing.T) {
	m, s := newTestMart(t)
	// Seed transaction 45 days in the past
	seedTransactions(t, s, "2026-07-20", "DIESEL", "CASH", 5, 200)

	if err := m.MaterializeAll(context.Background()); err != nil {
		t.Fatal(err)
	}

	var revenue float64
	err := s.DB().QueryRowContext(context.Background(), `
		SELECT revenue FROM daily_sales WHERE date = '2026-07-20' AND product_code = 'DIESEL'
	`).Scan(&revenue)
	if err != nil {
		t.Fatalf("query past daily_sales: %v", err)
	}
	if revenue != 1000 {
		t.Errorf("expected past revenue 1000, got %v", revenue)
	}
}
