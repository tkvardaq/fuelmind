package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// --- purchase prices (what the station paid per litre) ---

// PurchasePrice is one price, effective from a date.
type PurchasePrice struct {
	ProductCode   string
	EffectiveDate string // YYYY-MM-DD
	CostPerLiter  float64
	Note          string
}

// SetPurchasePrice records (or replaces) the cost per litre for a
// product from a date forward.
func (s *Storage) SetPurchasePrice(ctx context.Context, p PurchasePrice) error {
	if p.CostPerLiter < 0 {
		return errors.New("storage: cost per litre cannot be negative")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO fuel_purchase_prices (product_code, effective_date, cost_per_liter, note)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (product_code, effective_date) DO UPDATE SET
			cost_per_liter = excluded.cost_per_liter,
			note           = excluded.note
	`, p.ProductCode, p.EffectiveDate, p.CostPerLiter, nullableString(p.Note))
	return err
}

// PurchasePrices lists every recorded price, newest first.
func (s *Storage) PurchasePrices(ctx context.Context, limit int) ([]PurchasePrice, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT product_code, effective_date, cost_per_liter, COALESCE(note, '')
		FROM fuel_purchase_prices ORDER BY effective_date DESC, product_code LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PurchasePrice
	for rows.Next() {
		var p PurchasePrice
		if err := rows.Scan(&p.ProductCode, &p.EffectiveDate, &p.CostPerLiter, &p.Note); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeletePurchasePrice removes one price entry.
func (s *Storage) DeletePurchasePrice(ctx context.Context, productCode, effectiveDate string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM fuel_purchase_prices WHERE product_code = ? AND effective_date = ?`,
		productCode, effectiveDate)
	return err
}

// --- credit payments (money customers pay back) ---

// CreditPayment is one repayment.
type CreditPayment struct {
	ID            int64
	CustomerPhone string
	PaidOn        string // YYYY-MM-DD
	Amount        float64
	Method        string
	Note          string
}

// RecordCreditPayment stores a repayment from a credit customer.
func (s *Storage) RecordCreditPayment(ctx context.Context, p CreditPayment) error {
	if p.Amount <= 0 {
		return errors.New("storage: a payment must be more than zero")
	}
	if p.Method == "" {
		p.Method = "CASH"
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO credit_payments (customer_phone, paid_on, amount, method, note)
		VALUES (?, ?, ?, ?, ?)`,
		p.CustomerPhone, p.PaidOn, p.Amount, p.Method, nullableString(p.Note))
	return err
}

// CreditPayments lists a customer's repayments, newest first.
func (s *Storage) CreditPayments(ctx context.Context, phone string, limit int) ([]CreditPayment, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, customer_phone, paid_on, amount, method, COALESCE(note, '')
		FROM credit_payments WHERE customer_phone = ?
		ORDER BY paid_on DESC, id DESC LIMIT ?`, phone, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CreditPayment
	for rows.Next() {
		var p CreditPayment
		if err := rows.Scan(&p.ID, &p.CustomerPhone, &p.PaidOn, &p.Amount, &p.Method, &p.Note); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CustomerSale is one credit sale in a customer's history.
type CustomerSale struct {
	When    string
	Product string
	Liters  float64
	Amount  float64
}

// CustomerCreditHistory returns a customer's credit sales, newest first.
func (s *Storage) CustomerCreditHistory(ctx context.Context, phone string, limit int) ([]CustomerSale, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT transaction_time, product_code, quantity_liters, total_amount
		FROM transactions
		WHERE payment_method = 'CREDIT' AND customer_phone = ?
		ORDER BY transaction_time DESC LIMIT ?`, phone, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CustomerSale
	for rows.Next() {
		var c CustomerSale
		if err := rows.Scan(&c.When, &c.Product, &c.Liters, &c.Amount); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// --- margin (needs the purchase prices above) ---

// MarginRow is one day of one product's margin.
type MarginRow struct {
	Date         string
	ProductCode  string
	DisplayName  string
	Revenue      float64
	CostOfGoods  float64
	MarginAmount float64
	MarginPct    float64
	Costed       bool // false when no purchase price covers that day
}

// RecentMargin returns the margin rows for the last `days` days.
func (s *Storage) RecentMargin(ctx context.Context, days int) ([]MarginRow, error) {
	cutoff := time.Now().AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	rows, err := s.db.QueryContext(ctx, `
		SELECT fm.date, fm.product_code, fp.display_name, fm.revenue,
		       fm.cost_of_goods, fm.margin_amount, fm.margin_pct
		FROM fuel_margin fm
		JOIN fuel_products fp ON fp.product_code = fm.product_code
		WHERE fm.date >= ?
		ORDER BY fm.date DESC, fm.product_code`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MarginRow
	for rows.Next() {
		var m MarginRow
		if err := rows.Scan(&m.Date, &m.ProductCode, &m.DisplayName, &m.Revenue,
			&m.CostOfGoods, &m.MarginAmount, &m.MarginPct); err != nil {
			return nil, err
		}
		m.Costed = m.CostOfGoods > 0
		out = append(out, m)
	}
	return out, rows.Err()
}

// --- remote questions (cloud relay audit trail) ---

// RemoteQuestion is one question answered over the relay.
type RemoteQuestion struct {
	MessageID  string
	AskedBy    string
	Question   string
	Answer     string
	Route      string
	AnsweredAt time.Time
}

// RecordRemoteQuestion stores one answered question. Re-recording the
// same message id is a no-op.
func (s *Storage) RecordRemoteQuestion(ctx context.Context, q RemoteQuestion) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO remote_questions (message_id, asked_by, question, answer, route)
		VALUES (?, ?, ?, ?, ?)`, q.MessageID, nullableString(q.AskedBy), q.Question, q.Answer, q.Route)
	return err
}

// RecentRemoteQuestions returns the latest answered questions.
func (s *Storage) RecentRemoteQuestions(ctx context.Context, limit int) ([]RemoteQuestion, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT message_id, COALESCE(asked_by, ''), question, answer, route, answered_at
		FROM remote_questions ORDER BY answered_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RemoteQuestion
	for rows.Next() {
		var q RemoteQuestion
		var at sql.NullTime
		if err := rows.Scan(&q.MessageID, &q.AskedBy, &q.Question, &q.Answer, &q.Route, &at); err != nil {
			return nil, fmt.Errorf("storage: scan remote question: %w", err)
		}
		q.AnsweredAt = at.Time
		out = append(out, q)
	}
	return out, rows.Err()
}
