package llm

import (
	"context"
	"strings"
	"testing"
)

func marginContext() MartContext {
	c := sampleContext()
	c.MarginToday = 18500.25
	c.CostToday = 106500.25
	c.MarginPctToday = 14.8
	c.HaveCostToday = true
	c.Margin7d = 121000.00
	c.Cost7d = 780000.00
	c.Revenue7dCosted = 901000.00
	c.CostedDays7d = 5
	c.HaveCost7d = true
	c.LatestCostPerLiter = 250.30
	c.LatestCostProduct = "DIESEL"
	c.LatestCostFrom = "2026-09-07"
	return c
}

// Margin is the question an owner asks first, and until now it was on
// the "no feed" list — so asking it returned "I don't have that data"
// even though the Margin page was showing the answer.
func TestMarginIsAnsweredWhenCostIsKnown(t *testing.T) {
	r := NewRouter(&fakeClient{}, TierStandard)
	for _, q := range []string{
		"what is our margin today?",
		"how much profit did we make today?",
		"margin",
	} {
		ans, path, err := r.Route(context.Background(), q, marginContext())
		if err != nil {
			t.Fatalf("Route(%q): %v", q, err)
		}
		if path != "standard" {
			t.Errorf("Route(%q) path = %q, want standard (no model needed)", q, path)
		}
		if !strings.Contains(ans, "18,500") {
			t.Errorf("Route(%q) = %q, want today's margin in it", q, ans)
		}
		// It must be clear this is fuel margin, not net profit — an owner
		// who reads it as take-home is being misled.
		if !strings.Contains(ans, "fuel margin only") {
			t.Errorf("Route(%q) = %q, want it to say this is fuel margin only", q, ans)
		}
	}
}

func TestWeeklyMarginIsAnswered(t *testing.T) {
	r := NewRouter(&fakeClient{}, TierStandard)
	ans, path, err := r.Route(context.Background(), "what was our margin this week?", marginContext())
	if err != nil {
		t.Fatal(err)
	}
	if path != "standard" {
		t.Errorf("path = %q, want standard", path)
	}
	if !strings.Contains(ans, "121,000") {
		t.Errorf("answer = %q, want the margin of the costed days", ans)
	}
	// The revenue quoted must be the revenue of the days that have a
	// cost, not the whole week's — otherwise the margin reads far
	// healthier than it is.
	if !strings.Contains(ans, "901,000") {
		t.Errorf("answer = %q, want the revenue of the costed days", ans)
	}
	if strings.Contains(ans, "1,103,936") {
		t.Errorf("answer quoted a full week of revenue against part of a week of cost: %q", ans)
	}
	if !strings.Contains(ans, "5 of the last 7 days") {
		t.Errorf("answer = %q, want it to say how much of the week is covered", ans)
	}
}

// The worst version of this bug: one day priced out of seven, and the
// answer sets the whole week's revenue against that one day's cost.
func TestWeeklyMarginSaysHowLittleItCovers(t *testing.T) {
	c := sampleContext()
	c.Revenue7d = 1103936.25
	c.Margin7d = 35722.12
	c.Cost7d = 384780.02
	c.Revenue7dCosted = 420502.14
	c.CostedDays7d = 1
	c.HaveCost7d = true

	r := NewRouter(&fakeClient{}, TierStandard)
	ans, _, err := r.Route(context.Background(), "margin this week?", c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ans, "1,103,936") {
		t.Errorf("a week of revenue was set against one day of cost: %q", ans)
	}
	if !strings.Contains(ans, "the 1 day of the last 7") {
		t.Errorf("answer = %q, want it to say only one day is covered", ans)
	}
}

// With no purchase price recorded, the one answer that must never be
// given is "your margin is <all of revenue>". It has to say what is
// missing and how to fix it.
func TestMarginWithoutACostSaysWhatIsMissing(t *testing.T) {
	r := NewRouter(&fakeClient{}, TierStandard)
	ans, path, err := r.Route(context.Background(), "what is our margin today?", sampleContext())
	if err != nil {
		t.Fatal(err)
	}
	if path != "standard" {
		t.Errorf("path = %q, want standard", path)
	}
	if !strings.Contains(ans, "no purchase price") {
		t.Errorf("answer = %q, want it to name the missing purchase price", ans)
	}
	if !strings.Contains(ans, "Settings") {
		t.Errorf("answer = %q, want it to say where to enter the price", ans)
	}
	// Revenue must not be passed off as margin.
	if strings.Contains(ans, "125,000") {
		t.Errorf("revenue was reported as margin: %q", ans)
	}
}

func TestPurchasePriceIsAnswered(t *testing.T) {
	r := NewRouter(&fakeClient{}, TierStandard)
	for _, q := range []string{
		"what is our purchase price?",
		"what did we pay per litre?",
		"cost per litre",
		"what do we buy diesel for per liter?",
	} {
		ans, path, err := r.Route(context.Background(), q, marginContext())
		if err != nil {
			t.Fatalf("Route(%q): %v", q, err)
		}
		if path != "standard" {
			t.Errorf("Route(%q) path = %q, want standard", q, path)
		}
		if !strings.Contains(ans, "250.30") {
			t.Errorf("Route(%q) = %q, want the recorded cost per litre", q, ans)
		}
	}
}

// "help" has to work, from the dashboard and from WhatsApp. An owner who
// does not know what to ask is the most common way this feels broken.
func TestHelpListsWhatCanBeAsked(t *testing.T) {
	r := NewRouter(&fakeClient{}, TierStandard)
	for _, q := range []string{"help", "Help", "?", "what can you do", "menu"} {
		ans, path, err := r.Route(context.Background(), q, sampleContext())
		if err != nil {
			t.Fatalf("Route(%q): %v", q, err)
		}
		if path != "help" {
			t.Errorf("Route(%q) path = %q, want help", q, path)
		}
		if !strings.Contains(ans, "how much did we sell today?") {
			t.Errorf("Route(%q) did not list the questions: %q", q, ans)
		}
	}
}

// A question about something with no feed should say so specifically,
// rather than give the same blanket refusal as a model outage.
func TestNoFeedSaysWhichWayItIsMissing(t *testing.T) {
	r := NewRouter(&fakeClient{}, TierStandard)
	ans, path, err := r.Route(context.Background(), "how much diesel is in the tank?", sampleContext())
	if err != nil {
		t.Fatal(err)
	}
	if path != "no-feed" {
		t.Errorf("path = %q, want no-feed", path)
	}
	if !strings.Contains(ans, "feed for that yet") {
		t.Errorf("answer = %q, want it to explain there is no feed", ans)
	}
}

// A "why" question wants reasoning, not a figure. Answering "today's
// revenue is X" to "why did takings drop?" answers a different question.
func TestWhyQuestionsGoToTheModel(t *testing.T) {
	fc := &fakeClient{reply: "Takings are lower because fewer sales were recorded today."}
	r := NewRouter(fc, TierStandard)
	ans, path, err := r.Route(context.Background(), "why did our sales drop today?", sampleContext())
	if err != nil {
		t.Fatal(err)
	}
	if path != "llm" {
		t.Errorf("path = %q, want llm (a why question needs reasoning)", path)
	}
	if ans != fc.reply {
		t.Errorf("answer = %q, want the model's reply", ans)
	}
}

// The same question with no model must not silently fall back to a
// figure — it should say a model is needed.
func TestWhyQuestionsWithoutAModelSaySo(t *testing.T) {
	r := NewRouter(&fakeClient{}, TierBasic)
	ans, path, err := r.Route(context.Background(), "why did our sales drop today?", sampleContext())
	if err != nil {
		t.Fatal(err)
	}
	if path != "canned" {
		t.Errorf("path = %q, want canned", path)
	}
	if !strings.Contains(ans, "no AI model is connected") {
		t.Errorf("answer = %q, want it to say a model is needed", ans)
	}
}

// The grounding check must not reject the margin figures the router was
// given, or a correct answer would be thrown away.
func TestMarginNumbersAreGrounded(t *testing.T) {
	c := marginContext()
	reply := "Today's fuel margin was 18,500.25 PKR on 106,500.25 PKR of fuel cost, which is 14.8%."
	if bad := ungroundedNumbers(reply, c); len(bad) > 0 {
		t.Errorf("margin figures were treated as invented: %v", bad)
	}
}
