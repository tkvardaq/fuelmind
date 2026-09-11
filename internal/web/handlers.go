package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/fuelmind/fuelmind/internal/auth"
)

// handleSetup shows the "set your PIN" form on first run, or the
// login form if a PIN is already set. After the form posts a new
// PIN, redirects to /.
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	pinSet, err := s.auth.IsPINSet(r.Context())
	if err != nil {
		s.renderError(w, r, "setup: check pin", err)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.render(w, "setup.html", map[string]any{
			"PINSet":   pinSet,
			"LoggedIn": false,
		})
	case http.MethodPost:
		if pinSet {
			http.Error(w, "PIN already set", http.StatusForbidden)
			return
		}
		pin := strings.TrimSpace(r.FormValue("pin"))
		confirm := strings.TrimSpace(r.FormValue("pin_confirm"))
		if pin == "" || pin != confirm {
			s.render(w, "setup.html", map[string]any{
				"PINSet":   pinSet,
				"LoggedIn": false,
				"Error":    "PINs do not match (or are empty).",
			})
			return
		}
		if len(pin) < 8 {
			s.render(w, "setup.html", map[string]any{
				"PINSet":   pinSet,
				"LoggedIn": false,
				"Error":    "PIN must be at least 8 characters.",
			})
			return
		}
		if err := s.auth.SetupPIN(r.Context(), pin); err != nil {
			s.render(w, "setup.html", map[string]any{
				"PINSet":   pinSet,
				"LoggedIn": false,
				"Error":    err.Error(),
			})
			return
		}
		// Telemetry consent: checkbox sends "true" if checked, otherwise empty.
		telemetryConsent := r.FormValue("telemetry_consent") == "true"
		if err := s.store.SetLocalConfig(r.Context(), "telemetry_consent", fmt.Sprintf("%v", telemetryConsent), "", false); err != nil {
			s.logger.Warn("failed to persist telemetry consent", "err", err)
			// Continue anyway; not fatal.
		}
		// Auto-login the owner after they set the PIN.
		sid, err := s.auth.Login(r.Context(), pin, r.UserAgent())
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     SessionCookieName,
			Value:    sid,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			Expires:  time.Now().Add(auth.SessionTTL),
		})
		http.Redirect(w, r, "/", http.StatusFound)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleLogin shows the login form, or redirects to /setup if no
// PIN is set yet (first-run case).
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	pinSet, err := s.auth.IsPINSet(r.Context())
	if err != nil {
		s.renderError(w, r, "login: check pin", err)
		return
	}
	if !pinSet {
		http.Redirect(w, r, "/setup", http.StatusFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.render(w, "login.html", map[string]any{
			"LoggedIn": false,
			"Error":    "",
		})
	case http.MethodPost:
		pin := strings.TrimSpace(r.FormValue("pin"))
		sid, err := s.auth.Login(r.Context(), pin, r.UserAgent())
		if err != nil {
			s.render(w, "login.html", map[string]any{
				"LoggedIn": false,
				"Error":    "Invalid credentials.",
			})
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     SessionCookieName,
			Value:    sid,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			Expires:  time.Now().Add(auth.SessionTTL),
		})
		http.Redirect(w, r, "/", http.StatusFound)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleLogout deletes the session and redirects to /login.
// Requires POST to prevent accidental logout via link/prefetch.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sid := readSessionCookie(r)
	_ = s.auth.Logout(r.Context(), sid)
	http.SetCookie(w, &http.Cookie{
		Name:    SessionCookieName,
		Value:   "",
		Path:    "/",
		MaxAge:  -1,
		Expires: time.Unix(0, 0),
	})
	http.Redirect(w, r, "/login", http.StatusFound)
}

// handleHealthz returns 200 if the dashboard is up. Used by the
// unattended update self-check (Phase 6) and by external monitors.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// handleStatic serves embedded CSS. No JS for v1.
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	// Strip the /static/ prefix.
	name := strings.TrimPrefix(r.URL.Path, "/static/")
	data, err := templatesFS.ReadFile("templates/static/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(name, ".css") {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	}
	_, _ = w.Write(data)
}

// handleDashboard is the today-at-a-glance page.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	totals, err := s.store.TodayTotals(r.Context())
	if err != nil {
		s.renderError(w, r, "today totals", err)
		return
	}
	score, err := s.store.LatestScore(r.Context())
	if err != nil {
		s.renderError(w, r, "latest score", err)
		return
	}
	issues := parseIssues(score.IssuesJSON)
	s.render(w, "dashboard.html", map[string]any{
		"LoggedIn": true,
		"Totals":   totals,
		"Score":    score,
		"Issues":   issues,
	})
}

// handleSales shows the recent sales table.
func (s *Server) handleSales(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.RecentDailySales(r.Context(), 30)
	if err != nil {
		s.renderError(w, r, "recent sales", err)
		return
	}
	s.render(w, "sales.html", map[string]any{
		"LoggedIn": true,
		"Rows":     rows,
	})
}

// handleCredit shows credit-outstanding by customer.
func (s *Server) handleCredit(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.RecentCredit(r.Context(), 50)
	if err != nil {
		s.renderError(w, r, "credit", err)
		return
	}
	s.render(w, "credit.html", map[string]any{
		"LoggedIn": true,
		"Rows":     rows,
	})
}

// handleScore shows the FuelMind Score with the issue list.
func (s *Server) handleScore(w http.ResponseWriter, r *http.Request) {
	score, err := s.store.LatestScore(r.Context())
	if err != nil {
		s.renderError(w, r, "latest score", err)
		return
	}
	issues := parseIssues(score.IssuesJSON)
	s.render(w, "score.html", map[string]any{
		"LoggedIn": true,
		"Score":    score,
		"Issues":   issues,
	})
}

// render is a small wrapper to keep handler code tight.
//
// Each page template (dashboard.html, sales.html, etc.) was parsed
// into its own template set with the shared "base" chrome. The
// page file defines a "body" block and then invokes
// {{template "base" .}}, so we always execute "base".
func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	tmpl, ok := s.pages[strings.TrimSuffix(name, ".html")]
	if !ok {
		s.logger.Error("template not found", "template", name)
		http.Error(w, "template not found", http.StatusInternalServerError)
		return
	}
	if err := tmpl.ExecuteTemplate(w, "base", data); err != nil {
		s.logger.Error("template render", "template", name, "err", err)
	}
}

func (s *Server) renderError(w http.ResponseWriter, r *http.Request, what string, err error) {
	s.logger.Error(what, "err", err)
	http.Error(w, what+": "+err.Error(), http.StatusInternalServerError)
}

// parseIssues turns the issues_json column into a slice of maps the
// template can iterate. The mart's score package defines the
// concrete shape; for v1 we keep the template dumb and pass through
// the JSON as a map.
func parseIssues(jsonStr string) []map[string]any {
	if jsonStr == "" {
		return nil
	}
	var out []map[string]any
	if err := json.Unmarshal([]byte(jsonStr), &out); err != nil {
		return nil
	}
	return out
}
