package posadapter

import (
	"encoding/json"
	"testing"
	"time"
)

func TestBuildManualRowProducesACSVShapedPayload(t *testing.T) {
	when := time.Date(2026, 9, 13, 16, 30, 0, 0, time.Local)
	row, err := BuildManualRow(ManualSale{
		OccurredAt:     when,
		ProductAlias:   "HSD",
		QuantityLiters: 20,
		UnitPrice:      275.50,
		PaymentMethod:  "cash",
		PumpID:         "P1",
		Attendant:      "Ali",
	})
	if err != nil {
		t.Fatalf("BuildManualRow: %v", err)
	}
	if row.PosSourceID != ManualSourceID {
		t.Errorf("PosSourceID = %q, want %q", row.PosSourceID, ManualSourceID)
	}
	if row.PayloadKind != PayloadJSON {
		t.Errorf("PayloadKind = %q, want json", row.PayloadKind)
	}

	var payload map[string]string
	if err := json.Unmarshal(row.Payload, &payload); err != nil {
		t.Fatalf("payload is not a JSON object: %v", err)
	}
	// The payload has to carry every column the normalizer reads off a
	// CSV row, or a hand-entered sale would take a different path
	// through the pipeline than an exported one.
	for _, col := range []string{
		"pos_source_id", "external_id", "occurred_at", "product_alias",
		"quantity_liters", "unit_price", "total_amount", "payment_method",
	} {
		if payload[col] == "" {
			t.Errorf("payload is missing %q: %v", col, payload)
		}
	}
	// Total is worked out when it is not given.
	if payload["total_amount"] != "5510.00" {
		t.Errorf("total_amount = %q, want 5510.00 (20 x 275.50)", payload["total_amount"])
	}
	// Payment method is upper-cased the way the normalizer expects.
	if payload["payment_method"] != "CASH" {
		t.Errorf("payment_method = %q, want CASH", payload["payment_method"])
	}
}

func TestBuildManualRowKeepsTheTotalThatWasCharged(t *testing.T) {
	// A station rounds to the note in the customer's hand. That figure is
	// what was taken, so it must survive rather than being recomputed.
	row, err := BuildManualRow(ManualSale{
		OccurredAt:     time.Now().Add(-time.Hour),
		ProductAlias:   "HSD",
		QuantityLiters: 20,
		UnitPrice:      275.50,
		TotalAmount:    5500,
		PaymentMethod:  "CASH",
	})
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]string
	_ = json.Unmarshal(row.Payload, &payload)
	if payload["total_amount"] != "5500.00" {
		t.Errorf("total_amount = %q, want the 5500.00 that was actually charged", payload["total_amount"])
	}
}

func TestBuildManualRowRefusesWhatCannotBeCounted(t *testing.T) {
	ok := ManualSale{
		OccurredAt:     time.Now().Add(-time.Hour),
		ProductAlias:   "HSD",
		QuantityLiters: 20,
		UnitPrice:      275.50,
		PaymentMethod:  "CASH",
	}
	cases := []struct {
		name      string
		mutate    func(*ManualSale)
		wantField string
	}{
		{"no time", func(s *ManualSale) { s.OccurredAt = time.Time{} }, "occurred_at"},
		{"in the future", func(s *ManualSale) { s.OccurredAt = time.Now().Add(48 * time.Hour) }, "occurred_at"},
		{"no product", func(s *ManualSale) { s.ProductAlias = "" }, "product_alias"},
		{"no litres", func(s *ManualSale) { s.QuantityLiters = 0 }, "quantity_liters"},
		{"negative litres", func(s *ManualSale) { s.QuantityLiters = -5 }, "quantity_liters"},
		{"no price", func(s *ManualSale) { s.UnitPrice = 0 }, "unit_price"},
		{"credit with no customer", func(s *ManualSale) { s.PaymentMethod = "CREDIT" }, "customer_phone"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sale := ok
			c.mutate(&sale)
			_, err := BuildManualRow(sale)
			if err == nil {
				t.Fatalf("BuildManualRow accepted %s", c.name)
			}
			var se ManualSaleError
			if !asManualSaleError(err, &se) {
				t.Fatalf("error is not a ManualSaleError: %v", err)
			}
			if se.Field != c.wantField {
				t.Errorf("Field = %q, want %q", se.Field, c.wantField)
			}
			if se.Message == "" {
				t.Error("the owner was given no explanation")
			}
		})
	}
}

// The same sale typed twice must produce the same payload, so the raw
// layer's unique hash turns the second one into a no-op instead of
// doubling the day's revenue.
func TestBuildManualRowIsStableForTheSameSale(t *testing.T) {
	sale := ManualSale{
		OccurredAt:     time.Date(2026, 9, 13, 16, 30, 0, 0, time.Local),
		ProductAlias:   "HSD",
		QuantityLiters: 20,
		UnitPrice:      275.50,
		PaymentMethod:  "CASH",
	}
	a, err := BuildManualRow(sale)
	if err != nil {
		t.Fatal(err)
	}
	b, err := BuildManualRow(sale)
	if err != nil {
		t.Fatal(err)
	}
	if string(a.Payload) != string(b.Payload) {
		t.Errorf("the same sale produced two payloads:\n%s\n%s", a.Payload, b.Payload)
	}

	// A genuinely different sale at the same moment still counts.
	sale.QuantityLiters = 25
	c, _ := BuildManualRow(sale)
	if string(c.Payload) == string(a.Payload) {
		t.Error("a different sale produced the same payload, so it would be swallowed as a duplicate")
	}
}

func asManualSaleError(err error, out *ManualSaleError) bool {
	se, ok := err.(ManualSaleError)
	if ok {
		*out = se
	}
	return ok
}
