// Package ask answers owner questions about a station. It builds the
// pre-computed context from the data mart and hands it to the intent
// router, so the dashboard's Ask box and the cloud relay (WhatsApp / SMS
// / support) always give the same answer from the same numbers.
package ask

import (
	"context"
	"fmt"
	"time"

	"github.com/fuelmind/fuelmind/internal/llm"
	"github.com/fuelmind/fuelmind/internal/storage"
)

// MaxQuestionLength is the longest question accepted from any channel.
const MaxQuestionLength = 300

// Service answers questions for one station.
type Service struct {
	store  *storage.Storage
	router *llm.Router
}

// New builds a Service. A nil router means only "not available" answers.
func New(store *storage.Storage, router *llm.Router) *Service {
	return &Service{store: store, router: router}
}

// Answer replies to one question and reports which path produced it.
func (s *Service) Answer(ctx context.Context, question string) (answer, path string, err error) {
	if s.router == nil {
		return "Answering questions is not available on this station.", "unavailable", nil
	}
	if len(question) > MaxQuestionLength {
		question = question[:MaxQuestionLength]
	}
	mctx, err := s.Context(ctx)
	if err != nil {
		return "I could not read the station's figures just now.", "error", err
	}
	return s.router.Route(ctx, question, mctx)
}

// Context builds the numbers the router is allowed to use. Everything
// here is already computed by the mart; nothing is derived on the fly.
func (s *Service) Context(ctx context.Context) (llm.MartContext, error) {
	now := time.Now()
	today, err := s.store.TodayTotals(ctx)
	if err != nil {
		return llm.MartContext{}, fmt.Errorf("today: %w", err)
	}
	yesterday, err := s.store.DayTotals(ctx, now.AddDate(0, 0, -1).Format("2006-01-02"))
	if err != nil {
		return llm.MartContext{}, fmt.Errorf("yesterday: %w", err)
	}
	week, err := s.store.RecentDailySales(ctx, 7)
	if err != nil {
		return llm.MartContext{}, fmt.Errorf("week: %w", err)
	}
	credit, err := s.store.RecentCredit(ctx, 10000)
	if err != nil {
		return llm.MartContext{}, fmt.Errorf("credit: %w", err)
	}
	score, err := s.store.LatestScore(ctx)
	if err != nil {
		return llm.MartContext{}, fmt.Errorf("score: %w", err)
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
			m.TopProducts = append(m.TopProducts, llm.ProductRow{
				Code: row.ProductCode, Volume: row.VolumeLiters, Money: row.Revenue,
			})
		}
	}
	for _, c := range credit {
		if c.OutstandingAmount <= 0 {
			continue // settled customers are not "outstanding"
		}
		m.CreditOutstanding += c.OutstandingAmount
		m.CreditCustomers++
	}
	for _, issue := range parseIssueMessages(score.IssuesJSON) {
		m.OpenIssues = append(m.OpenIssues, issue)
	}
	return m, nil
}
