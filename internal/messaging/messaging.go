// Package messaging lets the station start the conversation.
//
// Everything else in FuelMind waits to be asked: the owner opens the
// dashboard, or sends a question over the relay. But the things that
// actually cost a station money — a score that slipped, a credit customer
// who has not paid in a month, a POS that stopped exporting — are exactly
// the things nobody thinks to go and look for. So the station composes a
// short closing summary every evening, and speaks up when one of those
// conditions is true.
//
// Two rules hold the feature honest:
//
//  1. Every figure in a message comes from the same mart context the
//     dashboard and the Ask box use. Nothing is recomputed here, so a
//     message can never disagree with the page.
//  2. Nothing is sent unless the owner turned it on and gave a number.
//     Off is the default, and the outbox is visible in the dashboard, so
//     the owner can always see exactly what left their station.
package messaging

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/fuelmind/fuelmind/internal/llm"
	"github.com/fuelmind/fuelmind/internal/storage"
)

// local_config keys.
const (
	ConfigKeyEnabled     = "messaging_enabled"      // "on" / "off"
	ConfigKeyRecipient   = "messaging_recipient"    // the owner's phone number
	ConfigKeySummaryHour = "messaging_summary_hour" // 0-23, station-local
	ConfigKeyAlerts      = "messaging_alerts"       // "on" / "off"
)

// Message kinds. The kind is half of a message's dedup key, so adding a
// kind here is all it takes to make a new alert send at most once a day.
const (
	KindDailySummary  = "daily_summary"
	KindScoreDrop     = "score_drop"
	KindCreditOverdue = "credit_overdue"
	KindIngestStalled = "ingest_stalled"
	KindTest          = "test"
)

// Thresholds for the alerts. They are deliberately conservative: an alert
// the owner learns to ignore is worse than no alert.
const (
	scoreAlertBelow   = 70            // out of 100
	creditOverdueDays = 30            // days since the customer's last credit sale
	ingestStallAfter  = 6 * time.Hour // no POS export for this long
	maxSendAttempts   = 5
)

// Sender delivers a message. WhatsApp is the implementation that ships;
// the interface is what lets the agent be tested without a network, and
// what a second channel would plug into.
type Sender interface {
	// Name is what the dashboard calls this channel, e.g. "whatsapp".
	Name() string
	// Ready reports whether the channel can send right now (paired,
	// connected). A channel that is not ready leaves messages queued
	// rather than burning attempts on them.
	Ready() bool
	// Send delivers body to a phone number in international format.
	Send(ctx context.Context, to, body string) error
}

// ContextProvider hands over the pre-computed figures a message may use.
// *ask.Service implements it; tests pass a stub.
type ContextProvider interface {
	Context(ctx context.Context) (llm.MartContext, error)
}

// Settings is the owner's messaging configuration.
type Settings struct {
	Enabled     bool
	Recipient   string
	SummaryHour int
	Alerts      bool
}

// DefaultSummaryHour is 9pm — after a typical station's busy evening, and
// early enough that the owner still reads it the same day.
const DefaultSummaryHour = 21

// LoadSettings reads the owner's choices from local_config.
func LoadSettings(ctx context.Context, store *storage.Storage) Settings {
	hour, err := strconv.Atoi(store.LocalConfigValue(ctx, ConfigKeySummaryHour, ""))
	if err != nil || hour < 0 || hour > 23 {
		hour = DefaultSummaryHour
	}
	return Settings{
		Enabled:     strings.EqualFold(store.LocalConfigValue(ctx, ConfigKeyEnabled, "off"), "on"),
		Recipient:   store.LocalConfigValue(ctx, ConfigKeyRecipient, ""),
		SummaryHour: hour,
		Alerts:      !strings.EqualFold(store.LocalConfigValue(ctx, ConfigKeyAlerts, "on"), "off"),
	}
}

// Save writes the owner's choices back.
func (s Settings) Save(ctx context.Context, store *storage.Storage) error {
	on := func(b bool) string {
		if b {
			return "on"
		}
		return "off"
	}
	if err := store.SetLocalConfig(ctx, ConfigKeyEnabled, on(s.Enabled), "", false); err != nil {
		return err
	}
	if err := store.SetLocalConfig(ctx, ConfigKeyRecipient, NormalizeNumber(s.Recipient), "", false); err != nil {
		return err
	}
	if err := store.SetLocalConfig(ctx, ConfigKeySummaryHour, strconv.Itoa(s.SummaryHour), "", false); err != nil {
		return err
	}
	return store.SetLocalConfig(ctx, ConfigKeyAlerts, on(s.Alerts), "", false)
}

// ErrNoRecipient means messaging is on but no phone number was given.
var ErrNoRecipient = errors.New("messaging: no recipient number configured")

// NormalizeNumber keeps only the digits of a phone number, which is what
// WhatsApp addresses use. "+92 300 123 4567" becomes "923001234567".
func NormalizeNumber(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// logger returns a usable logger even when none was configured.
func orDefault(l *slog.Logger) *slog.Logger {
	if l == nil {
		return slog.Default()
	}
	return l
}
