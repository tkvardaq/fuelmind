package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fuelmind/fuelmind/internal/mart"
	"github.com/fuelmind/fuelmind/internal/posadapter"
	"github.com/fuelmind/fuelmind/internal/posadapter/csvwatch"
)

// fakeIngest stands in for the POS watcher so the Data page can be
// exercised without a real file-system watch.
type fakeIngest struct {
	folders []string
	rows    []posadapter.RawTransaction
	uploads []string
	// unusable is a folder Reconcile refuses, standing in for a network
	// share that is down.
	unusable string
}

func (f *fakeIngest) IngestCSV(_ context.Context, name string, data []byte) (csvwatch.Result, error) {
	f.uploads = append(f.uploads, name)
	return csvwatch.Result{RowsRead: 1, RowsInserted: 1}, nil
}

func (f *fakeIngest) IngestRows(_ context.Context, _ string, rows []posadapter.RawTransaction) (csvwatch.Result, error) {
	f.rows = append(f.rows, rows...)
	return csvwatch.Result{RowsRead: len(rows), RowsInserted: len(rows)}, nil
}

func (f *fakeIngest) Reconcile(_ context.Context, want []string) map[string]error {
	f.folders = nil
	errs := map[string]error{}
	for _, w := range want {
		if f.unusable != "" && strings.EqualFold(w, f.unusable) {
			errs[w] = context.DeadlineExceeded
			continue
		}
		f.folders = append(f.folders, w)
	}
	return errs
}

func (f *fakeIngest) Folders() []string { return f.folders }

func dataServer(t *testing.T) (*Server, *fakeIngest, func(method, path string, form url.Values) string) {
	t.Helper()
	srv, do := loggedInServer(t)
	srv.Mart = mart.New(srv.store, srv.logger)
	ing := &fakeIngest{}
	srv.Ingest = ing
	return srv, ing, func(method, path string, form url.Values) string {
		return readBody(t, do(method, path, form).Result())
	}
}

// pastSale is a date and time that is always safely in the past. A fixed
// clock time with today's date is in the future when the suite runs just
// after midnight, which the manual-entry form correctly refuses.
func pastSale() (date, clock string) {
	t := time.Now().Add(-2 * time.Hour)
	return t.Format("2006-01-02"), t.Format("15:04")
}

// The Data page is the answer to "how does my data get in?". FuelMind
// reads another system's exports, so if this page is not there, there is
// no way for an owner to connect their POS at all.
func TestDataPageOffersEveryWayIn(t *testing.T) {
	_, _, do := dataServer(t)
	body := do("GET", "/data", nil)
	for _, want := range []string{
		"Where your exports land", // point it at the POS folder
		"Import a file now",       // upload one directly
		"Enter a sale by hand",    // type one in
		"What has arrived",        // see that it worked
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the Data page is missing the %q section", want)
		}
	}
}

func TestAddingAWatchFolderStartsWatchingIt(t *testing.T) {
	srv, ing, do := dataServer(t)
	dir := t.TempDir()

	body := do("POST", "/data", url.Values{
		"action": {"add_folder"}, "path": {dir}, "label": {"POS exports"},
	})
	if !strings.Contains(body, "Now watching") {
		t.Errorf("adding a folder did not confirm it:\n%s", body)
	}
	if len(ing.folders) != 1 || !strings.EqualFold(ing.folders[0], filepath.Clean(dir)) {
		t.Errorf("watcher folders = %v, want %q", ing.folders, dir)
	}

	folders, err := srv.store.WatchFolders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) != 1 || folders[0].Label != "POS exports" {
		t.Errorf("stored folders = %+v, want the one just added", folders)
	}
}

// A folder that cannot be used is refused with a reason the owner can
// act on, rather than accepted and then silently watched for nothing.
func TestAddingABadFolderExplainsWhy(t *testing.T) {
	_, ing, do := dataServer(t)

	body := do("POST", "/data", url.Values{
		"action": {"add_folder"}, "path": {filepath.Join(t.TempDir(), "not-there")},
	})
	if !strings.Contains(body, "no folder at") {
		t.Errorf("expected an explanation of what is wrong:\n%s", body)
	}
	if len(ing.folders) != 0 {
		t.Errorf("a folder that does not exist was watched anyway: %v", ing.folders)
	}
}

// A folder that is configured but unreachable right now must be shown as
// such. Reading "Watching" against a dead network share is how an owner
// ends up believing sales are coming in when they are not.
func TestUnreachableFolderIsShownAsAProblem(t *testing.T) {
	srv, ing, do := dataServer(t)
	dir := t.TempDir()
	do("POST", "/data", url.Values{"action": {"add_folder"}, "path": {dir}})

	ing.unusable = filepath.Clean(dir)
	srv.ApplyWatchFolders(context.Background())

	body := do("GET", "/data", nil)
	if !strings.Contains(body, "Not reachable") {
		t.Errorf("an unreachable folder is not flagged on the page:\n%s", body)
	}
}

func TestManualSaleGoesThroughTheNormalPipeline(t *testing.T) {
	_, ing, do := dataServer(t)
	saleDate, saleTime := pastSale()

	body := do("POST", "/data", url.Values{
		"action": {"manual_sale"}, "date": {saleDate},
		"time": {saleTime}, "product": {"DIESEL"}, "liters": {"20"},
		"unit_price": {"275.50"}, "payment_method": {"CASH"},
	})
	if !strings.Contains(body, "Sale recorded") {
		t.Errorf("a valid sale was not recorded:\n%s", body)
	}
	if len(ing.rows) != 1 {
		t.Fatalf("rows handed to the pipeline = %d, want 1", len(ing.rows))
	}
	// It must go in as a normal raw transaction, not by some side door.
	if ing.rows[0].PosSourceID != posadapter.ManualSourceID {
		t.Errorf("PosSourceID = %q, want %q", ing.rows[0].PosSourceID, posadapter.ManualSourceID)
	}
	if ing.rows[0].PayloadKind != posadapter.PayloadJSON {
		t.Errorf("PayloadKind = %q, want json", ing.rows[0].PayloadKind)
	}
}

func TestManualSaleRefusesWhatCannotBeCounted(t *testing.T) {
	_, ing, do := dataServer(t)
	saleDate, saleTime := pastSale()

	cases := []struct {
		name   string
		form   url.Values
		wantIn string
	}{
		{
			"no litres",
			url.Values{"action": {"manual_sale"}, "date": {saleDate}, "time": {saleTime},
				"product": {"DIESEL"}, "liters": {""}, "unit_price": {"275.50"}},
			"how many litres",
		},
		{
			"credit with no customer",
			url.Values{"action": {"manual_sale"}, "date": {saleDate}, "time": {saleTime},
				"product": {"DIESEL"}, "liters": {"20"}, "unit_price": {"275.50"},
				"payment_method": {"CREDIT"}},
			"phone number",
		},
		{
			"unreadable date",
			url.Values{"action": {"manual_sale"}, "date": {"not-a-date"}, "time": {saleTime},
				"product": {"DIESEL"}, "liters": {"20"}, "unit_price": {"275.50"}},
			"valid date",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := do("POST", "/data", c.form)
			if !strings.Contains(body, c.wantIn) {
				t.Errorf("expected an explanation containing %q:\n%s", c.wantIn, body)
			}
		})
	}
	if len(ing.rows) != 0 {
		t.Errorf("%d bad sales reached the pipeline, want none", len(ing.rows))
	}
}

// Numbers typed the way people type them, with separators and spaces.
func TestManualSaleAcceptsNumbersAsTyped(t *testing.T) {
	_, ing, do := dataServer(t)
	saleDate, saleTime := pastSale()
	body := do("POST", "/data", url.Values{
		"action": {"manual_sale"}, "date": {saleDate},
		"time": {saleTime}, "product": {"DIESEL"}, "liters": {" 1,250.5 "},
		"unit_price": {"275.50"}, "total": {"344,512.75"}, "payment_method": {"CASH"},
	})
	if !strings.Contains(body, "Sale recorded") {
		t.Errorf("a sale with thousands separators was refused:\n%s", body)
	}
	if len(ing.rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(ing.rows))
	}
	payload := string(ing.rows[0].Payload)
	if !strings.Contains(payload, `"quantity_liters":"1250.500"`) {
		t.Errorf("litres were not read as 1250.5: %s", payload)
	}
	if !strings.Contains(payload, `"total_amount":"344512.75"`) {
		t.Errorf("total was not read as 344512.75: %s", payload)
	}
}

// With no watcher attached, the page says so rather than offering
// buttons that quietly do nothing.
func TestDataPageWithoutAnIngesterSaysSo(t *testing.T) {
	srv, do := loggedInServer(t)
	srv.Ingest = nil
	body := readBody(t, do("GET", "/data", nil).Result())
	if !strings.Contains(body, "cannot accept data from the dashboard") {
		t.Errorf("the page should say it cannot take data:\n%s", body)
	}
}

// The page has to require a PIN like every other page: it can add a
// watch folder anywhere on the disk and put sales into the books.
func TestDataPageNeedsAPIN(t *testing.T) {
	srv, _, _ := newTestServer(t)
	r := httptest.NewRequest("GET", "/data", nil)
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Errorf("unauthenticated /data = %d, want a redirect to login", w.Code)
	}
}

// A customer page reached with the number in any format must show that
// customer's balance. Matching the raw query value against the mart's
// canonical rows found their sales and payments but reported a balance
// of zero, which is worse than not finding them at all.
func TestCustomerPageFindsTheBalanceInAnyNumberFormat(t *testing.T) {
	srv, _, do := dataServer(t)
	ctx := context.Background()

	if _, err := srv.store.DB().ExecContext(ctx, `INSERT INTO credit_outstanding
		(customer_phone, as_of_date, outstanding_amount, transaction_count, days_overdue)
		VALUES ('+923001234567', '2026-09-13', 42837.48, 19, 2)`); err != nil {
		t.Fatal(err)
	}

	for _, typed := range []string{"%2B923001234567", "03001234567", "0300-1234567", "923001234567"} {
		body := do("GET", "/customer?phone="+typed, nil)
		if !strings.Contains(body, "42,837") {
			t.Errorf("customer page for %q did not show the balance:\n%s", typed, firstLines(body, 40))
		}
	}
}

// firstLines trims a page down to something readable in a failure.
func firstLines(s string, n int) string {
	parts := strings.SplitN(s, "\n", n+1)
	if len(parts) > n {
		parts = parts[:n]
	}
	return strings.Join(parts, "\n")
}

// The Margin page must never set a month of revenue against a few days
// of cost. Doing so reports a margin percentage that is wrong, and wrong
// in the direction that flatters revenue — the worst way to be wrong
// about profit.
func TestMarginTotalsOnlyCoverCostedDays(t *testing.T) {
	srv, _, do := dataServer(t)
	ctx := context.Background()

	// Two days of sales; only the second has a purchase price.
	for _, row := range []struct {
		date            string
		revenue, cost   float64
		margin, marginP float64
	}{
		{"2026-09-12", 300000, 0, 0, 0},
		{"2026-09-13", 100000, 80000, 20000, 20},
	} {
		if _, err := srv.store.DB().ExecContext(ctx, `INSERT INTO fuel_margin
			(date, product_code, revenue, cost_of_goods, margin_amount, margin_pct)
			VALUES (?, 'DIESEL', ?, ?, ?, ?)`,
			row.date, row.revenue, row.cost, row.margin, row.marginP); err != nil {
			t.Fatal(err)
		}
	}

	body := do("GET", "/margin", nil)

	// 20,000 of 100,000 is 20.0%. Against both days' revenue it would
	// read 5.0%, which is the bug.
	if !strings.Contains(body, "20.0%") {
		t.Errorf("margin %% is not the costed days' own figure:\n%s", marginCards(body))
	}
	if strings.Contains(body, "5.0%") {
		t.Errorf("a month of revenue was set against one day of cost:\n%s", marginCards(body))
	}
	// The revenue headline must be the costed days' revenue, not the sum.
	if !strings.Contains(body, "PKR 100,000") {
		t.Errorf("revenue headline is not the costed days' revenue:\n%s", marginCards(body))
	}
	// The revenue that was left out still has to be visible, or the page
	// quietly hides a day's takings.
	if !strings.Contains(body, "PKR 300,000") {
		t.Errorf("the uncosted day's revenue was not accounted for:\n%s", marginCards(body))
	}
}

// marginCards trims the page to the totals block for a readable failure.
func marginCards(body string) string {
	start := strings.Index(body, `aria-label="Totals"`)
	if start < 0 {
		return firstLines(body, 30)
	}
	end := start + 1200
	if end > len(body) {
		end = len(body)
	}
	return body[start:end]
}
