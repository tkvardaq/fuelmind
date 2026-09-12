package web

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

	"github.com/fuelmind/fuelmind/internal/auth"
	"github.com/fuelmind/fuelmind/internal/storage"
)

func newTestServer(t *testing.T) (*Server, *auth.Auth, *storage.Storage) {
	t.Helper()
	dir := t.TempDir()
	s, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	a := auth.New(s)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv, err := New(s, a, logger, 0) // port 0 = don't actually listen
	if err != nil {
		t.Fatal(err)
	}
	return srv, a, s
}

func TestHealthz(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/healthz", nil)
	srv.Routes().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

func TestUnauthenticatedRootRedirectsToLogin(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	srv.Routes().ServeHTTP(w, r)
	if w.Code != http.StatusFound {
		t.Errorf("status = %d, want 302", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/login" {
		t.Errorf("Location = %q, want /login", loc)
	}
}

func TestSetupPageRenders(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/setup", nil)
	r.RemoteAddr = "127.0.0.1:50000"
	srv.Routes().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Set a PIN") {
		t.Error("setup page missing 'Set a PIN' heading")
	}
}

func TestLoginPageRedirectsToSetupWhenNoPIN(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/login", nil)
	srv.Routes().ServeHTTP(w, r)
	if w.Code != http.StatusFound {
		t.Errorf("status = %d, want 302 (no PIN set -> /setup)", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/setup" {
		t.Errorf("Location = %q, want /setup", loc)
	}
}

func TestFullFlow_SetupLoginDashboard(t *testing.T) {
	srv, _, _ := newTestServer(t)
	form := url.Values{}
	form.Set("pin", "12345678")
	form.Set("pin_confirm", "12345678")

	// 1. POST /setup with matching PINs.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/setup", strings.NewReader(form.Encode()))
	r.RemoteAddr = "127.0.0.1:50000"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	srv.Routes().ServeHTTP(w, r)
	if w.Code != http.StatusFound {
		t.Fatalf("setup POST: status = %d, want 302; body = %s", w.Code, w.Body.String())
	}
	// Cookie should be set.
	var setupCookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == SessionCookieName {
			setupCookie = c
		}
	}
	if setupCookie == nil {
		t.Fatal("setup did not set a session cookie")
	}

	// 2. With the cookie, / should render the dashboard.
	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest("GET", "/", nil)
	r2.AddCookie(setupCookie)
	srv.Routes().ServeHTTP(w2, r2)
	if w2.Code != 200 {
		t.Fatalf("dashboard: status = %d, want 200; body = %s", w2.Code, w2.Body.String())
	}
	if !strings.Contains(w2.Body.String(), "FuelMind Score") {
		t.Error("dashboard missing FuelMind Score section")
	}

	// 3. Logout clears the session.
	w3 := httptest.NewRecorder()
	r3 := httptest.NewRequest("POST", "/logout", nil)
	r3.AddCookie(setupCookie)
	srv.Routes().ServeHTTP(w3, r3)
	if w3.Code != http.StatusFound {
		t.Errorf("logout: status = %d, want 302", w3.Code)
	}
}

func TestLoginWithWrongPIN(t *testing.T) {
	srv, a, _ := newTestServer(t)
	_ = a.SetupPIN(context.Background(), "12345678")

	form := url.Values{}
	form.Set("pin", "99999999")
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	srv.Routes().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Errorf("status = %d, want 200 (login re-renders with error)", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Invalid credentials") {
		t.Error("login error not shown")
	}
}

func TestSalesPageRendersEmpty(t *testing.T) {
	srv, a, _ := newTestServer(t)
	_ = a.SetupPIN(context.Background(), "12345678")
	sid, _ := a.Login(context.Background(), "12345678", "")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/sales", nil)
	r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sid})
	srv.Routes().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "No sales in the last 30 days yet") {
		t.Error("empty sales page missing its empty-state message")
	}
}

func TestStaticCSS(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/static/app.css", nil)
	srv.Routes().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/css") {
		t.Errorf("Content-Type = %q, want text/css", w.Header().Get("Content-Type"))
	}
	if len(w.Body.Bytes()) < 100 {
		t.Error("CSS body suspiciously short")
	}
}

func TestSecurityHeaders(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/healthz", nil)
	srv.Routes().ServeHTTP(w, r)
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("expected X-Content-Type-Options: nosniff, got %q", w.Header().Get("X-Content-Type-Options"))
	}
	if w.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("expected X-Frame-Options: DENY, got %q", w.Header().Get("X-Frame-Options"))
	}
	if w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("expected Referrer-Policy: no-referrer, got %q", w.Header().Get("Referrer-Policy"))
	}
}

func TestLogoutRequiresPOST(t *testing.T) {
	srv, a, _ := newTestServer(t)
	_ = a.SetupPIN(context.Background(), "12345678")
	sid, _ := a.Login(context.Background(), "12345678", "")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/logout", nil)
	r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sid})
	srv.Routes().ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /logout status = %d, want 405 Method Not Allowed", w.Code)
	}
}

func TestSetupBypassBlockedWhenPINSet(t *testing.T) {
	srv, a, _ := newTestServer(t)
	_ = a.SetupPIN(context.Background(), "12345678")

	form := url.Values{}
	form.Set("pin", "87654321")
	form.Set("pin_confirm", "87654321")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/setup", strings.NewReader(form.Encode()))
	r.RemoteAddr = "127.0.0.1:50000"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	srv.Routes().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("POST /setup when PIN already set: status = %d, want 403 Forbidden", w.Code)
	}
}
