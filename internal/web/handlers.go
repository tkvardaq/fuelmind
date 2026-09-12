package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fuelmind/fuelmind/internal/auth"
	"github.com/fuelmind/fuelmind/internal/llm"
)

func (s *Server) setSessionCookie(w http.ResponseWriter, sid string) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    sid,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(auth.SessionTTL),
	})
}

// handleSetup is the first-run PIN wizard. It only accepts requests from
// the shop PC itself (loopback) while no PIN is set.
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	pinSet, err := s.auth.IsPINSet(r.Context())
	if err != nil {
		s.renderError(w, "setup: check pin", err)
		return
	}
	remote := !isLoopback(r)
	page := func(status int, errMsg string) {
		s.renderStatus(w, status, "setup", map[string]any{
			"PINSet": pinSet, "Remote": remote, "Port": s.port, "Error": errMsg,
		})
	}
	switch r.Method {
	case http.MethodGet:
		page(http.StatusOK, "")
	case http.MethodPost:
		if pinSet {
			http.Error(w, "PIN already set", http.StatusForbidden)
			return
		}
		if remote {
			page(http.StatusForbidden, "")
			return
		}
		pin := strings.TrimSpace(r.FormValue("pin"))
		confirm := strings.TrimSpace(r.FormValue("pin_confirm"))
		switch {
		case pin == "" || pin != confirm:
			page(http.StatusOK, "The two PINs don't match. Type the same PIN in both boxes.")
			return
		case len(pin) < auth.MinPINLength:
			page(http.StatusOK, "The PIN must be at least 8 characters.")
			return
		}
		if err := s.auth.SetupPIN(r.Context(), pin); err != nil {
			page(http.StatusOK, err.Error())
			return
		}
		consent := r.FormValue("telemetry_consent") == "true"
		if err := s.store.SetLocalConfig(r.Context(), "telemetry_consent", strconv.FormatBool(consent), "", false); err != nil {
			s.logger.Warn("failed to persist telemetry consent", "err", err)
		}
		sid, err := s.auth.Login(r.Context(), pin, r.UserAgent())
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		s.setSessionCookie(w, sid)
		http.Redirect(w, r, "/", http.StatusFound)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleLogin shows the login form, or redirects to /setup on first run.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	pinSet, err := s.auth.IsPINSet(r.Context())
	if err != nil {
		s.renderError(w, "login: check pin", err)
		return
	}
	if !pinSet {
		http.Redirect(w, r, "/setup", http.StatusFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.render(w, "login", map[string]any{})
	case http.MethodPost:
		pin := strings.TrimSpace(r.FormValue("pin"))
		sid, err := s.auth.Login(r.Context(), pin, r.UserAgent())
		if err != nil {
			msg := "Invalid credentials."
			status := http.StatusOK
			var locked *auth.LockedError
			if errors.As(err, &locked) {
				msg = "Too many wrong PINs. Login is locked until " + locked.Until.Local().Format("15:04") + "."
				status = http.StatusTooManyRequests
			} else if !errors.Is(err, auth.ErrInvalidPIN) {
				s.logger.Error("login", "err", err)
			}
			s.renderStatus(w, status, "login", map[string]any{"Error": msg})
			return
		}
		s.setSessionCookie(w, sid)
		http.Redirect(w, r, "/", http.StatusFound)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleLogout deletes the session. POST only (no logout via prefetch).
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	_ = s.auth.Logout(r.Context(), readSessionCookie(r))
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookieName, Value: "", Path: "/", MaxAge: -1, Expires: time.Unix(0, 0),
	})
	http.Redirect(w, r, "/login", http.StatusFound)
}

// handleHealthz returns 200 when the dashboard and database respond. Used
// by the launcher's post-update self-check.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.DB().PingContext(ctx); err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ok"))
}

// handleStatic serves embedded CSS and icons.
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/static/")
	data, err := templatesFS.ReadFile("templates/static/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch {
	case strings.HasSuffix(name, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case strings.HasSuffix(name, ".svg"):
		w.Header().Set("Content-Type", "image/svg+xml")
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(data)
}

func (s *Server) dashboardData(ctx context.Context) (map[string]any, error) {
	totals, err := s.store.TodayTotals(ctx)
	if err != nil {
		return nil, err
	}
	score, err := s.store.LatestScore(ctx)
	if err != nil {
		return nil, err
	}
	unresolved, err := s.store.UnresolvedProducts(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"LoggedIn":     true,
		"Totals":       totals,
		"Score":        score,
		"ScoreIsToday": score.Date == totals.Date,
		"Issues":       parseIssues(score.IssuesJSON),
		"Unresolved":   unresolved,
	}, nil
}

// handleDashboard is the today-at-a-glance page. It also owns the "/"
// catch-all, so any other unknown path is a 404.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		s.renderStatus(w, http.StatusNotFound, "notfound", map[string]any{"LoggedIn": true})
		return
	}
	data, err := s.dashboardData(r.Context())
	if err != nil {
		s.renderError(w, "dashboard", err)
		return
	}
	s.render(w, "dashboard", data)
}

// handleAsk answers a question typed into the dashboard's Ask box.
func (s *Server) handleAsk(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	q := strings.TrimSpace(r.FormValue("q"))
	if len(q) > 300 {
		q = q[:300]
	}
	data, err := s.dashboardData(r.Context())
	if err != nil {
		s.renderError(w, "ask", err)
		return
	}
	answer := "The Ask box is not available on this station."
	if s.Router != nil {
		mctx, err := s.martContext(r.Context())
		if err != nil {
			s.renderError(w, "ask: context", err)
			return
		}
		ans, path, rerr := s.Router.Route(r.Context(), q, mctx)
		if rerr != nil {
			s.logger.Warn("ask: llm", "err", rerr)
		}
		s.logger.Info("ask", "path", path)
		answer = ans
	}
	data["Question"] = q
	data["Answer"] = answer
	s.render(w, "dashboard", data)
}

// martContext builds the pre-computed numbers the intent router may use.
func (s *Server) martContext(ctx context.Context) (llm.MartContext, error) {
	now := time.Now()
	today, err := s.store.TodayTotals(ctx)
	if err != nil {
		return llm.MartContext{}, err
	}
	yesterday, err := s.store.DayTotals(ctx, now.AddDate(0, 0, -1).Format("2006-01-02"))
	if err != nil {
		return llm.MartContext{}, err
	}
	week, err := s.store.RecentDailySales(ctx, 7)
	if err != nil {
		return llm.MartContext{}, err
	}
	credit, err := s.store.RecentCredit(ctx, 10000)
	if err != nil {
		return llm.MartContext{}, err
	}
	score, err := s.store.LatestScore(ctx)
	if err != nil {
		return llm.MartContext{}, err
	}
	m := llm.MartContext{
		Date:              today.Date,
		RevenueToday:      today.Revenue,
		RevenueYesterday:  yesterday.Revenue,
		VolumeTodayLiters: today.VolumeLiters,
		TransactionsToday: today.TransactionCount,
		OverallScore:      score.Overall,
		ScoreDate:         score.Date,
	}
	for _, row := range week {
		m.Volume7d += row.VolumeLiters
		m.Revenue7d += row.Revenue
		if row.Date == today.Date {
			m.TopProducts = append(m.TopProducts, llm.ProductRow{Code: row.ProductCode, Volume: row.VolumeLiters, Money: row.Revenue})
		}
	}
	for _, c := range credit {
		m.CreditOutstanding += c.OutstandingAmount
		m.CreditCustomers++
	}
	for _, is := range parseIssues(score.IssuesJSON) {
		if msg, ok := is["message"].(string); ok {
			m.OpenIssues = append(m.OpenIssues, msg)
		}
	}
	return m, nil
}

func (s *Server) handleSales(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.RecentDailySales(r.Context(), 30)
	if err != nil {
		s.renderError(w, "recent sales", err)
		return
	}
	s.render(w, "sales", map[string]any{"LoggedIn": true, "Rows": rows})
}

func (s *Server) handleCredit(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.RecentCredit(r.Context(), 50)
	if err != nil {
		s.renderError(w, "credit", err)
		return
	}
	asOf := ""
	if len(rows) > 0 {
		asOf = rows[0].AsOfDate
	}
	s.render(w, "credit", map[string]any{"LoggedIn": true, "Rows": rows, "AsOf": asOf})
}

func (s *Server) handleScore(w http.ResponseWriter, r *http.Request) {
	score, err := s.store.LatestScore(r.Context())
	if err != nil {
		s.renderError(w, "latest score", err)
		return
	}
	s.render(w, "score", map[string]any{
		"LoggedIn":     true,
		"Score":        score,
		"ScoreIsToday": score.Date == time.Now().Format("2006-01-02"),
		"Issues":       parseIssues(score.IssuesJSON),
	})
}

// renderError logs the detail and shows the user a generic message.
func (s *Server) renderError(w http.ResponseWriter, what string, err error) {
	s.logger.Error(what, "err", err)
	http.Error(w, "Something went wrong loading this page. The error has been logged.", http.StatusInternalServerError)
}

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
