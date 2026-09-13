package web

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fuelmind/fuelmind/internal/relay"
	"github.com/fuelmind/fuelmind/internal/storage"
)

// handleMargin shows revenue against what the fuel cost. Days with no
// purchase price on record are shown as "no cost recorded" rather than as
// pure profit.
func (s *Server) handleMargin(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.RecentMargin(r.Context(), 30)
	if err != nil {
		s.renderError(w, "margin", err)
		return
	}
	prices, err := s.store.PurchasePrices(r.Context(), 5)
	if err != nil {
		s.renderError(w, "margin: prices", err)
		return
	}
	var revenue, cost, margin float64
	costedDays := map[string]bool{}
	for _, m := range rows {
		revenue += m.Revenue
		cost += m.CostOfGoods
		margin += m.MarginAmount
		if m.Costed {
			costedDays[m.Date] = true
		}
	}
	pct := 0.0
	if revenue > 0 && cost > 0 {
		pct = margin / revenue * 100
	}
	s.render(w, "margin", map[string]any{
		"LoggedIn":    true,
		"Rows":        rows,
		"HavePrices":  len(prices) > 0,
		"TotalRev":    revenue,
		"TotalCost":   cost,
		"TotalMargin": margin,
		"TotalPct":    pct,
		"CostedDays":  len(costedDays),
	})
}

// handleCustomer shows one credit customer: balance, sales and payments,
// and records a repayment.
func (s *Server) handleCustomer(w http.ResponseWriter, r *http.Request) {
	phone := strings.TrimSpace(r.URL.Query().Get("phone"))
	if phone == "" {
		http.Redirect(w, r, "/credit", http.StatusFound)
		return
	}
	var formError, notice string
	if r.Method == http.MethodPost {
		amount, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(r.FormValue("amount")), ",", ""), 64)
		paidOn := strings.TrimSpace(r.FormValue("paid_on"))
		if paidOn == "" {
			paidOn = time.Now().Format("2006-01-02")
		}
		switch {
		case err != nil || amount <= 0:
			formError = "Enter how much the customer paid, for example 5000."
		default:
			perr := s.store.RecordCreditPayment(r.Context(), storage.CreditPayment{
				CustomerPhone: phone,
				PaidOn:        paidOn,
				Amount:        amount,
				Method:        strings.TrimSpace(r.FormValue("method")),
				Note:          strings.TrimSpace(r.FormValue("note")),
			})
			if perr != nil {
				formError = perr.Error()
			} else {
				notice = "Payment recorded."
				s.refreshMart(r)
			}
		}
	}

	sales, err := s.store.CustomerCreditHistory(r.Context(), phone, 100)
	if err != nil {
		s.renderError(w, "customer history", err)
		return
	}
	payments, err := s.store.CreditPayments(r.Context(), phone, 100)
	if err != nil {
		s.renderError(w, "customer payments", err)
		return
	}
	var balance float64
	var overdue int
	credit, err := s.store.RecentCredit(r.Context(), 10000)
	if err != nil {
		s.renderError(w, "customer balance", err)
		return
	}
	for _, c := range credit {
		if c.CustomerPhone == phone {
			balance, overdue = c.OutstandingAmount, c.DaysOverdue
		}
	}
	s.render(w, "customer", map[string]any{
		"LoggedIn": true,
		"Phone":    phone,
		"Balance":  balance,
		"Overdue":  overdue,
		"Sales":    sales,
		"Payments": payments,
		"Error":    formError,
		"Notice":   notice,
		"Today":    time.Now().Format("2006-01-02"),
	})
}

// handleSettings is where the owner records purchase prices and decides
// whether questions may be asked from their phone.
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	var formError, notice string
	if r.Method == http.MethodPost {
		switch r.FormValue("action") {
		case "price":
			cost, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(r.FormValue("cost")), ",", ""), 64)
			date := strings.TrimSpace(r.FormValue("effective_date"))
			if date == "" {
				date = time.Now().Format("2006-01-02")
			}
			switch {
			case err != nil || cost <= 0:
				formError = "Enter the price you paid per litre, for example 250.50."
			default:
				if err := s.store.SetPurchasePrice(r.Context(), storage.PurchasePrice{
					ProductCode:   r.FormValue("product_code"),
					EffectiveDate: date,
					CostPerLiter:  cost,
					Note:          strings.TrimSpace(r.FormValue("note")),
				}); err != nil {
					formError = err.Error()
				} else {
					notice = "Purchase price saved. Margins have been recalculated."
					s.refreshMart(r)
				}
			}
		case "remote_ask":
			on := r.FormValue("remote_ask") == "on"
			if err := relay.SetEnabled(r.Context(), s.store, on); err != nil {
				formError = err.Error()
			} else if on {
				notice = "Questions from your phone are now answered."
			} else {
				notice = "Questions from your phone are now ignored."
			}
		case "telemetry":
			on := r.FormValue("telemetry") == "on"
			if err := s.store.SetLocalConfig(r.Context(), "telemetry_consent", strconv.FormatBool(on), "", false); err != nil {
				formError = err.Error()
			} else {
				notice = "Saved."
			}
		}
	}

	prices, err := s.store.PurchasePrices(r.Context(), 25)
	if err != nil {
		s.renderError(w, "settings: prices", err)
		return
	}
	questions, err := s.store.RecentRemoteQuestions(r.Context(), 10)
	if err != nil {
		s.renderError(w, "settings: questions", err)
		return
	}
	s.render(w, "settings", map[string]any{
		"LoggedIn":   true,
		"Prices":     prices,
		"Products":   []string{"DIESEL", "PETROL_92", "PETROL_95"},
		"RemoteAsk":  relay.Enabled(r.Context(), s.store),
		"Telemetry":  s.store.LocalConfigValue(r.Context(), "telemetry_consent", "false") == "true",
		"CloudOn":    s.CloudConfigured,
		"Questions":  questions,
		"StationID":  s.store.LocalConfigValue(r.Context(), "station_id", ""),
		"Today":      time.Now().Format("2006-01-02"),
		"Error":      formError,
		"Notice":     notice,
		"AskExample": "How much did we sell today?",
	})
}

// refreshMart recomputes the mart after the owner enters data, so the
// change is visible on the next page instead of at the next tick.
func (s *Server) refreshMart(r *http.Request) {
	if s.Mart == nil {
		return
	}
	if err := s.Mart.MaterializeSince(r.Context(), time.Now().AddDate(0, 0, -90)); err != nil {
		s.logger.Warn("recalculate after entry", "err", err)
	}
}
