package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/fuelmind/fuelmind/internal/ask"
	"github.com/fuelmind/fuelmind/internal/llm"
)

func loggedInServer(t *testing.T) (*Server, func(method, path string, form url.Values) *httptest.ResponseRecorder) {
	t.Helper()
	srv, a, _ := newTestServer(t)
	if err := a.SetupPIN(context.Background(), "12345678"); err != nil {
		t.Fatal(err)
	}
	sid, err := a.Login(context.Background(), "12345678", "")
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, path string, form url.Values) *httptest.ResponseRecorder {
		var r *http.Request
		if form != nil {
			r = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		} else {
			r = httptest.NewRequest(method, path, nil)
		}
		r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sid})
		w := httptest.NewRecorder()
		srv.Routes().ServeHTTP(w, r)
		return w
	}
	return srv, do
}

func TestCreditPageWithData(t *testing.T) {
	srv, do := loggedInServer(t)
	if _, err := srv.store.DB().Exec(`INSERT INTO credit_outstanding
		(customer_phone, as_of_date, outstanding_amount, transaction_count, days_overdue)
		VALUES ('+923001234567', '2026-09-11', 12394.10, 2, 3)`); err != nil {
		t.Fatal(err)
	}
	w := do("GET", "/credit", nil)
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "923001234567") || !strings.Contains(body, "PKR 12,394") || !strings.Contains(body, "</html>") {
		t.Fatalf("credit page broken: status=%d body=%s", w.Code, body)
	}
}

func TestUnknownPathIs404(t *testing.T) {
	_, do := loggedInServer(t)
	if w := do("GET", "/does-not-exist", nil); w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestRemoteCannotClaimFirstRunSetup(t *testing.T) {
	srv, _, _ := newTestServer(t)
	form := url.Values{"pin": {"attacker123"}, "pin_confirm": {"attacker123"}}
	r := httptest.NewRequest("POST", "/setup", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.RemoteAddr = "192.168.1.50:40000"
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("remote first-run setup status = %d, want 403", w.Code)
	}
	if set, _ := srv.auth.IsPINSet(context.Background()); set {
		t.Error("a LAN client was able to set the first PIN")
	}
}

func TestLoginLockoutMessage(t *testing.T) {
	srv, a, _ := newTestServer(t)
	_ = a.SetupPIN(context.Background(), "12345678")
	var last *httptest.ResponseRecorder
	for i := 0; i < 5; i++ {
		form := url.Values{"pin": {"wrongpin0"}}
		r := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		last = httptest.NewRecorder()
		srv.Routes().ServeHTTP(last, r)
	}
	if last.Code != http.StatusTooManyRequests || !strings.Contains(last.Body.String(), "locked until") {
		t.Errorf("5th wrong PIN: status=%d, want 429 with a lockout message", last.Code)
	}
}

func TestStaleScoreIsLabelled(t *testing.T) {
	srv, do := loggedInServer(t)
	_, _ = srv.store.DB().Exec(`INSERT INTO station_health_score
		(date, sales_score, inventory_score, cash_score, credit_score, data_quality_score, operations_score, overall_score, issues_json)
		VALUES ('2020-01-01', 50, 100, 100, 100, 100, 100, 79, '[]')`)
	body := do("GET", "/", nil).Body.String()
	if !strings.Contains(body, "latest: Wed 1 Jan 2020") {
		t.Errorf("stale score shown without its date")
	}
}

func TestAskUsesRouter(t *testing.T) {
	srv, do := loggedInServer(t)
	srv.Answerer = ask.New(srv.store, llm.NewRouter(nil, llm.TierBasic))
	w := do("POST", "/ask", url.Values{"q": {"how much did we sell today?"}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Today&#39;s revenue is PKR 0.00") {
		t.Errorf("ask: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestVersionInFooter(t *testing.T) {
	srv, do := loggedInServer(t)
	srv.Version = "9.9.9-test"
	if !strings.Contains(do("GET", "/", nil).Body.String(), "9.9.9-test") {
		t.Error("footer does not show the build version")
	}
}

func TestFormatNum(t *testing.T) {
	cases := map[float64]string{0: "0.00", -0.001: "0.00", 1234567.891: "1,234,567.89", -1500.5: "-1,500.50", 0.995: "1.00"}
	for in, want := range cases {
		if got := formatNum(in, 2); got != want {
			t.Errorf("formatNum(%v) = %q, want %q", in, got, want)
		}
	}
}
