package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fuelmind/fuelmind/internal/messaging"
	"github.com/fuelmind/fuelmind/internal/storage"
	qrcode "github.com/skip2/go-qrcode"
)

// Pairer is the part of a messaging channel the dashboard drives: link a
// phone, show its state, unlink it. The WhatsApp client implements it;
// the interface keeps the web layer free of whatsmeow.
type Pairer interface {
	Paired() bool
	Ready() bool
	Number() string
	StartPairing(ctx context.Context) error
	PairingCode() (code, pairErr string)
	Unpair(ctx context.Context) error
}

// Messenger is the part of the messaging agent the dashboard drives.
type Messenger interface {
	QueueTest(ctx context.Context) error
	Once(ctx context.Context) error
}

// handleMessages is the message page: what the station has sent, what it
// was asked from outside, and the controls for both.
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	var formError, notice string
	if r.Method == http.MethodPost {
		formError, notice = s.messagesAction(r)
	}

	settings := messaging.LoadSettings(r.Context(), s.store)
	sent, err := s.store.RecentMessages(r.Context(), 50)
	if err != nil {
		s.renderError(w, "messages: outbox", err)
		return
	}
	questions, err := s.store.RecentRemoteQuestions(r.Context(), 25)
	if err != nil {
		s.renderError(w, "messages: questions", err)
		return
	}
	queued, delivered, failed, err := s.store.MessageCounts(r.Context())
	if err != nil {
		s.renderError(w, "messages: counts", err)
		return
	}

	data := map[string]any{
		"LoggedIn":         true,
		"Settings":         settings,
		"Outbox":           sent,
		"Questions":        questions,
		"Queued":           queued,
		"Delivered":        delivered,
		"Failed":           failed,
		"Hours":            hourChoices(),
		"Error":            formError,
		"Notice":           notice,
		"ChannelName":      "WhatsApp",
		"AnswersQuestions": messaging.AnswerQuestionsEnabled(r.Context(), s.store),
	}
	if s.Pairing != nil {
		code, pairErr := s.Pairing.PairingCode()
		data["Configured"] = true
		data["Paired"] = s.Pairing.Paired()
		data["Connected"] = s.Pairing.Ready()
		data["PairedNumber"] = s.Pairing.Number()
		data["PairingCode"] = code
		if pairErr != "" && formError == "" {
			data["Error"] = "Pairing did not finish: " + pairErr
		}
	}
	s.render(w, r, "messages", data)
}

// messagesAction applies one button press and returns what to tell the
// owner.
func (s *Server) messagesAction(r *http.Request) (formError, notice string) {
	switch r.FormValue("action") {
	case "save":
		hour, err := strconv.Atoi(r.FormValue("summary_hour"))
		if err != nil || hour < 0 || hour > 23 {
			hour = messaging.DefaultSummaryHour
		}
		number := messaging.NormalizeNumber(r.FormValue("recipient"))
		enabled := r.FormValue("enabled") == "on"
		if enabled && number == "" {
			return "Enter the phone number that should receive the messages.", ""
		}
		if enabled && len(number) < 10 {
			return "That number looks too short. Use the international form, for example +923001234567.", ""
		}
		set := messaging.Settings{
			Enabled: enabled, Recipient: number,
			SummaryHour: hour, Alerts: r.FormValue("alerts") == "on",
		}
		if err := set.Save(r.Context(), s.store); err != nil {
			return err.Error(), ""
		}
		if !enabled {
			return "", "Messages are off. Nothing will be sent from this station."
		}
		return "", "Saved. The evening summary will go out at " + formatHour(hour) + "."

	case "test":
		if s.Messages == nil {
			return "Messaging is not running on this station.", ""
		}
		if err := s.Messages.QueueTest(r.Context()); err != nil {
			if errors.Is(err, messaging.ErrNoRecipient) {
				return "Add the phone number first, then send a test.", ""
			}
			return "Could not send the test message: " + err.Error(), ""
		}
		return "", "Test message queued. If the phone is paired it is already on its way."

	case "send_now":
		if s.Messages == nil {
			return "Messaging is not running on this station.", ""
		}
		if err := s.Messages.Once(r.Context()); err != nil {
			return "Could not send the queued messages: " + err.Error(), ""
		}
		return "", "Sent everything that was waiting."

	case "answer_questions":
		on := r.FormValue("answer_questions") == "on"
		if err := messaging.SetAnswerQuestions(r.Context(), s.store, on); err != nil {
			return err.Error(), ""
		}
		state := "off"
		if on {
			state = "on"
		}
		s.audit(r, storage.ActionMessagingChanged, "answer_questions",
			"Turned answering questions over WhatsApp "+state)
		if on {
			return "", "Message the station's WhatsApp from your number and it will answer. Send \"help\" to see what it can tell you."
		}
		return "", "The station will no longer answer messages. It can still send you the evening summary."

	case "pair":
		if s.Pairing == nil {
			return "This build has no WhatsApp channel.", ""
		}
		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		defer cancel()
		if err := s.Pairing.StartPairing(ctx); err != nil {
			return "Could not start pairing: " + err.Error(), ""
		}
		return "", "Scan the code below with WhatsApp on your phone: Settings → Linked devices → Link a device."

	case "unpair":
		if s.Pairing == nil {
			return "This build has no WhatsApp channel.", ""
		}
		if err := s.Pairing.Unpair(r.Context()); err != nil {
			return "Could not unlink the phone: " + err.Error(), ""
		}
		return "", "Phone unlinked. Nothing more will be sent until you pair again."
	}
	return "", ""
}

// handlePairingQR draws the current pairing code. It is a PNG rather than
// text because the owner scans it with their phone camera.
func (s *Server) handlePairingQR(w http.ResponseWriter, r *http.Request) {
	if s.Pairing == nil {
		http.NotFound(w, r)
		return
	}
	code, _ := s.Pairing.PairingCode()
	if code == "" {
		http.Error(w, "no pairing code right now", http.StatusNotFound)
		return
	}
	png, err := qrcode.Encode(code, qrcode.Medium, 320)
	if err != nil {
		s.renderError(w, "messages: qr", err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(png)
}

// hourChoices lists the hours the owner can pick for the summary.
func hourChoices() []map[string]any {
	out := make([]map[string]any, 0, 24)
	for h := 0; h < 24; h++ {
		out = append(out, map[string]any{"Value": h, "Label": formatHour(h)})
	}
	return out
}

// formatHour renders 21 as "9:00 pm".
func formatHour(h int) string {
	suffix := "am"
	display := h
	switch {
	case h == 0:
		display = 12
	case h == 12:
		suffix = "pm"
	case h > 12:
		display, suffix = h-12, "pm"
	}
	return strconv.Itoa(display) + ":00 " + suffix
}

// messageKindLabel turns a stored kind into something the owner reads.
func messageKindLabel(kind any) string {
	switch strings.ToLower(strings.TrimSpace(fmt.Sprint(kind))) {
	case messaging.KindDailySummary:
		return "Evening summary"
	case messaging.KindScoreDrop:
		return "Score alert"
	case messaging.KindCreditOverdue:
		return "Credit alert"
	case messaging.KindIngestStalled:
		return "No data alert"
	case messaging.KindTest:
		return "Test"
	default:
		return fmt.Sprint(kind)
	}
}
