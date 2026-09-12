package mart

import (
	"context"
	"testing"

	"github.com/fuelmind/fuelmind/internal/storage"
)

// Margin is only real once the owner records what the fuel cost.
func TestMarginUsesPurchasePrices(t *testing.T) {
	m, s := newTestMart(t)
	ctx := context.Background()
	// 100 litres of diesel sold for 27,550 on 2026-09-07.
	seedTransactions(t, s, "2026-09-07", "DIESEL", "CASH", 1, 27550)
	if _, err := s.DB().Exec(`UPDATE transactions SET quantity_liters = 100`); err != nil {
		t.Fatal(err)
	}

	// No price recorded yet: cost stays 0 and is reported as unknown.
	if err := m.MaterializeSince(ctx, parseDate(t, "2026-09-07")); err != nil {
		t.Fatal(err)
	}
	var cost, margin, pct float64
	q := `SELECT cost_of_goods, margin_amount, margin_pct FROM fuel_margin WHERE date='2026-09-07' AND product_code='DIESEL'`
	if err := s.DB().QueryRow(q).Scan(&cost, &margin, &pct); err != nil {
		t.Fatal(err)
	}
	if cost != 0 || margin != 0 || pct != 0 {
		t.Errorf("without a purchase price: cost=%v margin=%v pct=%v, want all zero", cost, margin, pct)
	}

	// Owner records 250.00 per litre from 2026-09-01.
	if err := s.SetPurchasePrice(ctx, storage.PurchasePrice{
		ProductCode: "DIESEL", EffectiveDate: "2026-09-01", CostPerLiter: 250,
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.MaterializeSince(ctx, parseDate(t, "2026-09-07")); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow(q).Scan(&cost, &margin, &pct); err != nil {
		t.Fatal(err)
	}
	if cost != 25000 || margin != 2550 {
		t.Errorf("cost=%v margin=%v, want 25000 and 2550", cost, margin)
	}
	if pct < 9.2 || pct > 9.3 {
		t.Errorf("margin_pct = %v, want about 9.26", pct)
	}
}

// A later price applies from its own date forward; earlier days keep the
// price that was in force then.
func TestMarginUsesThePriceInForceOnTheDay(t *testing.T) {
	m, s := newTestMart(t)
	ctx := context.Background()
	for _, d := range []string{"2026-09-05", "2026-09-09"} {
		seedTransactions(t, s, d, "DIESEL", "CASH", 1, 30000)
	}
	if _, err := s.DB().Exec(`UPDATE transactions SET quantity_liters = 100`); err != nil {
		t.Fatal(err)
	}
	for _, p := range []storage.PurchasePrice{
		{ProductCode: "DIESEL", EffectiveDate: "2026-09-01", CostPerLiter: 250},
		{ProductCode: "DIESEL", EffectiveDate: "2026-09-08", CostPerLiter: 280},
	} {
		if err := s.SetPurchasePrice(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.MaterializeSince(ctx, parseDate(t, "2026-09-05")); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		date string
		cost float64
	}{{"2026-09-05", 25000}, {"2026-09-09", 28000}} {
		var got float64
		if err := s.DB().QueryRow(
			`SELECT cost_of_goods FROM fuel_margin WHERE date=? AND product_code='DIESEL'`, c.date).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != c.cost {
			t.Errorf("%s cost = %v, want %v", c.date, got, c.cost)
		}
	}
}

// A repayment reduces the balance and clears the overdue counter.
func TestCreditBalanceFallsWhenTheCustomerPays(t *testing.T) {
	m, s := newTestMart(t)
	ctx := context.Background()
	seedCreditTransactions(t, s, "2026-09-05", "+923001234567", 2, 5000) // owes 10,000

	if err := m.MaterializeSince(ctx, parseDate(t, "2026-09-05")); err != nil {
		t.Fatal(err)
	}
	amount, overdue := creditRow(t, s, "+923001234567")
	if amount != 10000 {
		t.Fatalf("balance = %v, want 10000", amount)
	}

	if err := s.RecordCreditPayment(ctx, storage.CreditPayment{
		CustomerPhone: "+923001234567", PaidOn: "2026-09-06", Amount: 4000, Method: "CASH",
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.MaterializeSince(ctx, parseDate(t, "2026-09-05")); err != nil {
		t.Fatal(err)
	}
	amount, overdue = creditRow(t, s, "+923001234567")
	if amount != 6000 {
		t.Errorf("after a 4000 payment the balance is %v, want 6000", amount)
	}
	if overdue == 0 {
		t.Errorf("still owing 6000, so days_overdue should not be 0")
	}

	// Paying the rest clears the balance and the overdue counter.
	if err := s.RecordCreditPayment(ctx, storage.CreditPayment{
		CustomerPhone: "+923001234567", PaidOn: "2026-09-07", Amount: 6000,
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.MaterializeSince(ctx, parseDate(t, "2026-09-05")); err != nil {
		t.Fatal(err)
	}
	amount, overdue = creditRow(t, s, "+923001234567")
	if amount != 0 || overdue != 0 {
		t.Errorf("fully paid up: balance=%v overdue=%v, want 0 and 0", amount, overdue)
	}
}

// A settled customer must not count towards the high-credit alert.
func TestPaidUpCustomerDoesNotTriggerTheCreditAlert(t *testing.T) {
	m, s := newTestMart(t)
	ctx := context.Background()
	seedCreditTransactions(t, s, "2026-09-05", "+923009999999", 1, 100000)
	if err := s.RecordCreditPayment(ctx, storage.CreditPayment{
		CustomerPhone: "+923009999999", PaidOn: "2026-09-05", Amount: 100000,
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.MaterializeSince(ctx, parseDate(t, "2026-09-05")); err != nil {
		t.Fatal(err)
	}
	var credit int
	if err := s.DB().QueryRow(
		`SELECT credit_score FROM station_health_score WHERE date='2026-09-05'`).Scan(&credit); err != nil {
		t.Fatal(err)
	}
	if credit != 100 {
		t.Errorf("credit score = %d for a customer who paid in full, want 100", credit)
	}
}

func creditRow(t *testing.T, s *storage.Storage, phone string) (amount float64, overdue int) {
	t.Helper()
	if err := s.DB().QueryRow(`
		SELECT outstanding_amount, days_overdue FROM credit_outstanding
		WHERE customer_phone = ? ORDER BY as_of_date DESC LIMIT 1`, phone).Scan(&amount, &overdue); err != nil {
		t.Fatal(err)
	}
	return amount, overdue
}
