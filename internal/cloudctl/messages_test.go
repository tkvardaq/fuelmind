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
	form := strings.NewReader(url.Values{"Body": {"score?"}}.Encode())
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
