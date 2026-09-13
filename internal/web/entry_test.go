package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/fuelmind/fuelmind/internal/mart"
	"github.com/fuelmind/fuelmind/internal/relay"
	"github.com/fuelmind/fuelmind/internal/storage"
)

// entryServer is a logged-in server whose mart recomputes after entry,
// like the real one.
func entryServer(t *testing.T) (*Server, func(method, path string, form url.Values) *http.Response) {
	t.Helper()
	srv, do := loggedInServer(t)
	srv.Mart = mart.New(srv.store, srv.logger)
	return srv, func(method, path string, form url.Values) *http.Response {
		return do(method, path, form).Result()
	}
}

func seedDieselSale(t *testing.T, s *storage.Storage, date string, liters, revenue float64) {
	t.Helper()
	ctx := context.Background()
	res, err := s.DB().ExecContext(ctx, `
		INSERT INTO raw_pos_transactions (pos_source_id, raw_payload, payload_hash, ingestion_batch_id)
		VALUES ('lane_1', '{}', ?, 'b1')`, date+"-hash")
	if err != nil {
		t.Fatal(err)
	}
	rawID, _ := res.LastInsertId()
	if _, err := s.DB().ExecContext(ctx, `
		INSERT INTO transactions (raw_transaction_id, product_code, quantity_liters, unit_price,
			total_amount, payment_method, transaction_time)
		VALUES (?, 'DIESEL', ?, 275.5, ?, 'CASH', ?)`,
		rawID, liters, revenue, date+"T10:00:00+05:00"); err != nil {
		t.Fatal(err)
	}
}

// Entering a purchase price turns the Margin page from "no cost recorded"
// into real numbers, including for days already in the database.
func TestSettingsPriceMakesMarginReal(t *testing.T) {
	srv, do := entryServer(t)
	today := time.Now().Format("2006-01-02")
	seedDieselSale(t, srv.store, today, 100, 27550)
	if err := srv.Mart.MaterializeSince(context.Background(), time.Now().AddDate(0, 0, -1)); err != nil {
		t.Fatal(err)
	}

	body := readBody(t, do("GET", "/margin", nil))
	if !strings.Contains(body, "No purchase prices recorded yet") {
		t.Error("margin page should say prices are missing before any are entered")
	}

	resp := do("POST", "/settings", url.Values{
		"action": {"price"}, "product_code": {"DIESEL"}, "cost": {"250"}, "effective_date": {today},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("saving a price = %d", resp.StatusCode)
	}
	body = readBody(t, do("GET", "/margin", nil))
	if !strings.Contains(body, "PKR 25,000") || !strings.Contains(body, "PKR 2,550") {
		t.Errorf("margin page missing cost/margin after entering a price:\n%s", body)
	}
}

// Recording a payment on the customer page lowers the balance.
func TestCustomerPaymentLowersTheBalance(t *testing.T) {
	srv, do := entryServer(t)
	ctx := context.Background()
	today := time.Now().Format("2006-01-02")
	res, err := srv.store.DB().ExecContext(ctx, `
		INSERT INTO raw_pos_transactions (pos_source_id, raw_payload, payload_hash, ingestion_batch_id)
		VALUES ('lane_1', '{}', 'c-hash', 'b1')`)
	if err != nil {
		t.Fatal(err)
	}
	rawID, _ := res.LastInsertId()
	if _, err := srv.store.DB().ExecContext(ctx, `
		INSERT INTO transactions (raw_transaction_id, product_code, quantity_liters, unit_price,
			total_amount, payment_method, customer_phone, transaction_time)
		VALUES (?, 'DIESEL', 40, 275.5, 11020, 'CREDIT', '+923001112223', ?)`,
		rawID, today+"T10:00:00+05:00"); err != nil {
		t.Fatal(err)
	}
	if err := srv.Mart.MaterializeSince(ctx, time.Now().AddDate(0, 0, -1)); err != nil {
		t.Fatal(err)
	}

	body := readBody(t, do("GET", "/customer?phone=%2B923001112223", nil))
	if !strings.Contains(body, "PKR 11,020") {
		t.Fatalf("customer page missing the balance:\n%s", body)
	}
	do("POST", "/customer?phone=%2B923001112223", url.Values{"amount": {"5,020"}, "paid_on": {today}, "method": {"CASH"}})
	body = readBody(t, do("GET", "/customer?phone=%2B923001112223", nil))
	if !strings.Contains(body, "PKR 6,000") {
		t.Errorf("balance did not fall to 6,000 after a payment:\n%s", body)
	}
	credit := readBody(t, do("GET", "/credit", nil))
	if !strings.Contains(credit, "PKR 6,000") {
		t.Errorf("credit list still shows the old balance")
	}
}

// Remote questions are off until the owner turns them on in Settings.
func TestSettingsTogglesRemoteAsk(t *testing.T) {
	srv, do := entryServer(t)
	srv.CloudConfigured = true
	ctx := context.Background()
	if relay.Enabled(ctx, srv.store) {
		t.Fatal("remote questions should start off")
	}
	body := readBody(t, do("GET", "/settings", nil))
	if !strings.Contains(body, "Turn on") {
		t.Error("settings page has no way to turn remote questions on")
	}
	do("POST", "/settings", url.Values{"action": {"remote_ask"}, "remote_ask": {"on"}})
	if !relay.Enabled(ctx, srv.store) {
		t.Fatal("turning remote questions on did not stick")
	}
	do("POST", "/settings", url.Values{"action": {"remote_ask"}, "remote_ask": {"off"}})
	if relay.Enabled(ctx, srv.store) {
		t.Error("turning remote questions off did not stick")
	}
}

// Without a cloud, Settings says so rather than offering a switch that
// cannot work.
func TestSettingsHidesRemoteAskWithoutCloud(t *testing.T) {
	_, do := entryServer(t)
	body := readBody(t, do("GET", "/settings", nil))
	if strings.Contains(body, "Turn on") {
		t.Error("offered remote questions with no cloud configured")
	}
	if !strings.Contains(body, "not connected to a FuelMind service") {
		t.Error("settings page does not explain why remote questions are unavailable")
	}
}

func TestSettingsRejectsBadPrice(t *testing.T) {
	_, do := entryServer(t)
	body := readBody(t, do("POST", "/settings", url.Values{
		"action": {"price"}, "product_code": {"DIESEL"}, "cost": {"free"},
	}))
	if !strings.Contains(body, "Enter the price you paid per litre") {
		t.Error("a bad price should be explained, not silently ignored")
	}
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	buf := new(strings.Builder)
	if _, err := copyTo(buf, resp); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func copyTo(dst *strings.Builder, resp *http.Response) (int64, error) {
	b := make([]byte, 32*1024)
	var n int64
	for {
		r, err := resp.Body.Read(b)
		if r > 0 {
			dst.Write(b[:r])
			n += int64(r)
		}
		if err != nil {
			return n, nil
		}
	}
}
