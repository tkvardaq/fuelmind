package messaging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/fuelmind/fuelmind/internal/storage"
)

// Agent composes messages and drains the outbox. One tick does both, so
// a message queued by an alert at 21:00:05 goes out on the same pass.
type Agent struct {
	store     *storage.Storage
	mart      ContextProvider
	sender    Sender
	log       *slog.Logger
	stationID string
	tick      time.Duration
	now       func() time.Time
}

// Config wires an Agent.
type Config struct {
	Store     *storage.Storage
	Context   ContextProvider
	Sender    Sender
	Logger    *slog.Logger
	StationID string
	// Tick is how often the agent looks for something to say. One minute
	// is enough: the summary hour has minute resolution and alerts are
	// deduped per day, so a faster tick would only re-read the mart.
	Tick time.Duration
	// Now is overridable so tests can stand at any hour of any day.
	Now func() time.Time
}

// New builds an Agent.
func New(cfg Config) *Agent {
	a := &Agent{
		store: cfg.Store, mart: cfg.Context, sender: cfg.Sender,
		log: orDefault(cfg.Logger), stationID: cfg.StationID,
		tick: cfg.Tick, now: cfg.Now,
	}
	if a.tick <= 0 {
		a.tick = time.Minute
	}
	if a.now == nil {
		a.now = time.Now
	}
	return a
}

// Sender returns the channel in use, which may be nil.
func (a *Agent) Sender() Sender { return a.sender }

// Run ticks until ctx is done.
func (a *Agent) Run(ctx context.Context) {
	ticker := time.NewTicker(a.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := a.Once(ctx); err != nil && !errors.Is(err, context.Canceled) {
				a.log.Warn("messaging tick", "err", err)
			}
		}
	}
}

// Once composes anything due and then sends whatever is queued.
func (a *Agent) Once(ctx context.Context) error {
	settings := LoadSettings(ctx, a.store)
	if !settings.Enabled || settings.Recipient == "" {
		return nil
	}
	if err := a.compose(ctx, settings); err != nil {
		a.log.Warn("compose messages", "err", err)
	}
	_, _, err := a.Drain(ctx)
	return err
}

// compose queues the summary and any alert that has become true today.
func (a *Agent) compose(ctx context.Context, s Settings) error {
	now := a.now()
	today := now.Format("2006-01-02")
	mctx, err := a.mart.Context(ctx)
	if err != nil {
		return fmt.Errorf("mart context: %w", err)
	}

	if now.Hour() >= s.SummaryHour {
		a.queue(ctx, s, KindDailySummary, KindDailySummary+":"+today, ComposeDailySummary(mctx))
	}
	if !s.Alerts {
		return nil
	}
	if mctx.OverallScore > 0 && mctx.OverallScore < scoreAlertBelow {
		a.queue(ctx, s, KindScoreDrop, KindScoreDrop+":"+today, ComposeScoreDrop(mctx))
	}
	if overdue, err := a.overdueCredit(ctx); err != nil {
		a.log.Warn("credit alert", "err", err)
	} else if len(overdue) > 0 {
		a.queue(ctx, s, KindCreditOverdue, KindCreditOverdue+":"+today,
			ComposeCreditOverdue(overdue, mctx.CreditOutstanding))
	}
	last, err := a.store.LastPosIngestionAt(ctx)
	if err != nil {
		a.log.Warn("ingest alert", "err", err)
	} else if !last.IsZero() && now.Sub(last) >= ingestStallAfter {
		// Keyed by the day: a station that is down all day says so once,
		// then again after a night's sleep — not every minute. Widen this
		// key only if you also intend to raise the alert frequency.
		key := fmt.Sprintf("%s:%s", KindIngestStalled, now.Format("2006-01-02"))
		a.queue(ctx, s, KindIngestStalled, key, ComposeIngestStalled(last, now))
	}
	return nil
}

// overdueCredit returns the customers whose balance has not moved in
// creditOverdueDays.
func (a *Agent) overdueCredit(ctx context.Context) ([]storage.CreditRow, error) {
	rows, err := a.store.RecentCredit(ctx, 100)
	if err != nil {
		return nil, err
	}
	var out []storage.CreditRow
	for _, r := range rows {
		if r.DaysOverdue >= creditOverdueDays && r.OutstandingAmount > 0 {
			out = append(out, r)
		}
	}
	return out, nil
}

// queue adds one message, treating "already queued today" as success.
func (a *Agent) queue(ctx context.Context, s Settings, kind, dedup, body string) {
	_, err := a.store.QueueMessage(ctx, storage.Message{
		Kind: kind, DedupKey: dedup, Channel: a.channelName(),
		Recipient: s.Recipient, Body: body,
	})
	switch {
	case errors.Is(err, storage.ErrDuplicateMessage):
	case err != nil:
		a.log.Warn("queue message", "kind", kind, "err", err)
	default:
		a.log.Info("message queued", "kind", kind)
	}
}

// QueueTest queues the "does this reach you?" message and sends it now.
func (a *Agent) QueueTest(ctx context.Context) error {
	s := LoadSettings(ctx, a.store)
	if s.Recipient == "" {
		return ErrNoRecipient
	}
	now := a.now()
	if _, err := a.store.QueueMessage(ctx, storage.Message{
		Kind:     KindTest,
		DedupKey: KindTest + ":" + uniqueSuffix(now),
		Channel:  a.channelName(), Recipient: s.Recipient,
		Body: ComposeTest(a.stationID, now),
	}); err != nil {
		return err
	}
	_, _, err := a.Drain(ctx)
	return err
}

// Drain sends what is queued. A channel that is not ready is not an
// error: the messages stay queued and go out when pairing completes.
func (a *Agent) Drain(ctx context.Context) (sent, failed int, err error) {
	if a.sender == nil || !a.sender.Ready() {
		return 0, 0, nil
	}
	pending, err := a.store.PendingMessages(ctx, 20)
	if err != nil {
		return 0, 0, err
	}
	for _, m := range pending {
		sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		sendErr := a.sender.Send(sendCtx, m.Recipient, m.Body)
		cancel()
		if sendErr != nil {
			failed++
			a.log.Warn("send message", "kind", m.Kind, "attempt", m.Attempts+1, "err", sendErr)
			if err := a.store.MarkMessageAttempt(ctx, m.ID, sendErr, maxSendAttempts); err != nil {
				a.log.Warn("record send failure", "err", err)
			}
			continue
		}
		sent++
		a.log.Info("message sent", "kind", m.Kind, "channel", m.Channel)
		if err := a.store.MarkMessageSent(ctx, m.ID); err != nil {
			a.log.Warn("record send", "err", err)
		}
	}
	return sent, failed, nil
}

// uniqueSuffix makes a test message's dedup key one of a kind. A test is
// the one message the owner may legitimately send twice in a row, so it
// must never collide with the one before it — and on Windows the clock
// alone is too coarse to guarantee that.
func uniqueSuffix(now time.Time) string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(now.UnixNano(), 10)
	}
	return fmt.Sprintf("%d-%s", now.Unix(), hex.EncodeToString(b[:]))
}

func (a *Agent) channelName() string {
	if a.sender == nil {
		return "whatsapp"
	}
	return a.sender.Name()
}
