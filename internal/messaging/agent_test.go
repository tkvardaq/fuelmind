package messaging_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fuelmind/fuelmind/internal/llm"
	"github.com/fuelmind/fuelmind/internal/messaging"
	"github.com/fuelmind/fuelmind/internal/storage"
)

// fakeSender records what would have gone out.
type fakeSender struct {
	mu    sync.Mutex
	ready bool
	err   error
	sent  []string
	to    []string
}

func (f *fakeSender) Name() string { return "fake" }
func (f *fakeSender) Ready() bool  { f.mu.Lock(); defer f.mu.Unlock(); return f.ready }
func (f *fakeSender) Send(_ context.Context, to, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.to, f.sent = append(f.to, to), append(f.sent, body)
	return nil
}
func (f *fakeSender) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.sent) }
func (f *fakeSender) setReady(b bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ready = b
}

type stubMart struct{ ctx llm.MartContext }

func (s stubMart) Context(context.Context) (llm.MartContext, error) { return s.ctx, nil }

func newStore(t *testing.T) *storage.Storage {
	t.Helper()
	s, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// at builds a "now" function standing at a fixed local time.
func at(hour int) func() time.Time {
	return func() time.Time { return time.Date(2026, 9, 13, hour, 5, 0, 0, time.Local) }
}

func agentFor(t *testing.T, store *storage.Storage, mart llm.MartContext, s *fakeSender, hour int) *messaging.Agent {
	t.Helper()
	return messaging.New(messaging.Config{
		Store: store, Context: stubMart{mart}, Sender: s,
		StationID: "FM-TEST1", Now: at(hour),
	})
}

func enable(t *testing.T, store *storage.Storage, summaryHour int) {
	t.Helper()
	set := messaging.Settings{Enabled: true, Recipient: "+92 300 1234567", SummaryHour: summaryHour, Alerts: true}
	if err := set.Save(context.Background(), store); err != nil {
		t.Fatal(err)
	}
}

func TestOffByDefault(t *testing.T) {
	store := newStore(t)
	s := &fakeSender{ready: true}
	if got := messaging.LoadSettings(context.Background(), store); got.Enabled {
		t.Fatal("messaging is on before the owner turned it on")
	}
	if err := agentFor(t, store, llm.MartContext{}, s, 22).Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.count() != 0 {
		t.Errorf("sent %d messages while switched off", s.count())
	}
}

func TestDailySummarySendsOncePerDay(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	enable(t, store, 21)
	s := &fakeSender{ready: true}
	a := agentFor(t, store, llm.MartContext{
		Date: "2026-09-13", RevenueToday: 92662, RevenueYesterday: 80000,
		VolumeTodayLiters: 340, TransactionsToday: 3,
		CreditOutstanding: 45100, CreditCustomers: 2, OverallScore: 88,
	}, s, 21)

	for i := 0; i < 3; i++ {
		if err := a.Once(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if s.count() != 1 {
		t.Fatalf("sent %d summaries, want exactly 1", s.count())
	}
	body := s.sent[0]
	for _, want := range []string{"PKR 92,662", "340.00 L", "3 sale(s)", "PKR 45,100", "88/100", "16% higher"} {
		if !strings.Contains(body, want) {
			t.Errorf("summary missing %q:\n%s", want, body)
		}
	}
	if s.to[0] != "923001234567" {
		t.Errorf("recipient = %q, want the digits of the number the owner entered", s.to[0])
	}
}

func TestSummaryWaitsForItsHour(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	enable(t, store, 21)
	s := &fakeSender{ready: true}
	if err := agentFor(t, store, llm.MartContext{Date: "2026-09-13"}, s, 14).Once(ctx); err != nil {
		t.Fatal(err)
	}
	if s.count() != 0 {
		t.Errorf("summary went out at 14:05, before the 21:00 the owner chose")
	}
}

func TestQueuedUntilTheChannelIsReady(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	enable(t, store, 21)
	s := &fakeSender{ready: false}
	a := agentFor(t, store, llm.MartContext{Date: "2026-09-13", RevenueToday: 100}, s, 21)
	if err := a.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if s.count() != 0 {
		t.Fatal("sent while the channel was not ready")
	}
	queued, _, _, err := store.MessageCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if queued != 1 {
		t.Fatalf("queued = %d, want the summary to be waiting", queued)
	}
	s.setReady(true)
	if _, _, err := a.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if s.count() != 1 {
		t.Errorf("queued message did not go out once the channel was ready")
	}
}

func TestFailedSendRetriesThenGivesUp(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	enable(t, store, 21)
	s := &fakeSender{ready: true, err: errors.New("no route to host")}
	a := agentFor(t, store, llm.MartContext{Date: "2026-09-13"}, s, 21)
	for i := 0; i < 6; i++ {
		if err := a.Once(ctx); err != nil {
			t.Fatal(err)
		}
	}
	queued, sent, failed, err := store.MessageCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sent != 0 || failed != 1 || queued != 0 {
		t.Errorf("counts after repeated failures: queued=%d sent=%d failed=%d, want 0/0/1", queued, sent, failed)
	}
	msgs, err := store.RecentMessages(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msgs[0].LastError, "no route to host") {
		t.Errorf("the failure reason was not kept: %q", msgs[0].LastError)
	}
}

func TestScoreDropAlert(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	enable(t, store, 23) // past the summary hour, so only the alert can fire
	s := &fakeSender{ready: true}
	a := agentFor(t, store, llm.MartContext{
		Date: "2026-09-13", ScoreDate: "2026-09-13", OverallScore: 61,
		OpenIssues: []string{"inventory variance high on T1"},
	}, s, 9)
	if err := a.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if s.count() != 1 {
		t.Fatalf("sent %d messages, want the score alert", s.count())
	}
	if !strings.Contains(s.sent[0], "61/100") || !strings.Contains(s.sent[0], "inventory variance high on T1") {
		t.Errorf("score alert does not say what happened:\n%s", s.sent[0])
	}
}

func TestHealthyScoreIsQuiet(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	enable(t, store, 23)
	s := &fakeSender{ready: true}
	a := agentFor(t, store, llm.MartContext{Date: "2026-09-13", ScoreDate: "2026-09-13", OverallScore: 91}, s, 9)
	if err := a.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if s.count() != 0 {
		t.Errorf("alerted on a healthy station: %q", s.sent)
	}
}

func TestCreditOverdueAlert(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	enable(t, store, 23)
	if _, err := store.DB().Exec(`INSERT INTO credit_outstanding
		(customer_phone, as_of_date, outstanding_amount, transaction_count, days_overdue)
		VALUES ('+923009876543', '2026-09-13', 45100, 4, 41),
		       ('+923001112222', '2026-09-13', 900, 1, 2)`); err != nil {
		t.Fatal(err)
	}
	s := &fakeSender{ready: true}
	a := agentFor(t, store, llm.MartContext{Date: "2026-09-13", CreditOutstanding: 46000}, s, 9)
	if err := a.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if s.count() != 1 {
		t.Fatalf("sent %d messages, want the credit alert", s.count())
	}
	body := s.sent[0]
	if !strings.Contains(body, "+923009876543") || !strings.Contains(body, "41 days") {
		t.Errorf("credit alert does not name the overdue customer:\n%s", body)
	}
	if strings.Contains(body, "+923001112222") {
		t.Errorf("credit alert named a customer who is not overdue:\n%s", body)
	}
}

func TestTestMessageAlwaysSends(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	enable(t, store, 23)
	s := &fakeSender{ready: true}
	a := agentFor(t, store, llm.MartContext{Date: "2026-09-13"}, s, 9)
	for i := 0; i < 2; i++ {
		if err := a.QueueTest(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if s.count() != 2 {
		t.Fatalf("sent %d test messages, want 2 (a test is never deduped)", s.count())
	}
	if !strings.Contains(s.sent[0], "FM-TEST1") {
		t.Errorf("test message does not identify the station:\n%s", s.sent[0])
	}
}

func TestTestMessageNeedsANumber(t *testing.T) {
	store := newStore(t)
	a := agentFor(t, store, llm.MartContext{}, &fakeSender{ready: true}, 9)
	if err := a.QueueTest(context.Background()); !errors.Is(err, messaging.ErrNoRecipient) {
		t.Errorf("QueueTest with no number = %v, want ErrNoRecipient", err)
	}
}

func TestNormalizeNumber(t *testing.T) {
	for in, want := range map[string]string{
		"+92 300 123 4567": "923001234567",
		"0300-1234567":     "03001234567",
		"":                 "",
	} {
		if got := messaging.NormalizeNumber(in); got != want {
			t.Errorf("NormalizeNumber(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIngestStalledAlert(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	enable(t, store, 23)
	// A station that was ingesting, then went quiet eight hours ago.
	// Eight hours before the moment the agent thinks it is, stored the
	// way SQLite stores CURRENT_TIMESTAMP: UTC, no zone marker.
	stale := at(9)().UTC().Add(-8 * time.Hour).Format("2006-01-02 15:04:05")
	if _, err := store.DB().Exec(`INSERT INTO raw_pos_transactions
		(ingestion_batch_id, pos_source_id, raw_payload, payload_hash, received_at)
		VALUES ('b1', 'lane_1', '{}', 'h1', ?)`, stale); err != nil {
		t.Fatal(err)
	}
	s := &fakeSender{ready: true}
	a := agentFor(t, store, llm.MartContext{Date: "2026-09-13"}, s, 9)
	if err := a.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if s.count() != 1 {
		t.Fatalf("sent %d messages, want the no-data alert", s.count())
	}
	if !strings.Contains(s.sent[0], "no sales data coming in") || !strings.Contains(s.sent[0], "8 hours ago") {
		t.Errorf("stalled alert does not say what happened:\n%s", s.sent[0])
	}
}

func TestFreshInstallIsNotToldItIsStalled(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	enable(t, store, 23)
	s := &fakeSender{ready: true}
	a := agentFor(t, store, llm.MartContext{Date: "2026-09-13"}, s, 9)
	if err := a.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if s.count() != 0 {
		t.Errorf("a station that has never ingested anything was alerted: %q", s.sent)
	}
}
