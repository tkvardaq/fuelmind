package mart

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// score is the in-memory shape of a single FuelMind Score row. Each
// component is 0-100.
//
// v1 algorithm: deliberately simple, clear-cut thresholds, no ML.
type score struct {
	sales       int
	inventory   int
	cash        int
	credit      int
	dataQuality int
	operations  int
	overall     int
	issues      []issue
}

// issue is a structured description of something the operator should
// look at. The LLM reads this list verbatim to explain the score.
type issue struct {
	Date     string `json:"date"`
	Category string `json:"category"` // sales|inventory|cash|credit|data|operations
	Severity string `json:"severity"` // info|warn|alert
	Code     string `json:"code"`
	Message  string `json:"message"`
}

// Weights of the measured components. Inventory and cash have no data
// feed in v1; they are stored as 100 but excluded from the overall score
// rather than handing out 30% of the score unmeasured.
const (
	wSales       = 25
	wCredit      = 15
	wDataQuality = 15
	wOperations  = 15
	wMeasured    = wSales + wCredit + wDataQuality + wOperations
)

// highCreditThreshold is the per-customer outstanding balance (PKR)
// above which the credit component is penalised.
const highCreditThreshold = 50000

func computeScore(ctx context.Context, db *sql.DB, date string) (score, error) {
	s := score{inventory: 100, cash: 100, issues: []issue{}}

	// --- sales
	var txCount int
	var revenue float64
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(total_amount), 0)
		FROM transactions WHERE substr(transaction_time, 1, 10) = ?
	`, date).Scan(&txCount, &revenue); err != nil {
		return s, fmt.Errorf("sales: %w", err)
	}
	switch {
	case txCount == 0:
		s.sales = 50
		msg := "No sales were recorded for this day."
		if date < time.Now().Format(dateLayout) {
			msg += " Check that the POS is still exporting files to the drop folder."
		}
		s.issues = append(s.issues, issue{Date: date, Category: "sales", Severity: "warn",
			Code: "no_transactions", Message: msg})
	case txCount < 10:
		s.sales = 80
		s.issues = append(s.issues, issue{Date: date, Category: "sales", Severity: "info",
			Code: "low_transaction_count", Message: fmt.Sprintf("Only %d transactions recorded.", txCount)})
	case revenue < 1000:
		s.sales = 70
		s.issues = append(s.issues, issue{Date: date, Category: "sales", Severity: "info",
			Code: "low_revenue", Message: fmt.Sprintf("Revenue of PKR %.0f is unusually low.", revenue)})
	default:
		s.sales = 100
	}

	// --- credit
	var highCredit int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM credit_outstanding
		WHERE as_of_date = ? AND outstanding_amount > ?
	`, date, highCreditThreshold).Scan(&highCredit); err != nil {
		return s, fmt.Errorf("credit: %w", err)
	}
	s.credit = 100
	if highCredit > 0 {
		s.credit = 60
		s.issues = append(s.issues, issue{Date: date, Category: "credit", Severity: "alert",
			Code:    "high_credit_outstanding",
			Message: fmt.Sprintf("%d customer(s) owe more than PKR 50,000 on credit.", highCredit)})
	}

	// --- data quality: rows we could not read, and rows whose amounts
	// don't add up.
	s.dataQuality = 100
	unresolved, aliases, err := unresolvedForDate(ctx, db, date)
	if err != nil {
		return s, fmt.Errorf("data_quality: %w", err)
	}
	if unresolved > 0 {
		s.dataQuality = 70
		msg := fmt.Sprintf("%d POS row(s) could not be read.", unresolved)
		if len(aliases) > 0 {
			msg += " Unknown product name(s): " + strings.Join(aliases, ", ") + "."
		}
		s.issues = append(s.issues, issue{Date: date, Category: "data", Severity: "warn",
			Code: "unnormalized_rows", Message: msg})
	}
	var mismatched, negative int
	if err := db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(flags LIKE '%total_mismatch%'), 0),
		       COALESCE(SUM(flags LIKE '%negative_quantity%'), 0)
		FROM transactions WHERE substr(transaction_time, 1, 10) = ?
	`, date).Scan(&mismatched, &negative); err != nil {
		return s, fmt.Errorf("flags: %w", err)
	}
	if mismatched > 0 {
		if s.dataQuality > 80 {
			s.dataQuality = 80
		}
		s.issues = append(s.issues, issue{Date: date, Category: "data", Severity: "warn",
			Code:    "total_mismatch",
			Message: fmt.Sprintf("%d sale(s) where quantity × price does not match the total charged.", mismatched)})
	}
	if negative > 0 {
		s.issues = append(s.issues, issue{Date: date, Category: "data", Severity: "info",
			Code:    "negative_quantity",
			Message: fmt.Sprintf("%d sale(s) with a negative quantity (refunds or corrections).", negative)})
	}

	// --- operations: v1 mirrors data health.
	s.operations = 100
	if s.dataQuality < 100 {
		s.operations = 90
	}

	s.overall = (s.sales*wSales + s.credit*wCredit + s.dataQuality*wDataQuality + s.operations*wOperations) / wMeasured
	return s, nil
}

// unresolvedForDate counts raw rows for a business date that could not
// be normalized and returns the distinct unknown product names.
func unresolvedForDate(ctx context.Context, db *sql.DB, date string) (int, []string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT COALESCE(u.product_alias, ''), COUNT(*),
		       COALESCE(MAX(e.error LIKE 'unknown product%'), 0)
		FROM raw_unresolved u
		LEFT JOIN raw_normalize_errors e ON e.raw_transaction_id = u.id
		WHERE u.occurred_date = ? GROUP BY 1 ORDER BY 2 DESC`, date)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	total := 0
	var names []string
	for rows.Next() {
		var alias string
		var n, unknown int
		if err := rows.Scan(&alias, &n, &unknown); err != nil {
			return 0, nil, err
		}
		total += n
		if unknown == 1 && alias != "" && len(names) < 5 {
			names = append(names, fmt.Sprintf("%q", alias))
		}
	}
	return total, names, rows.Err()
}
