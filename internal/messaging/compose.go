package messaging

import (
	"fmt"
	"strings"
	"time"

	"github.com/fuelmind/fuelmind/internal/format"
	"github.com/fuelmind/fuelmind/internal/llm"
	"github.com/fuelmind/fuelmind/internal/storage"
)

// ComposeDailySummary writes the evening message. It is deliberately a
// list of figures rather than prose: the owner reads it on a phone, often
// while doing something else, and every line is a number they already
// know how to read from the dashboard.
func ComposeDailySummary(m llm.MartContext) string {
	var b strings.Builder
	fmt.Fprintf(&b, "FuelMind · %s\n", format.Date(m.Date))
	fmt.Fprintf(&b, "Sales today: %s\n", format.Money(m.RevenueToday))
	fmt.Fprintf(&b, "Volume: %s across %d sale(s)\n", format.Liters(m.VolumeTodayLiters), m.TransactionsToday)
	fmt.Fprintf(&b, "Yesterday: %s%s\n", format.Money(m.RevenueYesterday), changeNote(m.RevenueToday, m.RevenueYesterday))
	if m.CreditOutstanding > 0 {
		fmt.Fprintf(&b, "Credit outstanding: %s from %d customer(s)\n", format.Money(m.CreditOutstanding), m.CreditCustomers)
	}
	if m.OverallScore > 0 {
		fmt.Fprintf(&b, "Station score: %d/100\n", m.OverallScore)
	}
	if len(m.OpenIssues) > 0 {
		b.WriteString("Needs a look: " + m.OpenIssues[0] + "\n")
	}
	b.WriteString("\nReply to this number to ask anything about today.")
	return b.String()
}

// changeNote describes today against yesterday without inventing a
// figure: the percentage is derived from two numbers already in the
// message, and is omitted when yesterday was zero.
func changeNote(today, yesterday float64) string {
	if yesterday <= 0 {
		return ""
	}
	pct := (today - yesterday) / yesterday * 100
	switch {
	case pct >= 1:
		return fmt.Sprintf(" (today is %s%% higher)", format.Num(pct, 0))
	case pct <= -1:
		return fmt.Sprintf(" (today is %s%% lower)", format.Num(-pct, 0))
	default:
		return " (about the same)"
	}
}

// ComposeScoreDrop warns that the station score fell below the threshold.
func ComposeScoreDrop(m llm.MartContext) string {
	var b strings.Builder
	fmt.Fprintf(&b, "FuelMind alert · station score %d/100\n", m.OverallScore)
	fmt.Fprintf(&b, "As of %s, your score is below %d.\n", format.Date(m.ScoreDate), scoreAlertBelow)
	if len(m.OpenIssues) > 0 {
		b.WriteString("\nWhat pulled it down:\n")
		for _, issue := range m.OpenIssues {
			b.WriteString("· " + issue + "\n")
		}
	}
	b.WriteString("\nOpen the dashboard for the detail.")
	return b.String()
}

// ComposeCreditOverdue lists the customers who have not bought — or paid —
// in creditOverdueDays. Only the largest few are named; a message the
// owner has to scroll is a message they will not read.
func ComposeCreditOverdue(rows []storage.CreditRow, total float64) string {
	var b strings.Builder
	b.WriteString("FuelMind alert · credit not moving\n")
	fmt.Fprintf(&b, "%d customer(s) have a balance untouched for %d days or more.\n\n",
		len(rows), creditOverdueDays)
	for i, r := range rows {
		if i == 5 {
			fmt.Fprintf(&b, "…and %d more.\n", len(rows)-i)
			break
		}
		fmt.Fprintf(&b, "%s · %s · %d days\n", r.CustomerPhone, format.Money(r.OutstandingAmount), r.DaysOverdue)
	}
	if total > 0 {
		fmt.Fprintf(&b, "\nTotal outstanding: %s", format.Money(total))
	}
	return b.String()
}

// ComposeIngestStalled warns that the POS has gone quiet. This is the
// message that matters most: every other figure silently goes stale when
// the export stops, and the dashboard would keep showing yesterday's
// numbers as if they were today's.
func ComposeIngestStalled(last time.Time, now time.Time) string {
	var b strings.Builder
	b.WriteString("FuelMind alert · no sales data coming in\n")
	if last.IsZero() {
		b.WriteString("This station has not received a single POS export yet.\n")
	} else {
		fmt.Fprintf(&b, "The last POS export arrived %s (%s).\n",
			last.Local().Format("Mon 2 Jan 15:04"), roughAge(now.Sub(last)))
	}
	b.WriteString("\nToday's figures on the dashboard are incomplete until it resumes. " +
		"Check that the POS is still writing its export file.")
	return b.String()
}

// roughAge says "7 hours ago" rather than "6h58m12s".
func roughAge(d time.Duration) string {
	switch {
	case d < 2*time.Hour:
		return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
}

// ComposeTest is what the Send test message button sends. It proves the
// whole path — pairing, the outbox, delivery — with a message the owner
// cannot mistake for a real alert.
func ComposeTest(stationID string, now time.Time) string {
	id := stationID
	if id == "" {
		id = "this station"
	}
	return fmt.Sprintf("FuelMind test message from %s, sent %s.\n"+
		"If you are reading this, alerts and the evening summary will reach you here.",
		id, now.Format("Mon 2 Jan 15:04"))
}
