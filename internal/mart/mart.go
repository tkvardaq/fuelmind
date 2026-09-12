// Package mart is the business data mart (spec §3.3, Layer 3 of the
// three-layer data model). It pre-aggregates the normalized
// `transactions` table into the query-ready tables the dashboard and
// intent router read from: daily_sales, fuel_margin, credit_outstanding,
// station_health_score.
//
// The materialization job runs after every ingestion batch (from the
// earliest business date in that batch, so backfills are picked up) and
// on a 15-minute timer for the trailing week.
package mart

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/fuelmind/fuelmind/internal/storage"
)

const dateLayout = "2006-01-02"

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
// today] inclusive. Idempotent (UPSERT semantics).
func (m *Mart) MaterializeSince(ctx context.Context, since time.Time) error {
	sinceStr := since.Format(dateLayout)
	todayStr := time.Now().Format(dateLayout)
	if sinceStr > todayStr {
		sinceStr = todayStr
	}

	ds, err := m.refreshDailySales(ctx, sinceStr, todayStr)
	if err != nil {
		return fmt.Errorf("mart: daily_sales: %w", err)
	}
	fm, err := m.refreshFuelMargin(ctx, sinceStr, todayStr)
	if err != nil {
		return fmt.Errorf("mart: fuel_margin: %w", err)
	}
	dates, err := m.datesToScore(ctx, sinceStr, todayStr)
	if err != nil {
		return fmt.Errorf("mart: dates: %w", err)
	}
	co, err := m.refreshCreditOutstanding(ctx, dates)
	if err != nil {
		return fmt.Errorf("mart: credit_outstanding: %w", err)
	}
	sc, err := m.refreshScores(ctx, dates)
	if err != nil {
		return fmt.Errorf("mart: station_health_score: %w", err)
	}
	m.logger.Debug("mart refreshed", "since", sinceStr, "daily_sales", ds, "fuel_margin", fm,
		"credit_outstanding", co, "scores", sc)
	return nil
}

// MaterializeAll refreshes all mart rows across the entire history.
func (m *Mart) MaterializeAll(ctx context.Context) error {
	var earliest sql.NullString
	err := m.store.DB().QueryRowContext(ctx, `
		SELECT MIN(d) FROM (
			SELECT MIN(substr(transaction_time, 1, 10)) AS d FROM transactions
			UNION ALL
			SELECT MIN(occurred_date) FROM raw_unresolved
		)`).Scan(&earliest)
	if err != nil {
		return fmt.Errorf("mart: earliest date: %w", err)
	}
	if !earliest.Valid || earliest.String == "" {
		return m.MaterializeSince(ctx, time.Now())
	}
	t, err := time.ParseInLocation(dateLayout, earliest.String, time.Local)
	if err != nil {
		return fmt.Errorf("mart: bad earliest date %q: %w", earliest.String, err)
	}
	return m.MaterializeSince(ctx, t)
}

func (m *Mart) refreshDailySales(ctx context.Context, since, until string) (int, error) {
	res, err := m.store.DB().ExecContext(ctx, `
		INSERT INTO daily_sales (date, product_code, volume_liters, revenue, transaction_count, updated_at)
		SELECT substr(t.transaction_time, 1, 10), t.product_code,
		       COALESCE(SUM(t.quantity_liters), 0), COALESCE(SUM(t.total_amount), 0),
		       COUNT(*), CURRENT_TIMESTAMP
		FROM transactions t
		WHERE substr(t.transaction_time, 1, 10) BETWEEN ? AND ?
		GROUP BY substr(t.transaction_time, 1, 10), t.product_code
		ON CONFLICT (date, product_code) DO UPDATE SET
			volume_liters     = excluded.volume_liters,
			revenue           = excluded.revenue,
			transaction_count = excluded.transaction_count,
			updated_at        = CURRENT_TIMESTAMP
	`, since, until)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// refreshFuelMargin UPSERTs fuel_margin rows. v1 has no purchase-price
// feed, so cost_of_goods and margin are 0 (unknown), not "equal to
// revenue".
func (m *Mart) refreshFuelMargin(ctx context.Context, since, until string) (int, error) {
	res, err := m.store.DB().ExecContext(ctx, `
		INSERT INTO fuel_margin (date, product_code, revenue, cost_of_goods, margin_amount, margin_pct, updated_at)
		SELECT date, product_code, revenue, 0.0, 0.0, 0.0, CURRENT_TIMESTAMP
		FROM daily_sales WHERE date BETWEEN ? AND ?
		ON CONFLICT (date, product_code) DO UPDATE SET
			revenue    = excluded.revenue,
			updated_at = CURRENT_TIMESTAMP
	`, since, until)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// refreshCreditOutstanding snapshots every credit customer's balance as
// of each date. v1 sees credit sales only (no payment feed), so the
// balance is the sum of credit sales up to that date and days_overdue is
// the number of days since the customer's most recent credit purchase.
func (m *Mart) refreshCreditOutstanding(ctx context.Context, dates []string) (int, error) {
	total := 0
	for _, d := range dates {
		res, err := m.store.DB().ExecContext(ctx, `
			INSERT INTO credit_outstanding
				(customer_phone, as_of_date, outstanding_amount, transaction_count, days_overdue, updated_at)
			SELECT customer_phone, ?, COALESCE(SUM(total_amount), 0), COUNT(*),
			       CAST(julianday(?) - julianday(MAX(substr(transaction_time, 1, 10))) AS INTEGER),
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
		`, d, d, d)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		total += int(n)
	}
	return total, nil
}

func (m *Mart) refreshScores(ctx context.Context, dates []string) (int, error) {
	count := 0
	for _, d := range dates {
		s, err := computeScore(ctx, m.store.DB(), d)
		if err != nil {
			return count, fmt.Errorf("compute score %s: %w", d, err)
		}
		if err := upsertScore(ctx, m.store.DB(), d, s); err != nil {
			return count, fmt.Errorf("upsert score %s: %w", d, err)
		}
		count++
	}
	return count, nil
}

// datesToScore returns the business dates in [since, until] that get a
// FuelMind Score:
//   - every date with normalized sales,
//   - every date with raw rows that could not be normalized (so a day of
//     unreadable data raises an issue instead of looking empty),
//   - every past date (before today) since the station's first data, so
//     a day with no POS export at all is flagged too.
func (m *Mart) datesToScore(ctx context.Context, since, until string) ([]string, error) {
	set := map[string]bool{}
	rows, err := m.store.DB().QueryContext(ctx, `
		SELECT DISTINCT substr(transaction_time, 1, 10) FROM transactions
		WHERE substr(transaction_time, 1, 10) BETWEEN ?1 AND ?2
		UNION
		SELECT DISTINCT occurred_date FROM raw_unresolved
		WHERE occurred_date BETWEEN ?1 AND ?2`, since, until)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var d sql.NullString
		if err := rows.Scan(&d); err != nil {
			rows.Close()
			return nil, err
		}
		if d.Valid && d.String != "" {
			set[d.String] = true
		}
	}
	rows.Close()

	var first sql.NullString
	if err := m.store.DB().QueryRowContext(ctx,
		`SELECT MIN(substr(transaction_time, 1, 10)) FROM transactions`).Scan(&first); err != nil {
		return nil, err
	}
	if first.Valid && first.String != "" {
		start := since
		if first.String > start {
			start = first.String
		}
		yesterday := time.Now().AddDate(0, 0, -1).Format(dateLayout)
		if yesterday > until {
			yesterday = until
		}
		if t, err := time.Parse(dateLayout, start); err == nil {
			for d := t; d.Format(dateLayout) <= yesterday; d = d.AddDate(0, 0, 1) {
				set[d.Format(dateLayout)] = true
			}
		}
	}

	out := make([]string, 0, len(set))
	for d := range set {
		out = append(out, d)
	}
	sort.Strings(out)
	return out, nil
}

func upsertScore(ctx context.Context, db *sql.DB, date string, s score) error {
	issuesJSON, err := json.Marshal(s.issues)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `
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
	`, date, s.sales, s.inventory, s.cash, s.credit, s.dataQuality, s.operations, s.overall, string(issuesJSON))
	return err
}
