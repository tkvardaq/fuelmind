package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fuelmind/fuelmind/internal/ask"
	"github.com/fuelmind/fuelmind/internal/auth"
	"github.com/fuelmind/fuelmind/internal/backup"
	"github.com/fuelmind/fuelmind/internal/llm"
	"github.com/fuelmind/fuelmind/internal/mart"
	"github.com/fuelmind/fuelmind/internal/normalizer"
	"github.com/fuelmind/fuelmind/internal/posadapter/csvwatch"
	"github.com/fuelmind/fuelmind/internal/storage"
	"github.com/fuelmind/fuelmind/internal/web"
)

const csvHeader = "pos_source_id,external_id,occurred_at,product_alias,quantity_liters," +
	"unit_price,total_amount,payment_method,customer_phone,pump_id,attendant\n"

// TestFullSystemE2E wires the real components together the way main.go
// does and drives one day of a station's life through them.
func TestFullSystemE2E(t *testing.T) {
	dataDir := t.TempDir()
	posDrop := filepath.Join(dataDir, "pos_drop")
	if err := os.MkdirAll(posDrop, 0o755); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	store, err := storage.Open(filepath.Join(dataDir, "fuelmind.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer store.Close()
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	norm, err := normalizer.New(store, logger)
	if err != nil {
		t.Fatalf("normalizer.New: %v", err)
	}
	m := mart.New(store, logger)
	watcher, err := csvwatch.NewWatcher(store, norm, m, logger)
	if err != nil {
		t.Fatalf("csvwatch.NewWatcher: %v", err)
	}
	defer watcher.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := watcher.Start(ctx, posDrop); err != nil {
		t.Fatalf("watcher.Start: %v", err)
	}

	a := auth.New(store)
	srv, err := web.New(store, a, logger, 0)
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	srv.Version = "e2e-test"
	srv.Answerer = ask.New(store, llm.NewRouter(nil, llm.TierBasic))
	handler := srv.Routes()

	call := func(method, path string, form url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
		var r *http.Request
		if form != nil {
			r = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		} else {
			r = httptest.NewRequest(method, path, nil)
		}
		r.RemoteAddr = "127.0.0.1:50000"
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}

	// 1. Health and security headers.
	w := call("GET", "/healthz", nil, nil)
	if w.Code != 200 {
		t.Fatalf("/healthz = %d", w.Code)
	}
	if w.Header().Get("X-Frame-Options") != "DENY" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("missing security headers: %v", w.Header())
	}

	// 2. First-run PIN from the shop PC, then the bypass is closed.
	form := url.Values{"pin": {"strongpin123"}, "pin_confirm": {"strongpin123"}}
	w = call("POST", "/setup", form, nil)
	if w.Code != http.StatusFound {
		t.Fatalf("setup POST = %d, want 302; body: %s", w.Code, w.Body.String())
	}
	var session *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == web.SessionCookieName {
			session = c
		}
	}
	if session == nil {
		t.Fatal("no session cookie after setup")
	}
	if w := call("POST", "/setup", form, nil); w.Code != http.StatusForbidden {
		t.Fatalf("second setup POST = %d, want 403", w.Code)
	}

	// 3. A day of POS exports: two normal sales, one unknown product, and
	//    a re-export of the first sale with the attendant filled in.
	today := time.Now().Format("2006-01-02")
	drop := func(name, body string) {
		if err := os.WriteFile(filepath.Join(posDrop, name), []byte(csvHeader+body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	drop("morning.csv", fmt.Sprintf(
		"lane_1,TX-1,%sT08:10:00,HSD,40.000,275.50,\"11,020.00\",CASH,,P1,\n"+
			"lane_1,TX-2,%sT09:15:00,PMG-92,30.000,250.30,7509.00,CREDIT,+923001112223,P2,Bilal\n"+
			"lane_2,TX-3,%sT10:00:00,Super Diesel,5.000,280.00,1400.00,CASH,,P3,Ali\n", today, today, today))
	waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(posDrop, "processed", "morning.csv"))
		return err == nil
	})
	drop("corrected.csv", fmt.Sprintf("lane_1,TX-1,%sT08:10:00,HSD,40.000,275.50,11020.00,CASH,,P1,Ali\n", today))
	waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(posDrop, "processed", "corrected.csv"))
		return err == nil
	})

	// The re-export replaces the original instead of double counting.
	var txCount int
	var revenue float64
	if err := store.DB().QueryRow(`SELECT COUNT(*), COALESCE(SUM(total_amount),0) FROM transactions`).Scan(&txCount, &revenue); err != nil {
		t.Fatal(err)
	}
	if txCount != 2 || revenue != 18529 {
		t.Errorf("transactions=%d revenue=%.2f, want 2 and 18529.00", txCount, revenue)
	}
	var attendant string
	_ = store.DB().QueryRow(`SELECT COALESCE(attendant,'') FROM transactions WHERE external_id='TX-1'`).Scan(&attendant)
	if attendant != "Ali" {
		t.Errorf("correction not applied: attendant = %q", attendant)
	}

	// 4. Today's page shows the money, the score and the unknown product.
	body := call("GET", "/", nil, session).Body.String()
	for _, want := range []string{"PKR 18,529", "70.00 L", "FuelMind Score", "Super Diesel", "e2e-test"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}

	// 5. Credit page lists the credit customer.
	if body := call("GET", "/credit", nil, session).Body.String(); !strings.Contains(body, "923001112223") {
		t.Errorf("credit page missing the customer: %s", body)
	}

	// 6. The Ask box answers from the mart without an LLM.
	answer := call("POST", "/ask", url.Values{"q": {"how much did we sell today?"}}, session).Body.String()
	if !strings.Contains(answer, "PKR 18,529.00") {
		t.Errorf("ask did not answer from the mart: %s", answer)
	}

	// 7. A backup is a usable copy of the live database.
	agent := backup.NewAgent(backup.Config{DataDir: dataDir, Store: store, Logger: logger})
	if err := agent.PerformBackup(context.Background()); err != nil {
		t.Fatalf("PerformBackup: %v", err)
	}
	backups, err := os.ReadDir(filepath.Join(dataDir, "backups"))
	if err != nil || len(backups) == 0 {
		t.Fatalf("no backup written: err=%v count=%d", err, len(backups))
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", timeout)
}
