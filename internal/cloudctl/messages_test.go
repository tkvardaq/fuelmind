package cloudctl

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func msgServer(t *testing.T) (*Server, string) {
	t.Helper()
	s, u := testServer(t)
	s.AdminToken = "admin-tok"
	s.WebhookToken = "hook-tok"
	s.WebhookWait = 2 * time.Second
	// The owner's number is what proves an inbound message may see this
	// station's figures.
	s.mu.Lock()
	s.stations["FM-TEST1"].OwnerNumbers = []string{"+923001234567"}
	s.mu.Unlock()
	return s, u
}

func TestStationPollsAndAnswers(t *testing.T) {
	s, u := msgServer(t)
	msg, err := s.Enqueue("FM-TEST1", "+923001234567", "how much did we sell today?", "whatsapp")
	if err != nil {
		t.Fatal(err)
	}

	// Another station's key must not see it.
	s.AddStation(&Station{StationID: "FM-OTHER", APIKey: "other", Tier: "private", Status: "active", ValidFrom: time.Now()})
	resp := do(t, "GET", u+"/v1/messages/pending?station_id=FM-TEST1", "other", "")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("another station read the queue: %d", resp.StatusCode)
	}

	resp = do(t, "GET", u+"/v1/messages/pending?station_id=FM-TEST1", "abc123", "")
	var pending struct{ Messages []Message }
	if err := json.NewDecoder(resp.Body).Decode(&pending); err != nil {
		t.Fatal(err)
	}
	if len(pending.Messages) != 1 || pending.Messages[0].Text != "how much did we sell today?" {
		t.Fatalf("pending = %+v", pending.Messages)
	}

	do(t, "POST", u+"/v1/messages/"+msg.ID+"/reply", "abc123", `{"answer":"PKR 90,745 today."}`)
	got, _ := s.Message(msg.ID)
	if !got.Answered() || got.Answer != "PKR 90,745 today." {
		t.Errorf("answer not stored: %+v", got)
	}

	// Answered messages leave the queue.
	resp = do(t, "GET", u+"/v1/messages/pending?station_id=FM-TEST1", "abc123", "")
	pending.Messages = nil
	_ = json.NewDecoder(resp.Body).Decode(&pending)
	if len(pending.Messages) != 0 {
		t.Errorf("answered message still pending: %+v", pending.Messages)
	}
}

func TestQueueingNeedsTheAdminToken(t *testing.T) {
	_, u := msgServer(t)
	body := `{"station_id":"FM-TEST1","from":"support","text":"score?"}`
	if r := do(t, "POST", u+"/v1/admin/messages", "", body); r.StatusCode != 401 {
		t.Errorf("unauthenticated queueing = %d, want 401", r.StatusCode)
	}
	if r := do(t, "POST", u+"/v1/admin/messages", "admin-tok", body); r.StatusCode != 200 {
		t.Errorf("admin queueing = %d, want 200", r.StatusCode)
	}
}

func TestWhatsAppWebhook(t *testing.T) {
	s, u := msgServer(t)

	// A station answers whatever arrives, shortly after it arrives.
	go func() {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			s.mu.Lock()
			var id string
			for _, mid := range s.order {
				if m := s.messages[mid]; m != nil && !m.Answered() {
					id = mid
				}
			}
			s.mu.Unlock()
			if id != "" {
				do2(t, "POST", u+"/v1/messages/"+id+"/reply", "abc123", `{"answer":"Today's revenue is PKR 90,745.00."}`)
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()

	form := url.Values{"From": {"whatsapp:+923001234567"}, "Body": {"how much did we sell today?"}}
	resp, err := http.Post(u+"/v1/whatsapp/webhook?station_id=FM-TEST1&token=hook-tok",
		"application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 1024)
	n, _ := resp.Body.Read(buf)
	reply := string(buf[:n])
	if !strings.Contains(reply, "<Message>") || !strings.Contains(reply, "90,745.00") {
		t.Errorf("webhook reply = %q, want the station's answer as TwiML", reply)
	}
}

func TestWebhookNeedsItsToken(t *testing.T) {
	_, u := msgServer(t)
	form := strings.NewReader(url.Values{"Body": {"hi"}}.Encode())
	resp, err := http.Post(u+"/v1/whatsapp/webhook?station_id=FM-TEST1&token=wrong",
		"application/x-www-form-urlencoded", form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("webhook with a bad token = %d, want 401", resp.StatusCode)
	}
}

func TestWebhookTellsTheSenderWhenTheStationIsOffline(t *testing.T) {
	s, u := msgServer(t)
	s.WebhookWait = 300 * time.Millisecond // nobody is answering
	form := strings.NewReader(url.Values{
		"From": {"whatsapp:+923001234567"},
		"Body": {"score?"},
	}.Encode())
	resp, err := http.Post(u+"/v1/whatsapp/webhook?station_id=FM-TEST1&token=hook-tok",
		"application/x-www-form-urlencoded", form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 512)
	n, _ := resp.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), "offline") {
		t.Errorf("reply = %q, want an offline message", string(buf[:n]))
	}
}

// do2 is do() without registering a cleanup on the test's goroutine.
func do2(t *testing.T, method, url, token, body string) {
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

// The shared webhook token authenticates the gateway, not the person. A
// caller who holds it must still not be able to read a station's takings
// by naming its id: station ids are printed on dashboards, not secret.
func TestWebhookRefusesUnregisteredSender(t *testing.T) {
	s, u := msgServer(t)
	s.WebhookWait = 300 * time.Millisecond

	form := url.Values{
		"From": {"whatsapp:+923009999999"}, // not the owner's number
		"Body": {"how much did we sell today?"},
	}
	resp, err := http.Post(u+"/v1/whatsapp/webhook?station_id=FM-TEST1&token=hook-tok",
		"application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 1024)
	n, _ := resp.Body.Read(buf)
	reply := string(buf[:n])
	if !strings.Contains(reply, "not registered") {
		t.Errorf("reply = %q, want a refusal for an unregistered number", reply)
	}

	// Nothing was queued, so the station never even sees the question.
	s.mu.Lock()
	queued := len(s.order)
	s.mu.Unlock()
	if queued != 0 {
		t.Errorf("queued %d messages for an unregistered sender, want 0", queued)
	}
}

// A registered owner must not be able to read a different station by
// putting someone else's id in the query string.
func TestWebhookRefusesMismatchedStationID(t *testing.T) {
	s, u := msgServer(t)
	s.AddStation(&Station{
		StationID: "FM-OTHER", APIKey: "other-key", Tier: "private",
		Status: "active", ValidFrom: time.Now(),
	})

	form := url.Values{
		"From": {"whatsapp:+923001234567"}, // owner of FM-TEST1
		"Body": {"how much did we sell today?"},
	}
	resp, err := http.Post(u+"/v1/whatsapp/webhook?station_id=FM-OTHER&token=hook-tok",
		"application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-station request = %d, want 403", resp.StatusCode)
	}
}

// The owner may register a number in any shape and message from any
// other shape of the same number.
func TestWebhookMatchesNumberInAnyFormat(t *testing.T) {
	s, _ := msgServer(t)
	s.mu.Lock()
	s.stations["FM-TEST1"].OwnerNumbers = []string{"0300-1234567"}
	st := s.stations["FM-TEST1"]
	s.mu.Unlock()

	for _, from := range []string{"whatsapp:+923001234567", "+92 300 1234567", "03001234567"} {
		if !st.ownsNumber(senderNumber(from)) {
			t.Errorf("ownsNumber(%q) = false, want true", from)
		}
	}
	if st.ownsNumber("+923009999999") {
		t.Error("ownsNumber matched a different number")
	}
}

// An unthrottled webhook lets a caller work through the number space at
// machine speed, so a burst from one sender is cut off.
func TestWebhookRateLimitsOneSender(t *testing.T) {
	s, u := msgServer(t)
	s.WebhookWait = 50 * time.Millisecond

	limited := false
	for i := 0; i < webhookBurst+5; i++ {
		form := url.Values{
			"From": {"whatsapp:+923009999999"},
			"Body": {"probe"},
		}
		resp, err := http.Post(u+"/v1/whatsapp/webhook?token=hook-tok",
			"application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 512)
		n, _ := resp.Body.Read(buf)
		resp.Body.Close()
		if strings.Contains(string(buf[:n]), "Too many messages") {
			limited = true
			break
		}
	}
	if !limited {
		t.Errorf("sent %d messages without being rate limited", webhookBurst+5)
	}
}
