package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fuelmind/fuelmind/internal/auth"
	"github.com/fuelmind/fuelmind/internal/backup"
	"github.com/fuelmind/fuelmind/internal/mart"
	"github.com/fuelmind/fuelmind/internal/normalizer"
	"github.com/fuelmind/fuelmind/internal/posadapter/csvwatch"
	"github.com/fuelmind/fuelmind/internal/storage"
	"github.com/fuelmind/fuelmind/internal/web"
)

func TestFullSystemE2E(t *testing.T) {
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "fuelmind.db")
	posDrop := filepath.Join(dataDir, "pos_drop")
	_ = os.MkdirAll(posDrop, 0755)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// 1. Storage & Migrations
	store, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer store.Close()

	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}

	// 2. Normalizer & Mart
	norm, err := normalizer.New(store, logger)
	if err != nil {
		t.Fatalf("normalizer.New: %v", err)
	}
	m := mart.New(store, logger)

	// 3. POS Watcher
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

	// 4. Web Dashboard & Auth
	a := auth.New(store)
	srv, err := web.New(store, a, logger, 0)
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}

	handler := srv.Routes()

	// Step A: /healthz & Security Headers
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/healthz", nil)
	handler.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("/healthz code = %d, want 200", w.Code)
	}
	if w.Header().Get("X-Frame-Options") != "DENY" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("missing security headers on healthz: %v", w.Header())
	}

	// Step B: Setup first-time PIN
	form := url.Values{}
	form.Set("pin", "strongpin123")
	form.Set("pin_confirm", "strongpin123")
	w = httptest.NewRecorder()
	r = httptest.NewRequest("POST", "/setup", strings.NewReader(form.Encode()))
	r.RemoteAddr = "127.0.0.1:50000"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusFound {
		t.Fatalf("setup POST code = %d, want 302", w.Code)
	}

	var sessionCookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == web.SessionCookieName {
			sessionCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatal("expected session cookie from setup POST")
	}

	// Step C: Setup bypass attempt blocked
	w = httptest.NewRecorder()
	r = httptest.NewRequest("POST", "/setup", strings.NewReader(form.Encode()))
	r.RemoteAddr = "127.0.0.1:50000"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("setup bypass code = %d, want 403 Forbidden", w.Code)
	}

	// Step D: Drop a sample POS export
	sampleCSV := "pos_source_id,external_id,occurred_at,product_alias,quantity_liters,unit_price,total_amount,payment_method\n" +
		"lane_1,TX-1001," + time.Now().Format("2006-01-02T15:04:05") + ",DIESEL,50.00,270.00,13500.00,CASH\n" +
		"lane_1,TX-1002," + time.Now().Format("2006-01-02T15:04:05") + ",PETROL_92,20.00,260.00,5200.00,CREDIT\n"

	csvPath := filepath.Join(posDrop, "daily_tx.csv")
	if err := os.WriteFile(csvPath, []byte(sampleCSV), 0644); err != nil {
		t.Fatal(err)
	}

	// Allow watcher to process
	time.Sleep(300 * time.Millisecond)

	processedPath := filepath.Join(posDrop, "processed", "daily_tx.csv")
	if _, err := os.Stat(processedPath); err != nil {
		t.Fatalf("expected file to be moved to processed/, err: %v", err)
	}

	// Step E: Request Dashboard and check data
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/", nil)
	r.AddCookie(sessionCookie)
	handler.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("dashboard GET code = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "Revenue") || !strings.Contains(body, "PKR") {
		t.Errorf("dashboard missing revenue card, got: %s", body)
	}
	if !strings.Contains(body, "FuelMind Score") {
		t.Errorf("dashboard missing FuelMind Score section, got: %s", body)
	}

	// Step F: Backup Agent Verification
	backupAgent := backup.NewAgent(backup.Config{DataDir: dataDir, Logger: logger})
	if err := backupAgent.PerformBackup(context.Background()); err != nil {
		t.Fatalf("PerformBackup: %v", err)
	}
	backups, err := os.ReadDir(filepath.Join(dataDir, "backups"))
	if err != nil || len(backups) == 0 {
		t.Fatalf("expected backup database file in backups/, got err=%v count=%d", err, len(backups))
	}
}
