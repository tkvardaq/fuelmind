package mart

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// score is the in-memory shape of a single FuelMind Score row. Each
// component is 0-100; the overall is the weighted sum.
//
// v1 algorithm: deliberately simple, clear-cut thresholds, no ML.
// v2 will add anomaly detection and trend-based adjustments.
type score struct {
	sales        int
	inventory    int
	cash         int
	credit       int
	dataQuality  int
	operations   int
	overall      int
	issues       []issue
}

// issue is a structured description of something the operator
// should look at. The LLM (Phase 7) reads this list verbatim to
// explain the score in natural language.
type issue struct {
	Date     string `json:"date"`
	Category string `json:"category"` // sales|inventory|cash|credit|data|operations
	Severity string `json:"severity"` // info|warn|alert
	Code     string `json:"code"`     // e.g. "no_transactions", "high_credit_outstanding"
	Message  string `json:"message"`
}

// computeScore reads the day's data and returns a score. The
// implementation is intentionally explicit (no clever aggregations)
// so it's obvious in code review what triggers each issue.
//
// Weights (must sum to 100):
//   sales         25
//   inventory     15  (v1: always 100 because we have no inventory feed yet)
//   cash          15  (v1: always 100; needs shift data in v1.1)
//   credit        15
//   data_quality  15
//   operations    15
func computeScore(ctx context.Context, db *sql.DB, date string) (score, error) {
	s := score{
		inventory: 100, // placeholder until inventory_variance lands
		cash:      100, // placeholder until cash_variance lands
	}
	s.issues = []issue{}

	// --- sales_score: based on whether we have transactions at all,
	// and whether revenue is in a plausible range.
	var txCount int
	var revenue float64
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(total_amount), 0)
		FROM transactions
		WHERE substr(transaction_time, 1, 10) = ?
	`, date).Scan(&txCount, &revenue)
	if err != nil {
		return s, fmt.Errorf("sales: %w", err)
	}
	switch {
	case txCount == 0:
		s.sales = 50 // no data is better than bad data, but not great
		s.issues = append(s.issues, issue{
			Date: date, Category: "sales", Severity: "warn",
			Code: "no_transactions", Message: "No transactions recorded for this day.",
		})
	case txCount < 10:
		s.sales = 80
		s.issues = append(s.issues, issue{
			Date: date, Category: "sales", Severity: "info",
			Code: "low_transaction_count", Message: fmt.Sprintf("Only %d transactions recorded.", txCount),
		})
	case revenue < 1000:
		s.sales = 70
		s.issues = append(s.issues, issue{
			Date: date, Category: "sales", Severity: "info",
			Code: "low_revenue", Message: fmt.Sprintf("Revenue of %.2f is unusually low.", revenue),
		})
	default:
		s.sales = 100
	}

	// --- credit_score: penalize if any single customer has more than
	// 50,000 in outstanding credit (a real PK station's threshold).
	var highCreditCount int
	err = db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM credit_outstanding
		WHERE as_of_date = ? AND outstanding_amount > 50000
	`, date).Scan(&highCreditCount)
	if err != nil {
		return s, fmt.Errorf("credit: %w", err)
	}
	if highCreditCount > 0 {
		s.credit = 60
		s.issues = append(s.issues, issue{
			Date: date, Category: "credit", Severity: "alert",
			Code: "high_credit_outstanding",
			Message: fmt.Sprintf("%d customer(s) have more than 50,000 PKR outstanding.", highCreditCount),
		})
	} else {
		// No high credit outstanding; if there are no transactions
		// there's no credit risk, and normal days get full score.
		s.credit = 100
	}

	// --- data_quality_score: penalize if there are raw rows that
	// couldn't be normalized.
	var unnorm int
	err = db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM raw_pos_transactions r
		LEFT JOIN transactions t ON t.raw_transaction_id = r.id
		WHERE t.id IS NULL
		AND received_at >= ? AND received_at < ?
	`, date, nextDay(date)).Scan(&unnorm)
	if err != nil {
		return s, fmt.Errorf("data_quality: %w", err)
	}
	if unnorm > 0 {
		s.dataQuality = 70
		s.issues = append(s.issues, issue{
			Date: date, Category: "data", Severity: "warn",
			Code: "unnormalized_rows",
			Message: fmt.Sprintf("%d raw rows were not normalized (likely unknown product alias or bad data).", unnorm),
		})
	} else {
		s.dataQuality = 100
	}

	// --- operations_score: high-level "is anything obviously wrong"
	// check. v1: always 100 unless data quality is degraded.
	if s.dataQuality < 100 {
		s.operations = 90
	} else {
		s.operations = 100
	}

	// --- overall: weighted sum.
	s.overall = (s.sales*25 + s.inventory*15 + s.cash*15 + s.credit*15 +
		s.dataQuality*15 + s.operations*15) / 100

	return s, nil
}

// nextDay returns date + 1 day as a string in the same format.
// Used to bound the unnormalized-rows query to the day in question.
func nextDay(date string) string {
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		return date
	}
	return t.AddDate(0, 0, 1).Format("2006-01-02")
}
