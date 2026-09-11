// Package mart is the business data mart (spec §3.3, Layer 3 of the
// three-layer data model). It pre-aggregates the normalized
// `transactions` table into the query-ready tables the dashboard and
// intent router read from: daily_sales, fuel_margin, credit_outstanding,
// station_health_score.
//
// Per spec, the materialization job runs after every ingestion batch
// AND on a 15-minute timer. This file implements the first half; the
// watcher calls MaterializeAfterIngest() after every successful ingest,
// and main.go starts a ticker for the periodic refresh.
//
// The FuelMind Score (station_health_score) is a v1 simple
// weighted-sum algorithm. ML-based anomaly detection is v2.
package mart

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/fuelmind/fuelmind/internal/storage"
)

// Mart is the materialization job.
type Mart struct {
	store  *storage.Storage
	logger *slog.Logger
}

// New builds a Mart. Cheap; no I/O.
func New(store *storage.Storage, logger *slog.Logger) *Mart {
	if logger == nil {
		logger = slog.Default()
	}
	return &Mart{store: store, logger: logger}
}

// MaterializeSince refreshes every mart row whose date is in [since,
// today] inclusive. Called by both the ingest hook (with since =
// batch date) and the periodic ticker (with since = now - 24h).
//
// Idempotent: UPSERT semantics, re-running over the same data
// produces the same values.
func (m *Mart) MaterializeSince(ctx context.Context, since time.Time) error {
	// 1. Refresh daily_sales. We compute the set of (date,
	//    product_code) keys present in [since, today] and rebuild
	//    those rows. Anything older stays as-is.
	ds, err := m.refreshDailySales(ctx, since)
	if err != nil {
		return fmt.Errorf("mart: daily_sales: %w", err)
	}
	m.logger.Info("mart: daily_sales refreshed", "rows", ds, "since", since.Format("2006-01-02"))

	// 2. Refresh fuel_margin (currently a passthrough of revenue —
	//    cost_of_goods stays 0 until v1.1 adds the purchase-price UI).
	fm, err := m.refreshFuelMargin(ctx, since)
	if err != nil {
		return fmt.Errorf("mart: fuel_margin: %w", err)
	}
	m.logger.Info("mart: fuel_margin refreshed", "rows", fm)

	// 3. Refresh credit_outstanding (as-of today for every customer
	//    with a credit sale in [since, today]).
	co, err := m.refreshCreditOutstanding(ctx, since)
	if err != nil {
		return fmt.Errorf("mart: credit_outstanding: %w", err)
	}
	m.logger.Info("mart: credit_outstanding refreshed", "rows", co)

	// 4. Refresh the FuelMind Score for every day in [since, today].
	sc, err := m.refreshScores(ctx, since)
	if err != nil {
		return fmt.Errorf("mart: station_health_score: %w", err)
	}
	m.logger.Info("mart: station_health_score refreshed", "rows", sc)

	return nil
}

// MaterializeAfterIngest is a convenience for the watcher. It figures
// out the date(s) touched by the batch and refreshes from the
// earliest one. Cheap because the per-day UPSERT is bounded.
func (m *Mart) MaterializeAfterIngest(ctx context.Context) error {
	// Find the earliest date in transactions that hasn't been
	// materialized in the last 5 minutes (i.e. recent ingest
	// activity). For v1 we just refresh the last 2 days — the daily
	// mart keys are bounded by date and the cost is trivial.
	return m.MaterializeSince(ctx, time.Now().AddDate(0, 0, -2))
}

// MaterializeAll refreshes all mart rows across the entire transaction history.
// Useful for full rebuilds, backfills, and database repairs.
func (m *Mart) MaterializeAll(ctx context.Context) error {
	var earliest string
	err := m.store.DB().QueryRowContext(ctx, `
		SELECT COALESCE(MIN(substr(transaction_time, 1, 10)), '')
		FROM transactions
	`).Scan(&earliest)
	if err != nil || earliest == "" {
		return m.MaterializeSince(ctx, time.Now().AddDate(0, 0, -30))
	}
	t, err := time.Parse("2006-01-02", earliest)
	if err != nil {
		return m.MaterializeSince(ctx, time.Now().AddDate(0, 0, -365))
	}
	return m.MaterializeSince(ctx, t)
}

// refreshDailySales UPSERTs daily_sales rows for [since, today].
func (m *Mart) refreshDailySales(ctx context.Context, since time.Time) (int, error) {
	q := `
		INSERT INTO daily_sales (date, product_code, volume_liters, revenue, transaction_count, updated_at)
		SELECT
			substr(t.transaction_time, 1, 10) AS date,
			t.product_code,
			COALESCE(SUM(t.quantity_liters), 0) AS volume_liters,
			COALESCE(SUM(t.total_amount), 0) AS revenue,
			COUNT(*) AS transaction_count,
			CURRENT_TIMESTAMP
		FROM transactions t
		WHERE substr(t.transaction_time, 1, 10) >= ? AND substr(t.transaction_time, 1, 10) <= ?
		GROUP BY substr(t.transaction_time, 1, 10), t.product_code
		ON CONFLICT (date, product_code) DO UPDATE SET
			volume_liters     = excluded.volume_liters,
			revenue           = excluded.revenue,
			transaction_count = excluded.transaction_count,
			updated_at        = CURRENT_TIMESTAMP
	`
	sinceStr := since.Format("2006-01-02")
	todayStr := time.Now().Format("2006-01-02")
	res, err := m.store.DB().ExecContext(ctx, q, sinceStr, todayStr)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// refreshFuelMargin UPSERTs fuel_margin rows. In v1 the cost of
// goods is 0 (no purchase-price feed), so margin_pct is 0 for every
// row. v1.1 will add the cost input.
func (m *Mart) refreshFuelMargin(ctx context.Context, since time.Time) (int, error) {
	q := `
		INSERT INTO fuel_margin (date, product_code, revenue, cost_of_goods, margin_amount, margin_pct, updated_at)
		SELECT
			date,
			product_code,
			revenue,
			0.0 AS cost_of_goods,
			revenue AS margin_amount,
			0.0 AS margin_pct,
			CURRENT_TIMESTAMP
		FROM daily_sales
		WHERE date >= ? AND date <= ?
		ON CONFLICT (date, product_code) DO UPDATE SET
			revenue      = excluded.revenue,
			cost_of_goods = excluded.cost_of_goods,
			margin_amount = excluded.margin_amount,
			margin_pct   = excluded.margin_pct,
			updated_at   = CURRENT_TIMESTAMP
	`
	sinceStr := since.Format("2006-01-02")
	todayStr := time.Now().Format("2006-01-02")
	res, err := m.store.DB().ExecContext(ctx, q, sinceStr, todayStr)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// refreshCreditOutstanding aggregates per-customer running balance
// for as_of_date = today. v1 only sees credit sales (no payment
// feed); the balance is simply the sum of all credit sales for the
// customer. v1.1 will subtract payments.
func (m *Mart) refreshCreditOutstanding(ctx context.Context, since time.Time) (int, error) {
	// Get dates with any transaction activity in [since, today].
	dates, err := m.datesWithActivity(ctx, since)
	if err != nil {
		return 0, err
	}
	var totalUpdated int
	for _, dateStr := range dates {
		q := `
			INSERT INTO credit_outstanding
				(customer_phone, as_of_date, outstanding_amount, transaction_count, days_overdue, updated_at)
			SELECT
				customer_phone,
				? AS as_of_date,
				COALESCE(SUM(total_amount), 0) AS outstanding_amount,
				COUNT(*) AS transaction_count,
				CASE
					WHEN MAX(substr(transaction_time, 1, 10)) < ? THEN
						CAST(julianday(?) - julianday(MAX(substr(transaction_time, 1, 10))) AS INTEGER)
					ELSE 0
				END AS days_overdue,
				CURRENT_TIMESTAMP
			FROM transactions
			WHERE payment_method = 'CREDIT' AND customer_phone IS NOT NULL AND customer_phone != ''
				AND substr(transaction_time, 1, 10) <= ?
			GROUP BY customer_phone
			ON CONFLICT (customer_phone, as_of_date) DO UPDATE SET
				outstanding_amount = excluded.outstanding_amount,
				transaction_count  = excluded.transaction_count,
				days_overdue       = excluded.days_overdue,
				updated_at         = CURRENT_TIMESTAMP
		`
		res, err := m.store.DB().ExecContext(ctx, q, dateStr, dateStr, dateStr, dateStr)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		totalUpdated += int(n)
	}
	return totalUpdated, nil
}

// refreshScores computes the FuelMind Score for every date in
// [since, today]. v1 algorithm: a simple weighted sum of clear-cut
// thresholds. See score.go for the implementation.
func (m *Mart) refreshScores(ctx context.Context, since time.Time) (int, error) {
	dates, err := m.datesWithActivity(ctx, since)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, d := range dates {
		s, err := computeScore(ctx, m.store.DB(), d)
		if err != nil {
			m.logger.Warn("compute score", "date", d, "err", err)
			continue
		}
		if err := upsertScore(ctx, m.store.DB(), d, s); err != nil {
			m.logger.Warn("upsert score", "date", d, "err", err)
			continue
		}
		count++
	}
	return count, nil
}

// datesWithActivity returns the set of dates between [since, today]
// that have at least one transaction.
func (m *Mart) datesWithActivity(ctx context.Context, since time.Time) ([]string, error) {
	q := `
		SELECT DISTINCT substr(transaction_time, 1, 10) AS d
		FROM transactions
		WHERE substr(transaction_time, 1, 10) >= ? AND substr(transaction_time, 1, 10) <= ?
		ORDER BY d ASC
	`
	rows, err := m.store.DB().QueryContext(ctx, q,
		since.Format("2006-01-02"),
		time.Now().Format("2006-01-02"),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// upsertScore writes one station_health_score row.
func upsertScore(ctx context.Context, db *sql.DB, date string, s score) error {
	issuesJSON, _ := json.Marshal(s.issues)
	_, err := db.ExecContext(ctx, `
		INSERT INTO station_health_score
			(date, sales_score, inventory_score, cash_score, credit_score,
			 data_quality_score, operations_score, overall_score, issues_json, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT (date) DO UPDATE SET
			sales_score        = excluded.sales_score,
			inventory_score    = excluded.inventory_score,
			cash_score         = excluded.cash_score,
			credit_score       = excluded.credit_score,
			data_quality_score = excluded.data_quality_score,
			operations_score   = excluded.operations_score,
			overall_score      = excluded.overall_score,
			issues_json        = excluded.issues_json,
			updated_at         = CURRENT_TIMESTAMP
	`,
		date, s.sales, s.inventory, s.cash, s.credit, s.dataQuality, s.operations, s.overall, string(issuesJSON),
	)
	return err
}
