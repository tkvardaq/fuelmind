// Package web serves the local dashboard. It renders HTML pages from
// embedded templates on <bind>:<port> (all interfaces by default, so the
// owner can open it from a phone on the station LAN).
//
// Per spec §9 the dashboard always requires a PIN; the very first PIN can
// only be set from the shop PC itself (loopback), so nobody on the LAN
// can claim a fresh install.
package web

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/fuelmind/fuelmind/internal/ask"
	"github.com/fuelmind/fuelmind/internal/auth"
	"github.com/fuelmind/fuelmind/internal/format"
	"github.com/fuelmind/fuelmind/internal/storage"
)

//go:embed templates/*
var templatesFS embed.FS

// Server is the HTTP server.
type Server struct {
	store  *storage.Storage
	auth   *auth.Auth
	logger *slog.Logger
	port   int
	pages  map[string]*template.Template

	// Bind is the listen address (default "0.0.0.0", all interfaces).
	Bind string
	// Version is shown in the footer.
	Version string
	// Answerer replies to questions from the Ask box. Nil disables it.
	Answerer *ask.Service
	// Mart recomputes figures right after the owner enters data.
	Mart Materializer
	// CloudConfigured tells the Settings page whether remote questions
	// can work at all.
	CloudConfigured bool
	// Messages composes and sends the station's own messages. Nil means
	// this build cannot send anything.
	Messages Messenger
	// Pairing links the owner's phone to the station. Nil when no
	// pairing-based channel is configured.
	Pairing Pairer
	// Ingest accepts data from the Data page: an uploaded export, a sale
	// typed in by hand, and changes to which folders are watched. Nil
	// means this build cannot take data from the dashboard.
	Ingest Ingester
	// Model is the answerer's model settings, so changing the provider in
	// Settings takes effect on the next question instead of needing a
	// restart. Nil disables the model section of the Settings page.
	Model ModelConfigurator
}

// New builds a server. Templates are parsed at startup so any template
// error is caught early.
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
		Bind:    "0.0.0.0",
		Version: "dev",
	}, nil
}

// ListenAndServe blocks until ctx is done or the listener fails.
func (s *Server) ListenAndServe(ctx context.Context) error {
	srv := &http.Server{
		Addr:              net.JoinHostPort(s.Bind, strconv.Itoa(s.port)),
		Handler:           s.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second, // the Ask box may wait on a local LLM
		IdleTimeout:       60 * time.Second,
	}
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return err
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	s.logger.Info("dashboard listening", "addr", ln.Addr().String())

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// Routes wires up the URL space.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	// Public.
	mux.HandleFunc("/login", s.handleLogin)
	// Anyone who can reach the login page can ask for a new PIN. The
	// request grants nothing; only a signed-in owner can act on it.
	mux.HandleFunc("/forgot", s.handleForgotPIN)
	mux.HandleFunc("/setup", s.handleSetup)
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/static/", s.handleStatic)
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/static/favicon.svg", http.StatusMovedPermanently)
	})

	// Protected.
	mux.Handle("/", s.requireAuth(http.HandlerFunc(s.handleDashboard)))
	mux.Handle("/sales", s.requireAuth(http.HandlerFunc(s.handleSales)))
	mux.Handle("/credit", s.requireAuth(http.HandlerFunc(s.handleCredit)))
	mux.Handle("/score", s.requireAuth(http.HandlerFunc(s.handleScore)))
	mux.Handle("/ask", s.requireAuth(http.HandlerFunc(s.handleAsk)))
	mux.Handle("/margin", s.requireAuth(http.HandlerFunc(s.handleMargin)))
	mux.Handle("/customer", s.requireAuth(http.HandlerFunc(s.handleCustomer)))
	// Recording what happened on a shift is staff work; changing how the
	// station is set up is the owner's.
	mux.Handle("/data", s.requireAuth(http.HandlerFunc(s.handleData)))
	mux.Handle("/activity", s.requireAuth(http.HandlerFunc(s.handleActivity)))
	mux.Handle("/account", s.requireAuth(http.HandlerFunc(s.handleAccount)))
	mux.Handle("/people", s.requireAuth(s.requireOwner(http.HandlerFunc(s.handlePeople))))
	mux.Handle("/settings", s.requireAuth(s.requireOwner(http.HandlerFunc(s.handleSettings))))
	mux.Handle("/messages", s.requireAuth(http.HandlerFunc(s.handleMessages)))
	mux.Handle("/messages/qr.png", s.requireAuth(http.HandlerFunc(s.handlePairingQR)))
	mux.Handle("/logout", s.requireAuth(http.HandlerFunc(s.handleLogout)))

	return s.securityHeaders(s.requestLogger(mux))
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// requireAuth redirects to /login when no valid session cookie is present.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sid := readSessionCookie(r)
		if sid == "" {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		// Resolve the whole person, not just their id: every page shows
		// who is signed in, and every change records who made it.
		user, err := s.auth.CurrentUser(r.Context(), sid)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		ctx := context.WithValue(r.Context(), ctxUserID{}, user.ID)
		next.ServeHTTP(w, r.WithContext(withUser(ctx, user)))
	})
}

func readSessionCookie(r *http.Request) string {
	c, err := r.Cookie(SessionCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

// SessionCookieName is the name of the dashboard session cookie.
const SessionCookieName = "fuelmind_session"

type ctxUserID struct{}

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
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// isLoopback reports whether the request comes from the shop PC itself.
func isLoopback(r *http.Request) bool {
	ip := net.ParseIP(clientIP(r))
	return ip != nil && ip.IsLoopback()
}

func funcMap() template.FuncMap {
	return template.FuncMap{
		"formatMoney":   formatMoney,
		"formatPrice":   formatPrice,
		"formatLiters":  formatLiters,
		"formatDate":    formatDate,
		"severityClass": severityClass,
		"messageKind":   messageKindLabel,
		"statusClass":   messageStatusClass,
		"auditAction":   auditActionLabel,
		"auditClass":    auditActionClass,
		"roleLabel":     storage.RoleLabel,
		"ingestSource":  ingestSourceLabel,
		"ingestOutcome": ingestOutcomeLabel,
		"ingestClass":   ingestOutcomeClass,
	}
}

// auditActionLabel turns an action slug into what actually happened, in
// the owner's words. The slug is what the code filters on; this is what
// the owner reads.
func auditActionLabel(v any) string {
	switch fmt.Sprint(v) {
	case storage.ActionLogin:
		return "Signed in"
	case storage.ActionLoginFailed:
		return "Wrong PIN"
	case storage.ActionLogout:
		return "Signed out"
	case storage.ActionPINChanged:
		return "PIN changed"
	case storage.ActionPINResetAsked:
		return "Asked for a new PIN"
	case storage.ActionPINResetDone:
		return "New PIN given"
	case storage.ActionPINResetDenied:
		return "PIN request dismissed"
	case storage.ActionUserAdded:
		return "Person added"
	case storage.ActionUserChanged:
		return "Person changed"
	case storage.ActionUserRemoved:
		return "Person removed"
	case storage.ActionSaleEntered:
		return "Sale entered by hand"
	case storage.ActionFileImported:
		return "File imported"
	case storage.ActionFolderAdded:
		return "Watch folder added"
	case storage.ActionFolderChanged:
		return "Watch folder changed"
	case storage.ActionFolderRemoved:
		return "Watch folder removed"
	case storage.ActionPriceSet:
		return "Purchase price set"
	case storage.ActionCreditPayment:
		return "Repayment recorded"
	case storage.ActionSettingChanged:
		return "Setting changed"
	case storage.ActionMessagingChanged:
		return "Messaging changed"
	case storage.ActionModelChanged:
		return "AI model changed"
	case storage.ActionQuestionAnswered:
		return "Question answered"
	default:
		return fmt.Sprint(v)
	}
}

// auditActionClass highlights the entries worth a second look: money
// moving, and someone failing to sign in.
func auditActionClass(v any) string {
	switch fmt.Sprint(v) {
	case storage.ActionLoginFailed, storage.ActionUserRemoved:
		return "alert"
	case storage.ActionPINResetAsked, storage.ActionPINResetDone, storage.ActionPINResetDenied:
		return "warn"
	case storage.ActionSaleEntered, storage.ActionCreditPayment, storage.ActionPriceSet:
		return "warn"
	default:
		return ""
	}
}

// ingestSourceLabel says how a batch of data reached FuelMind, in the
// owner's words rather than the column's.
func ingestSourceLabel(v any) string {
	switch fmt.Sprint(v) {
	case storage.IngestSourceFolder:
		return "Watched folder"
	case storage.IngestSourceUpload:
		return "Imported file"
	case storage.IngestSourceManual:
		return "Entered by hand"
	default:
		return fmt.Sprint(v)
	}
}

// ingestOutcomeLabel turns an outcome into something that reads like an
// answer to "did my data get in?".
func ingestOutcomeLabel(v any) string {
	switch fmt.Sprint(v) {
	case storage.IngestOK:
		return "All in"
	case storage.IngestPartial:
		return "Some rows skipped"
	case storage.IngestFailed:
		return "Nothing taken in"
	default:
		return fmt.Sprint(v)
	}
}

// ingestOutcomeClass colours that outcome.
func ingestOutcomeClass(v any) string {
	switch fmt.Sprint(v) {
	case storage.IngestOK:
		return "ok"
	case storage.IngestFailed:
		return "alert"
	default:
		return "warn"
	}
}

func formatMoney(n float64) string { return format.Money(n) }

func formatLiters(n float64) string { return format.Liters(n) }

func formatPrice(n float64) string { return format.Price(n) }

func formatNum(n float64, decimals int) string { return format.Num(n, decimals) }

// formatDate renders "2026-09-07" as "Mon 7 Sep 2026".
func formatDate(t string) string { return format.Date(t) }

// messageStatusClass colours an outbox row by what happened to it.
func messageStatusClass(s any) string {
	switch fmt.Sprint(s) {
	case "sent":
		return "ok"
	case "failed":
		return "alert"
	default:
		return "warn"
	}
}

func severityClass(s any) string {
	switch fmt.Sprint(s) {
	case "alert":
		return "alert"
	case "warn":
		return "warn"
	default:
		return "info"
	}
}

// render executes a page into a buffer first, so a template error becomes
// a clean 500 instead of half a page served with status 200.
func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, data map[string]any) {
	s.renderStatus(w, r, http.StatusOK, name, data)
}

// renderStatus writes one page. It fills in the things every page needs
// — the version in the footer, and who is signed in, which decides what
// the navigation offers — so no handler can forget them.
func (s *Server) renderStatus(w http.ResponseWriter, r *http.Request, status int, name string, data map[string]any) {
	tmpl, ok := s.pages[strings.TrimSuffix(name, ".html")]
	if !ok {
		s.logger.Error("template not found", "template", name)
		http.Error(w, "template not found", http.StatusInternalServerError)
		return
	}
	if data == nil {
		data = map[string]any{}
	}
	data["Version"] = s.Version
	user, ok := data["User"].(storage.User)
	if !ok {
		user = userFrom(r.Context())
		data["User"] = user
	}
	// The badge on People. An owner should find out that someone cannot
	// sign in by opening FuelMind, not by being telephoned.
	if _, set := data["PINRequests"]; !set {
		data["PINRequests"] = 0
		if user.IsOwner() {
			if reqs, err := s.store.PendingPINResets(r.Context()); err == nil {
				data["PINRequests"] = len(reqs)
			}
		}
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "base", data); err != nil {
		s.logger.Error("template render", "template", name, "err", err)
		http.Error(w, "The page could not be displayed. The error has been logged.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

// Materializer recomputes the data mart. The web layer only needs this
// one method, so tests can pass a stub.
type Materializer interface {
	MaterializeSince(ctx context.Context, since time.Time) error
}
