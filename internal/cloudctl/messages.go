package cloudctl

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"strings"
	"time"
)

// Message is one question routed to a station and the answer it sent
// back. Questions arrive from a WhatsApp/SMS webhook or the admin API;
// the station polls for them and answers from its own data.
type Message struct {
	ID         string     `json:"id"`
	StationID  string     `json:"station_id"`
	From       string     `json:"from"`
	Text       string     `json:"text"`
	Channel    string     `json:"channel"`
	ReceivedAt time.Time  `json:"received_at"`
	Answer     string     `json:"answer,omitempty"`
	AnsweredAt *time.Time `json:"answered_at,omitempty"`
}

// Answered reports whether the station has replied.
func (m *Message) Answered() bool { return m.AnsweredAt != nil }

// maxMessageLength caps an inbound question.
const maxMessageLength = 300

// Enqueue adds a question for a station and returns it.
func (s *Server) Enqueue(stationID, from, text, channel string) (*Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.stations[stationID]; !ok {
		return nil, ErrStationNotFound
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, fmt.Errorf("cloudctl: empty question")
	}
	if len(text) > maxMessageLength {
		text = text[:maxMessageLength]
	}
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	msg := &Message{
		ID: hex.EncodeToString(buf), StationID: stationID, From: from,
		Text: text, Channel: channel, ReceivedAt: time.Now(),
	}
	if s.messages == nil {
		s.messages = map[string]*Message{}
	}
	s.messages[msg.ID] = msg
	s.order = append(s.order, msg.ID)
	if len(s.order) > 500 { // keep the queue bounded
		delete(s.messages, s.order[0])
		s.order = s.order[1:]
	}
	return msg, nil
}

// Message returns one message by id.
func (s *Server) Message(id string) (*Message, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.messages[id]
	return m, ok
}

// WaitForAnswer blocks until the station answers the message or the
// timeout passes. It is what a WhatsApp webhook needs: reply in the same
// HTTP request when the station is quick, and tell the sender to expect a
// follow-up when it is not.
func (s *Server) WaitForAnswer(id string, timeout time.Duration) (*Message, bool) {
	deadline := time.Now().Add(timeout)
	for {
		if m, ok := s.Message(id); ok && m.Answered() {
			return m, true
		}
		if time.Now().After(deadline) {
			m, _ := s.Message(id)
			return m, false
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// handlePendingMessages returns GET /v1/messages/pending?station_id=X for
// the station itself.
func (s *Server) handlePendingMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	stationID := r.URL.Query().Get("station_id")
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.stationFor(w, r, stationID); !ok {
		return
	}
	out := struct {
		Messages []*Message `json:"messages"`
	}{}
	for _, id := range s.order {
		m := s.messages[id]
		if m != nil && m.StationID == stationID && !m.Answered() {
			out.Messages = append(out.Messages, m)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleMessageReply accepts POST /v1/messages/{id}/reply from the station.
func (s *Server) handleMessageReply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/messages/"), "/reply")
	var body struct {
		Answer string `json:"answer"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	msg, ok := s.messages[id]
	if !ok {
		http.Error(w, "no such message", http.StatusNotFound)
		return
	}
	if _, ok := s.stationFor(w, r, msg.StationID); !ok {
		return
	}
	now := time.Now()
	msg.Answer, msg.AnsweredAt = body.Answer, &now
	writeJSON(w, http.StatusOK, msg)
}

// handleAdminMessages is the support/admin side:
//
//	POST /v1/admin/messages           {station_id, from, text} -> queue a question
//	GET  /v1/admin/messages?id=...    read one message and its answer
func (s *Server) handleAdminMessages(w http.ResponseWriter, r *http.Request) {
	if !s.adminAuthorized(r) {
		http.Error(w, "admin token required", http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodGet:
		m, ok := s.Message(r.URL.Query().Get("id"))
		if !ok {
			http.Error(w, "no such message", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, m)
	case http.MethodPost:
		var body struct {
			StationID string `json:"station_id"`
			From      string `json:"from"`
			Text      string `json:"text"`
			WaitSecs  int    `json:"wait_seconds"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		msg, err := s.Enqueue(body.StationID, body.From, body.Text, "admin")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if body.WaitSecs > 0 {
			msg, _ = s.WaitForAnswer(msg.ID, time.Duration(body.WaitSecs)*time.Second)
		}
		writeJSON(w, http.StatusOK, msg)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleWhatsAppWebhook accepts an inbound message from a WhatsApp or SMS
// provider and answers it with the station's own reply.
//
// It reads the form fields Twilio and 360dialog-style gateways post
// (From/Body, or from/text) and replies with TwiML, which Twilio sends
// straight back to the sender. Point the provider at:
//
//	POST /v1/whatsapp/webhook?station_id=FM-XXXXXXX&token=<webhook token>
//
// The token is the shared secret configured on the server; without it the
// endpoint is disabled, so nobody can ask a station questions by guessing
// its id.
func (s *Server) handleWhatsAppWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.WebhookToken == "" {
		http.Error(w, "webhook disabled", http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	if subtleCompare(q.Get("token"), s.WebhookToken) != 1 {
		http.Error(w, "bad token", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	stationID := firstNonEmpty(q.Get("station_id"), r.FormValue("station_id"))
	from := firstNonEmpty(r.FormValue("From"), r.FormValue("from"), "unknown")
	text := firstNonEmpty(r.FormValue("Body"), r.FormValue("text"), r.FormValue("message"))

	msg, err := s.Enqueue(stationID, from, text, "whatsapp")
	if err != nil {
		writeTwiML(w, "I could not reach that station just now.")
		return
	}
	answered, ok := s.WaitForAnswer(msg.ID, s.webhookWait())
	if !ok || answered == nil {
		writeTwiML(w, "Your station is offline right now. It will answer as soon as it is back online.")
		return
	}
	writeTwiML(w, answered.Answer)
}

func (s *Server) webhookWait() time.Duration {
	if s.WebhookWait > 0 {
		return s.WebhookWait
	}
	return 25 * time.Second
}

// writeTwiML replies in the XML shape WhatsApp/SMS gateways expect.
func writeTwiML(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	fmt.Fprintf(w, "<?xml version=\"1.0\" encoding=\"UTF-8\"?><Response><Message>%s</Message></Response>",
		html.EscapeString(body))
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
