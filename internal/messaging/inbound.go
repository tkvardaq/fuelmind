package messaging

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/fuelmind/fuelmind/internal/phone"
	"github.com/fuelmind/fuelmind/internal/storage"
)

// ConfigKeyAnswerQuestions is the owner's switch for answering questions
// that arrive over WhatsApp. It is on once a phone is paired: an owner
// who replies to the evening summary with "how much today?" expects an
// answer, and getting silence is the surprising outcome.
const ConfigKeyAnswerQuestions = "messaging_answer_questions"

// Answerer produces the reply to a question. *ask.Service implements it.
type Answerer interface {
	Answer(ctx context.Context, question string) (answer, path string, err error)
}

// Inbound answers questions the station receives over its own WhatsApp.
//
// This is the path that needs no control plane, no Twilio account and no
// webhook: the owner messages the station's own WhatsApp number, and the
// shop PC answers from its own data. The figures never leave the two
// phones involved.
//
// Who may ask is the whole security model. The station answers the
// number the owner registered for messages and nobody else, because the
// answer is the day's takings.
type Inbound struct {
	store    *storage.Storage
	answerer Answerer
	log      *slog.Logger

	mu     sync.Mutex
	recent map[string]*askWindow
}

type askWindow struct {
	start time.Time
	n     int
}

// Rate limit for inbound questions. An owner asks a handful; anything
// past this is a loop or a mistake, and every reply is a message sent
// from the station's own WhatsApp account, which is exactly the kind of
// volume that gets an account banned.
const (
	inboundBurst  = 12
	inboundWindow = 5 * time.Minute
)

// NewInbound builds the responder.
func NewInbound(store *storage.Storage, answerer Answerer, logger *slog.Logger) *Inbound {
	if logger == nil {
		logger = slog.Default()
	}
	return &Inbound{store: store, answerer: answerer, log: logger, recent: map[string]*askWindow{}}
}

// AnswerQuestionsEnabled reports the owner's switch.
func AnswerQuestionsEnabled(ctx context.Context, store *storage.Storage) bool {
	return !strings.EqualFold(store.LocalConfigValue(ctx, ConfigKeyAnswerQuestions, "on"), "off")
}

// SetAnswerQuestions turns answering over WhatsApp on or off.
func SetAnswerQuestions(ctx context.Context, store *storage.Storage, on bool) error {
	v := "off"
	if on {
		v = "on"
	}
	return store.SetLocalConfig(ctx, ConfigKeyAnswerQuestions, v, "", false)
}

// Handle answers one incoming message, or returns "" to stay silent.
//
// It is the whatsapp.InboundHandler for this station.
func (in *Inbound) Handle(ctx context.Context, from, text string) string {
	if !AnswerQuestionsEnabled(ctx, in.store) {
		return ""
	}

	// Only the owner's registered number. A stranger gets silence rather
	// than a refusal: replying at all would confirm the number belongs to
	// a FuelMind station, and an automatic reply to an unknown number is
	// how a WhatsApp account gets reported.
	if !in.isOwner(ctx, from) {
		in.log.Warn("whatsapp: a question came from a number that is not the owner's; ignoring",
			"from", phone.Normalize(from))
		in.record(ctx, from, text, "", "ignored-unknown-sender")
		return ""
	}

	if !in.allow(from) {
		in.log.Warn("whatsapp: too many questions from the owner's number in a short time", "from", phone.Normalize(from))
		in.record(ctx, from, text, "", "rate-limited")
		return "That is a lot of questions at once — give me a few minutes."
	}

	answer, path, err := in.answerer.Answer(ctx, text)
	if err != nil {
		in.log.Warn("whatsapp: could not answer a question", "err", err)
	}
	if strings.TrimSpace(answer) == "" {
		answer = "I could not work that out just now."
	}
	in.record(ctx, from, text, answer, path)
	in.log.Info("whatsapp: answered a question", "path", path)
	return answer
}

// isOwner reports whether this number is the one the owner registered.
// Numbers are compared in canonical form, so a number saved as
// "0300 1234567" matches a message from "+92 300 1234567".
func (in *Inbound) isOwner(ctx context.Context, from string) bool {
	registered := in.store.LocalConfigValue(ctx, ConfigKeyRecipient, "")
	if strings.TrimSpace(registered) == "" {
		return false
	}
	return phone.Normalize(registered) == phone.Normalize(from)
}

// allow is a fixed-window rate limit per sender.
func (in *Inbound) allow(from string) bool {
	key := phone.Normalize(from)
	now := time.Now()

	in.mu.Lock()
	defer in.mu.Unlock()
	w, ok := in.recent[key]
	if !ok || now.Sub(w.start) >= inboundWindow {
		in.recent[key] = &askWindow{start: now, n: 1}
		return true
	}
	w.n++
	return w.n <= inboundBurst
}

// record writes the exchange to the station's own log, so the owner can
// see every question that was asked of their station and what was
// answered — including the ones that were refused.
func (in *Inbound) record(ctx context.Context, from, question, answer, path string) {
	if strings.TrimSpace(answer) == "" {
		answer = "(no reply sent)"
	}
	err := in.store.RecordRemoteQuestion(ctx, storage.RemoteQuestion{
		MessageID: fmt.Sprintf("wa-%d", time.Now().UnixNano()),
		AskedBy:   phone.Normalize(from),
		Question:  question,
		Answer:    answer,
		Route:     "whatsapp:" + path,
	})
	if err != nil {
		in.log.Warn("whatsapp: could not log the question", "err", err)
	}
}
