package llm

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestModelForTier(t *testing.T) {
	cases := map[Tier]string{
		TierBasic:    "",
		TierStandard: "qwen2.5:1.5b",
		TierEnhanced: "qwen2.5:7b",
		TierPro:      "qwen2.5:14b",
	}
	for tier, want := range cases {
		if got := ModelForTier(tier); got != want {
			t.Errorf("ModelForTier(%q) = %q, want %q", tier, got, want)
		}
	}
}

// fakeClient is a deterministic test double for the Ollama client.
type fakeClient struct {
	lastModel  string
	lastPrompt string
	reply      string
	err        error
}

func (f *fakeClient) Generate(_ context.Context, model, prompt string) (string, error) {
	f.lastModel = model
	f.lastPrompt = prompt
	return f.reply, f.err
}
func (f *fakeClient) Ping(_ context.Context) error { return nil }

func sampleContext() MartContext {
	return MartContext{
		Date:              "2026-09-08",
		RevenueToday:      125000.50,
		RevenueYesterday:  110000.00,
		VolumeTodayLiters: 1450.25,
		Volume7d:          9800.00,
		CreditOutstanding: 50000.00,
		CreditCustomers:   3,
		OverallScore:      82,
		ScoreDate:         "2026-09-08",
		OpenIssues:        []string{"low_transaction_count"},
		TopProducts: []ProductRow{
			{Code: "DIESEL", Volume: 800, Money: 70000},
			{Code: "PETROL_92", Volume: 650, Money: 55000},
		},
	}
}

func TestRouter_StandardPath_TodayRevenue(t *testing.T) {
	r := NewRouter(&fakeClient{}, TierStandard)
	ans, path, err := r.Route(context.Background(), "how much did we make today?", sampleContext())
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if path != "standard" {
		t.Errorf("path = %q, want standard", path)
	}
	if !strings.Contains(ans, "125,000.50") {
		t.Errorf("answer = %q, want it to contain today's revenue", ans)
	}
}

func TestRouter_StandardPath_CreditOutstanding(t *testing.T) {
	r := NewRouter(&fakeClient{}, TierStandard)
	ans, path, err := r.Route(context.Background(), "what is the credit outstanding?", sampleContext())
	if err != nil {
		t.Fatal(err)
	}
	if path != "standard" {
		t.Errorf("path = %q, want standard", path)
	}
	if !strings.Contains(ans, "50,000.00") {
		t.Errorf("answer = %q, want it to contain outstanding amount", ans)
	}
	if !strings.Contains(ans, "3") {
		t.Errorf("answer = %q, want it to contain 3 customers", ans)
	}
}

func TestRouter_StandardPath_Score(t *testing.T) {
	r := NewRouter(&fakeClient{}, TierStandard)
	ans, path, err := r.Route(context.Background(), "what is my fuelmind score?", sampleContext())
	if err != nil {
		t.Fatal(err)
	}
	if path != "standard" {
		t.Errorf("path = %q, want standard", path)
	}
	if !strings.Contains(ans, "82") {
		t.Errorf("answer = %q, want it to contain 82", ans)
	}
}

func TestRouter_BasicTierRefusesLLM(t *testing.T) {
	fc := &fakeClient{}
	r := NewRouter(fc, TierBasic)
	// Open-ended question that the standard patterns can't match.
	ans, path, err := r.Route(context.Background(), "why did profit fall this week?", sampleContext())
	if err != nil {
		t.Fatal(err)
	}
	if path != "canned" {
		t.Errorf("path = %q, want canned (Basic tier should not call LLM)", path)
	}
	if fc.lastModel != "" {
		t.Errorf("fakeClient was called with model=%q, want empty (no LLM call expected)", fc.lastModel)
	}
	if !strings.Contains(ans, "I don't have that data") {
		t.Errorf("answer = %q, want it to say 'I don't have that data'", ans)
	}
}

func TestRouter_StandardTierCallsLLM(t *testing.T) {
	fc := &fakeClient{reply: "Revenue today is 125000.50 PKR against 110000.00 PKR yesterday."}
	r := NewRouter(fc, TierStandard)
	ans, path, err := r.Route(context.Background(), "why did profit fall this week?", sampleContext())
	if err != nil {
		t.Fatal(err)
	}
	if path != "llm" {
		t.Errorf("path = %q, want llm", path)
	}
	if ans != fc.reply {
		t.Errorf("answer = %q, want %q", ans, fc.reply)
	}
	if fc.lastModel != ModelForTier(TierStandard) {
		t.Errorf("llm called with model=%q, want %q", fc.lastModel, ModelForTier(TierStandard))
	}
	// Sanity: the prompt should contain the system prefix and the
	// today's revenue number from the mart context.
	if !strings.Contains(fc.lastPrompt, SystemPromptPrefix) {
		t.Error("llm prompt missing system prefix")
	}
	if !strings.Contains(fc.lastPrompt, "125000.50") {
		t.Error("llm prompt missing today's revenue")
	}
}

func TestRouter_LLMFailureReturnsSafeMessage(t *testing.T) {
	fc := &fakeClient{err: errors.New("ollama down")}
	r := NewRouter(fc, TierStandard)
	ans, path, err := r.Route(context.Background(), "why did profit fall this week?", sampleContext())
	if err == nil {
		t.Error("expected error from LLM path")
	}
	if path != "llm-fail" {
		t.Errorf("path = %q, want llm-fail", path)
	}
	if !strings.Contains(ans, "I don't have that data") {
		t.Errorf("answer = %q, want it to refuse to fabricate", ans)
	}
}

func TestRouter_EmptyQuestion(t *testing.T) {
	r := NewRouter(&fakeClient{}, TierStandard)
	ans, path, err := r.Route(context.Background(), "   ", sampleContext())
	if err != nil {
		t.Fatal(err)
	}
	if path != "noop" {
		t.Errorf("path = %q, want noop", path)
	}
	if ans == "" {
		t.Error("answer = empty, want a polite default")
	}
}

func TestRouter_RejectsInventedNumbers(t *testing.T) {
	fc := &fakeClient{reply: "Profit fell because volume dropped 12% week-over-week."}
	r := NewRouter(fc, TierStandard)
	ans, path, err := r.Route(context.Background(), "why did profit fall this week?", sampleContext())
	if err != nil {
		t.Fatal(err)
	}
	if path != "llm-rejected" {
		t.Errorf("path = %q, want llm-rejected (12%% is not in the context)", path)
	}
	if strings.Contains(ans, "12") {
		t.Errorf("invented number leaked to the owner: %q", ans)
	}
}

// Questions the deterministic paths used to answer with an unrelated
// figure (review probe). None may produce a revenue or credit number.
func TestRouter_DoesNotMisroute(t *testing.T) {
	r := NewRouter(&fakeClient{err: errors.New("no llm")}, TierBasic)
	for _, q := range []string{
		"How many liters of diesel did we sell last month?",
		"What is the price of petrol 95?",
		"Is the credit card machine working?",
		"What was the health of tank 2 last week?",
		"Total cash in the drawer?",
	} {
		ans, _, _ := r.Route(context.Background(), q, sampleContext())
		for _, leak := range []string{"125,000", "50,000", "1,450"} {
			if strings.Contains(ans, leak) {
				t.Errorf("%q -> %q (unrelated figure %s)", q, ans, leak)
			}
		}
	}
}

func TestRouter_ProductAndPeriods(t *testing.T) {
	r := NewRouter(&fakeClient{}, TierBasic)
	cases := map[string]string{
		"how much diesel did we sell today?": "800.00 L",
		"sales yesterday":                    "110,000.00",
		"revenue over the last 7 days":       "Over the last 7 days",
		"how much petrol 95 today":           "No Petrol 95 sales",
	}
	for q, want := range cases {
		ans, path, _ := r.Route(context.Background(), q, sampleContext())
		if path != "standard" || !strings.Contains(ans, want) {
			t.Errorf("%q -> [%s] %q, want it to contain %q", q, path, ans, want)
		}
	}
}

func TestUngroundedNumbers(t *testing.T) {
	c := sampleContext()
	grounded := "Revenue is 125,000.50 today (about 125,001) from 1,450.25 liters over the last 7 days."
	if bad := ungroundedNumbers(grounded, c); len(bad) != 0 {
		t.Errorf("grounded numbers rejected: %v", bad)
	}
	// A percentage is never in the mart context, so it must be caught
	// even though "12" appears in the context date and "7" in "last 7 days".
	for _, reply := range []string{
		"Profit fell because volume dropped 12% and costs rose 7% this week.",
		"Revenue was 987654 PKR.",
		"Margin is about 3.5%.",
	} {
		if bad := ungroundedNumbers(reply, c); len(bad) == 0 {
			t.Errorf("invented figures not caught in %q", reply)
		}
	}
	// The score and its denominator are quotable.
	if bad := ungroundedNumbers("Your score is 82/100 with 1 open issue.", c); len(bad) != 0 {
		t.Errorf("score rejected: %v", bad)
	}
}
