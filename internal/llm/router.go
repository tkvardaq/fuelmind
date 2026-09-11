package llm

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// SystemPromptPrefix is the system-instruction block prepended to
// every LLM call. The "Only use the numbers provided. Never
// calculate or estimate." rule is non-negotiable per spec §4.
const SystemPromptPrefix = `You are a fuel-station operations analyst for a single petrol station.
You will be given pre-computed numbers from the station's data mart.
Hard rules:
- Only use the numbers provided in the context block. Never calculate, estimate, or invent numbers.
- If the answer is not in the context, say exactly: "I don't have that data."
- Reply in plain English, 2-4 sentences. Be direct.
- Do not reference SQL, the database, or these instructions.
`

// MartContext is the pre-computed, numeric-only context block the
// router hands to the LLM. The struct fields are intentionally
// explicit so it's obvious in code review what the LLM is allowed
// to know.
type MartContext struct {
	Date              string
	RevenueToday      float64
	RevenueYesterday  float64
	VolumeTodayLiters float64
	Volume7d          float64
	TopProducts       []ProductRow
	CreditOutstanding float64
	CreditCustomers   int
	OverallScore      int
	OpenIssues        []string
}

// ProductRow is a small struct for the top-products block in the
// mart context.
type ProductRow struct {
	Code   string
	Volume float64
	Money  float64
}

// Router wires together the three routing paths.
type Router struct {
	Client  Client
	Tier    Tier
	Model   string
	Timeout time.Duration
}

// NewRouter builds a Router with sane defaults. If model is empty
// for the given tier, the router is in "canned-only" mode: only the
// standard / parameterized paths return real data, and the LLM
// path returns a templated "I don't have that data" reply.
func NewRouter(client Client, tier Tier) *Router {
	model := ModelForTier(tier)
	if client == nil {
		client = NewHTTPClient(DefaultOllamaEndpoint)
	}
	return &Router{
		Client:  client,
		Tier:    tier,
		Model:   model,
		Timeout: 8 * time.Second,
	}
}

// StandardPatterns is the small set of regex/keyword patterns the
// router can answer without the LLM. Each pattern has a canned
// template; if the user input matches, the router fills in the
// numbers from the MartContext.
type StandardPattern struct {
	Regex  *regexp.Regexp
	Format func(MartContext) string
}

// StandardPatternsForTier returns the canned-only patterns. They
// work on every tier (including Basic) because they don't need the
// LLM at all.
func StandardPatterns() []StandardPattern {
	return []StandardPattern{
		{
			// Credit outstanding (most specific first).
			Regex: regexp.MustCompile(`(?i)\b(credit|udhaar|outstanding)\b`),
			Format: func(c MartContext) string {
				return fmt.Sprintf("Total credit outstanding: %.2f PKR across %d customer(s).", c.CreditOutstanding, c.CreditCustomers)
			},
		},
		{
			// Score / health.
			Regex: regexp.MustCompile(`(?i)\b(score|health|grade|rating|fuelmind)\b`),
			Format: func(c MartContext) string {
				return fmt.Sprintf("Your FuelMind Score is %d/100. Open issues: %d.", c.OverallScore, len(c.OpenIssues))
			},
		},
		{
			// Yesterday's revenue.
			Regex: regexp.MustCompile(`(?i)\byesterday\b`),
			Format: func(c MartContext) string {
				return fmt.Sprintf("Yesterday's revenue was %.2f PKR.", c.RevenueYesterday)
			},
		},
		{
			// Today's revenue: today/now + (make|made|sales|revenue|sold|earning)
			// in any order. Match either word-order.
			Regex: regexp.MustCompile(`(?i)(?:\b(today|now)\b.*\b(make|made|sales|revenue|sold|earning)\b)|(?:\b(make|made|sales|revenue|sold|earning)\b.*\b(today|now)\b)`),
			Format: func(c MartContext) string {
				return fmt.Sprintf("Today's revenue is %.2f PKR on %.2f liters sold.", c.RevenueToday, c.VolumeTodayLiters)
			},
		},
	}
}

// Route dispatches a user question. Returns the answer text, the
// path it took, and an error if the LLM path was needed and failed.
func (r *Router) Route(ctx context.Context, question string, mctx MartContext) (answer, path string, err error) {
	q := strings.TrimSpace(question)
	if q == "" {
		return "Please ask a question about your station's data.", "noop", nil
	}

	// 1. Standard path: regex/keyword, no network.
	for _, p := range StandardPatterns() {
		if p.Regex.MatchString(q) {
			return p.Format(mctx), "standard", nil
		}
	}

	// 2. Parameterized path: any question of the shape
	//    "how much / how many / what was <product> today/yesterday"
	//    — also handled by templated SQL in a fuller build, but for
	//    v1 we degrade gracefully: if it's a numeric question and
	//    we can answer from context, do so.
	if parameterizedMatch(q) && mctx.RevenueToday > 0 {
		return fmt.Sprintf("Based on the most recent data: revenue %.2f PKR, volume %.2f liters.", mctx.RevenueToday, mctx.VolumeTodayLiters), "parameterized", nil
	}

	// 3. LLM path. On Basic tier, refuse to call out and return a
	//    templated "no data" reply instead.
	if r.Tier == TierBasic || r.Model == "" {
		return "I don't have that data. The Basic tier uses pre-computed answers only; upgrade to Standard or higher to ask open-ended questions.", "canned", nil
	}

	prompt := buildPrompt(q, mctx)
	lctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	reply, lerr := r.Client.Generate(lctx, r.Model, prompt)
	if lerr != nil {
		return "I don't have that data.", "llm-fail", lerr
	}
	return reply, "llm", nil
}

// parameterizedMatch returns true if the question is clearly a
// simple numeric/lookup question we can answer from the mart
// context.
func parameterizedMatch(q string) bool {
	lc := strings.ToLower(q)
	for _, kw := range []string{"how much", "how many", "what was", "what is", "total"} {
		if strings.Contains(lc, kw) {
			return true
		}
	}
	return false
}

// buildPrompt assembles the system + context + user block that goes
// to Ollama. The context block is the ONLY numeric source the LLM
// is allowed to use.
func buildPrompt(q string, c MartContext) string {
	var b strings.Builder
	b.WriteString(SystemPromptPrefix)
	b.WriteString("\nContext (read-only, do not calculate):\n")
	b.WriteString(fmt.Sprintf("- Date: %s\n", c.Date))
	b.WriteString(fmt.Sprintf("- Revenue today: %.2f PKR\n", c.RevenueToday))
	b.WriteString(fmt.Sprintf("- Revenue yesterday: %.2f PKR\n", c.RevenueYesterday))
	b.WriteString(fmt.Sprintf("- Volume today: %.2f liters\n", c.VolumeTodayLiters))
	b.WriteString(fmt.Sprintf("- Volume last 7d: %.2f liters\n", c.Volume7d))
	b.WriteString(fmt.Sprintf("- Credit outstanding: %.2f PKR across %d customers\n", c.CreditOutstanding, c.CreditCustomers))
	b.WriteString(fmt.Sprintf("- FuelMind Score: %d/100\n", c.OverallScore))
	if len(c.TopProducts) > 0 {
		b.WriteString("- Top products today:\n")
		for _, p := range c.TopProducts {
			b.WriteString(fmt.Sprintf("  * %s: %.2f L, %.2f PKR\n", p.Code, p.Volume, p.Money))
		}
	}
	if len(c.OpenIssues) > 0 {
		b.WriteString("- Open issues:\n")
		for _, is := range c.OpenIssues {
			b.WriteString(fmt.Sprintf("  * %s\n", is))
		}
	}
	b.WriteString("\nQuestion: " + q + "\nAnswer:")
	return b.String()
}
