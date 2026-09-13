package web

import (
	"context"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/fuelmind/fuelmind/internal/messaging"
)

type stubPairer struct {
	paired  bool
	code    string
	started bool
}

func (s *stubPairer) Paired() bool   { return s.paired }
func (s *stubPairer) Ready() bool    { return s.paired }
func (s *stubPairer) Number() string { return "923009999999" }
func (s *stubPairer) StartPairing(context.Context) error {
	s.started = true
	s.code = "2@abc"
	return nil
}
func (s *stubPairer) PairingCode() (string, string) { return s.code, "" }
func (s *stubPairer) Unpair(context.Context) error  { s.paired, s.code = false, ""; return nil }

type stubMessenger struct{ tests, drains int }

func (s *stubMessenger) QueueTest(context.Context) error { s.tests++; return nil }
func (s *stubMessenger) Once(context.Context) error      { s.drains++; return nil }

func TestMessagesPageStartsEmptyAndOff(t *testing.T) {
	_, do := loggedInServer(t)
	w := do("GET", "/messages", nil)
	body := w.Body.String()
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(body, "Nothing sent yet") {
		t.Errorf("messages page does not show its empty state:\n%s", body)
	}
	if strings.Contains(body, `name="enabled" checked`) {
		t.Error("messaging is switched on before the owner asked for it")
	}
}

func TestMessagesSaveRequiresANumberWhenOn(t *testing.T) {
	srv, do := loggedInServer(t)
	w := do("POST", "/messages", url.Values{"action": {"save"}, "enabled": {"on"}, "recipient": {""}})
	if !strings.Contains(w.Body.String(), "Enter the phone number") {
		t.Errorf("switching messages on without a number was accepted:\n%s", w.Body.String())
	}
	if messaging.LoadSettings(context.Background(), srv.store).Enabled {
		t.Error("messaging was enabled with nowhere to send")
	}
}

func TestMessagesSaveKeepsTheOwnersChoices(t *testing.T) {
	srv, do := loggedInServer(t)
	w := do("POST", "/messages", url.Values{
		"action": {"save"}, "enabled": {"on"}, "alerts": {"on"},
		"recipient": {"+92 300 123 4567"}, "summary_hour": {"20"},
	})
	if !strings.Contains(w.Body.String(), "8:00 pm") {
		t.Errorf("save did not confirm the summary time:\n%s", w.Body.String())
	}
	got := messaging.LoadSettings(context.Background(), srv.store)
	if !got.Enabled || got.Recipient != "923001234567" || got.SummaryHour != 20 || !got.Alerts {
		t.Errorf("saved settings = %+v", got)
	}
}

func TestMessagesTestButtonNeedsAMessenger(t *testing.T) {
	srv, do := loggedInServer(t)
	m := &stubMessenger{}
	srv.Messages = m
	if w := do("POST", "/messages", url.Values{"action": {"test"}}); !strings.Contains(w.Body.String(), "Test message queued") {
		t.Errorf("test button: %s", w.Body.String())
	}
	if m.tests != 1 {
		t.Errorf("QueueTest called %d times, want 1", m.tests)
	}
}

func TestPairingShowsAQRCode(t *testing.T) {
	srv, do := loggedInServer(t)
	p := &stubPairer{}
	srv.Pairing = p
	w := do("POST", "/messages", url.Values{"action": {"pair"}})
	if !p.started {
		t.Fatal("pair button did not start pairing")
	}
	if !strings.Contains(w.Body.String(), "/messages/qr.png") {
		t.Errorf("pairing page does not show the code:\n%s", w.Body.String())
	}
	qr := do("GET", "/messages/qr.png", nil)
	if qr.Code != 200 || qr.Header().Get("Content-Type") != "image/png" || qr.Body.Len() < 100 {
		t.Errorf("qr.png: status=%d type=%q bytes=%d", qr.Code, qr.Header().Get("Content-Type"), qr.Body.Len())
	}
}

func TestPairedStationShowsItsNumber(t *testing.T) {
	srv, do := loggedInServer(t)
	srv.Pairing = &stubPairer{paired: true}
	if body := do("GET", "/messages", nil).Body.String(); !strings.Contains(body, "923009999999") {
		t.Errorf("paired number not shown:\n%s", body)
	}
}

func TestMessagesRequireLogin(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for _, path := range []string{"/messages", "/messages/qr.png"} {
		r := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		srv.Routes().ServeHTTP(w, r)
		if w.Code != 302 {
			t.Errorf("%s without a session = %d, want a redirect to /login", path, w.Code)
		}
	}
}

func TestFormatHour(t *testing.T) {
	for in, want := range map[int]string{0: "12:00 am", 9: "9:00 am", 12: "12:00 pm", 21: "9:00 pm"} {
		if got := formatHour(in); got != want {
			t.Errorf("formatHour(%d) = %q, want %q", in, got, want)
		}
	}
}
