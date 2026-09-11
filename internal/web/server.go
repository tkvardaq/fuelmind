// Package web serves the local dashboard (Phase 4). It exposes a
// small HTTP API on localhost:<Port> that renders HTML pages from
// templates and serves HTMX-style partial updates.
//
// Per spec §2.1: "Local web UI: Served from the local core on
// localhost:PORT, accessed via browser on the shop PC or any
// device on the station's LAN."
//
// Per spec §9: dashboard requires a PIN — we never rely on
// "it's on the LAN" as access control.
package web

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/fuelmind/fuelmind/internal/auth"
	"github.com/fuelmind/fuelmind/internal/storage"
)

//go:embed templates/*
var templatesFS embed.FS

// Server is the HTTP server. Holds the dependencies the handlers
// need and a parsed template set per page.
type Server struct {
	store   *storage.Storage
	auth    *auth.Auth
	logger  *slog.Logger
	port    int
	pages   map[string]*template.Template
	started time.Time
}

// New builds a server. Templates are parsed at startup so any
// template error is caught early.
//
// We parse base.html + each page into a separate template set so
// each page's "body" define doesn't collide with another's.
// Each page file defines a "body" block and invokes
// {{template "base" .}}; the page-named template executes that
// top-level {{template}} which renders the chrome around body.
func New(store *storage.Storage, a *auth.Auth, logger *slog.Logger, port int) (*Server, error) {
	if logger == nil {
		logger = slog.Default()
	}
	pages := map[string]*template.Template{}
	pageFiles, err := fs.Glob(templatesFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	for _, f := range pageFiles {
		if f == "templates/base.html" {
			continue
		}
		pageName := strings.TrimSuffix(filepath.Base(f), ".html")
		t := template.New(pageName).Funcs(funcMap())
		if _, err := t.ParseFS(templatesFS, "templates/base.html", f); err != nil {
			return nil, fmt.Errorf("web: parse %s: %w", f, err)
		}
		pages[pageName] = t
	}
	return &Server{
		store:   store,
		auth:    a,
		logger:  logger,
		port:    port,
		pages:   pages,
		started: time.Now(),
	}, nil
}

// ListenAndServe blocks, serving on localhost:<port>. Returns when
// ctx is done.
func (s *Server) ListenAndServe(ctx context.Context) error {
	mux := s.Routes()
	srv := &http.Server{
		Addr:              "127.0.0.1:" + strconv.Itoa(s.port),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	s.logger.Info("dashboard listening", "url", srv.Addr)

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// Routes wires up the URL space. Public routes: /login, /setup.
// Protected (require a session cookie): /, /sales, /credit, /score,
// /logout.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	// Public.
	mux.HandleFunc("/login", s.handleLogin)
	mux.HandleFunc("/setup", s.handleSetup)
	mux.HandleFunc("/healthz", s.handleHealthz)

	// Protected.
	mux.Handle("/", s.requireAuth(http.HandlerFunc(s.handleDashboard)))
	mux.Handle("/sales", s.requireAuth(http.HandlerFunc(s.handleSales)))
	mux.Handle("/credit", s.requireAuth(http.HandlerFunc(s.handleCredit)))
	mux.Handle("/score", s.requireAuth(http.HandlerFunc(s.handleScore)))
	mux.Handle("/logout", s.requireAuth(http.HandlerFunc(s.handleLogout)))

	// Static assets (CSS, no JS dependency for v1).
	mux.HandleFunc("/static/", s.handleStatic)

	return s.securityHeaders(s.requestLogger(mux))
}

// securityHeaders sets standard defensive HTTP security headers.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-XSS-Protection", "1; mode=block")
		next.ServeHTTP(w, r)
	})
}

// requireAuth wraps a handler so it returns 302 → /login if no
// valid session cookie is present.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sid := readSessionCookie(r)
		if sid == "" {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		uid, err := s.auth.Verify(r.Context(), sid)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		// Stash the user_id on the request context for handlers.
		ctx := context.WithValue(r.Context(), ctxUserID{}, uid)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// readSessionCookie returns the session id, or "".
func readSessionCookie(r *http.Request) string {
	c, err := r.Cookie(SessionCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

const SessionCookieName = "fuelmind_session"

// ctxUserID is the context key for the authenticated user id.
type ctxUserID struct{}

// requestLogger logs every request at debug level. Cheap; can stay
// on permanently without flooding the log.
func (s *Server) requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rw, r)
		s.logger.Debug("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"remote", clientIP(r),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(c int) {
	r.status = c
	r.ResponseWriter.WriteHeader(c)
}

func clientIP(r *http.Request) string {
	// Best-effort; the dashboard is on localhost so this is mostly
	// 127.0.0.1 in practice. Strip any port.
	addr := r.RemoteAddr
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		addr = addr[:i]
	}
	return addr
}

// funcMap is the set of helpers exposed to templates.
func funcMap() template.FuncMap {
	return template.FuncMap{
		"formatMoney": formatMoney,
		"formatLiters": formatLiters,
		"formatDate":  formatDate,
		"severityClass": severityClass,
	}
}

func formatMoney(n float64) string {
	// PKR for now; v1.1 will read the station's currency from config.
	return "PKR " + formatNum(n, 0)
}

func formatLiters(n float64) string { return formatNum(n, 2) + " L" }

func formatNum(n float64, decimals int) string {
	// Locale-light number formatting: thousands with comma, decimals fixed.
	// Uses math.Round for correct round-half-away-from-zero semantics
	// instead of the int64(x+0.5) trick which can misfire on floating
	// point values (e.g. 0.285 * 100 may be slightly below 28.5).
	if n < 0 {
		return "-" + formatNum(-n, decimals)
	}
	pow := math.Pow10(decimals)
	scaled := n * pow
	rounded := math.Round(scaled)
	whole := int64(rounded / pow)
	frac := rounded - float64(whole)*pow
	wstr := strconv.FormatInt(whole, 10)
	var b strings.Builder
	for i, c := range wstr {
		if i > 0 && (len(wstr)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if decimals > 0 {
		b.WriteByte('.')
		fstr := strconv.FormatInt(int64(frac), 10)
		for len(fstr) < decimals {
			fstr = "0" + fstr
		}
		b.WriteString(fstr)
	}
	return b.String()
}

func formatDate(t string) string {
	// t is "YYYY-MM-DD" from SQLite; pass through.
	return t
}

func severityClass(s string) string {
	switch s {
	case "alert":
		return "alert"
	case "warn":
		return "warn"
	default:
		return "info"
	}
}
