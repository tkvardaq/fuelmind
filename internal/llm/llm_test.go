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
	if !strings.Contains(ans, "125000.50") {
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
	if !strings.Contains(ans, "50000.00") {
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
	fc := &fakeClient{reply: "Profit fell because volume dropped 12% week-over-week."}
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
