package messaging

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fuelmind/fuelmind/internal/storage"
)

func inboundStore(t *testing.T) *storage.Storage {
	t.Helper()
	s, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type stubAnswerer struct {
	reply  string
	path   string
	asked  []string
	failIt error
}

func (s *stubAnswerer) Answer(_ context.Context, q string) (string, string, error) {
	s.asked = append(s.asked, q)
	if s.failIt != nil {
		return "", "error", s.failIt
	}
	path := s.path
	if path == "" {
		path = "standard"
	}
	return s.reply, path, nil
}

func inboundFixture(t *testing.T, ownerNumber string) (*Inbound, *storage.Storage, *stubAnswerer) {
	t.Helper()
	store := inboundStore(t)
	if ownerNumber != "" {
		if err := store.SetLocalConfig(context.Background(), ConfigKeyRecipient, ownerNumber, "", false); err != nil {
			t.Fatal(err)
		}
	}
	ans := &stubAnswerer{reply: "Today's revenue is PKR 90,745.00."}
	return NewInbound(store, ans, quietLogger()), store, ans
}

// The owner messages the station's own WhatsApp number and gets an
// answer from the shop PC. This is the whole point: no control plane, no
// SMS gateway, no inbound port.
func TestOwnerGetsAnAnswer(t *testing.T) {
	in, _, ans := inboundFixture(t, "+923001234567")

	got := in.Handle(context.Background(), "923001234567", "how much did we sell today?")
	if got != "Today's revenue is PKR 90,745.00." {
		t.Errorf("reply = %q, want the station's answer", got)
	}
	if len(ans.asked) != 1 || ans.asked[0] != "how much did we sell today?" {
		t.Errorf("the question did not reach the answerer: %v", ans.asked)
	}
}

// The number is matched in canonical form, so however the owner saved it
// and however WhatsApp reports it, it is the same person.
func TestOwnerNumberMatchesInAnyFormat(t *testing.T) {
	for _, saved := range []string{"+92 300 1234567", "0300-1234567", "03001234567", "923001234567"} {
		in, _, _ := inboundFixture(t, saved)
		if got := in.Handle(context.Background(), "923001234567", "sales today"); got == "" {
			t.Errorf("a number saved as %q did not match the sender", saved)
		}
	}
}

// A stranger who messages the station's WhatsApp learns nothing. Silence
// rather than a refusal: replying would confirm the number belongs to a
// station, and auto-replying to strangers is how the account gets banned.
func TestStrangerGetsSilence(t *testing.T) {
	in, store, ans := inboundFixture(t, "+923001234567")

	got := in.Handle(context.Background(), "923009999999", "how much did we sell today?")
	if got != "" {
		t.Errorf("reply to a stranger = %q, want silence", got)
	}
	if len(ans.asked) != 0 {
		t.Errorf("a stranger's question reached the answerer: %v", ans.asked)
	}
	// The owner should still be able to see that it happened.
	questions, err := store.RecentRemoteQuestions(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(questions) != 1 || !strings.Contains(questions[0].Route, "ignored-unknown-sender") {
		t.Errorf("the ignored message was not logged for the owner: %+v", questions)
	}
}

// With no number registered, nobody is the owner, so nobody is answered.
func TestNoRegisteredNumberAnswersNobody(t *testing.T) {
	in, _, _ := inboundFixture(t, "")
	if got := in.Handle(context.Background(), "923001234567", "sales today"); got != "" {
		t.Errorf("reply = %q, want silence when no number is registered", got)
	}
}

func TestAnsweringCanBeTurnedOff(t *testing.T) {
	in, store, _ := inboundFixture(t, "+923001234567")
	ctx := context.Background()

	if !AnswerQuestionsEnabled(ctx, store) {
		t.Error("answering should be on once a phone is paired")
	}
	if err := SetAnswerQuestions(ctx, store, false); err != nil {
		t.Fatal(err)
	}
	if got := in.Handle(ctx, "923001234567", "sales today"); got != "" {
		t.Errorf("reply = %q, want silence when answering is off", got)
	}
}

// Every reply is a message sent from the owner's own WhatsApp account.
// A loop would send hundreds, which is exactly what gets an account
// banned, so a burst is cut off.
func TestBurstIsRateLimited(t *testing.T) {
	in, _, _ := inboundFixture(t, "+923001234567")
	ctx := context.Background()

	limited := ""
	for i := 0; i < inboundBurst+3; i++ {
		reply := in.Handle(ctx, "923001234567", "sales today")
		if strings.Contains(reply, "lot of questions") {
			limited = reply
			break
		}
	}
	if limited == "" {
		t.Errorf("sent %d questions without being rate limited", inboundBurst+3)
	}
}

// Every question and answer is written down, so the owner can see what
// was asked of their station while they were away.
func TestExchangesAreLogged(t *testing.T) {
	in, store, _ := inboundFixture(t, "+923001234567")
	ctx := context.Background()

	in.Handle(ctx, "923001234567", "how much credit is outstanding?")
	questions, err := store.RecentRemoteQuestions(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(questions) != 1 {
		t.Fatalf("logged %d exchanges, want 1", len(questions))
	}
	q := questions[0]
	if q.Question != "how much credit is outstanding?" {
		t.Errorf("question = %q", q.Question)
	}
	if !strings.HasPrefix(q.Route, "whatsapp:") {
		t.Errorf("route = %q, want it to say the question came over WhatsApp", q.Route)
	}
	// The number is stored in canonical form so it lines up with the
	// rest of the station's records.
	if q.AskedBy != "+923001234567" {
		t.Errorf("asked_by = %q, want the canonical number", q.AskedBy)
	}
}

// A failure to answer must still send something back: silence looks
// like the station is dead.
func TestAnswerFailureStillReplies(t *testing.T) {
	in, _, ans := inboundFixture(t, "+923001234567")
	ans.reply = ""

	got := in.Handle(context.Background(), "923001234567", "sales today")
	if got == "" {
		t.Error("an empty answer produced silence; the owner should be told something")
	}
}
