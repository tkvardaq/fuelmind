package relay

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/fuelmind/fuelmind/internal/ask"
	"github.com/fuelmind/fuelmind/internal/ident"
	"github.com/fuelmind/fuelmind/internal/llm"
	"github.com/fuelmind/fuelmind/internal/storage"
)

func newStore(t *testing.T) *storage.Storage {
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

// fakeCloud is a control plane that hands out one question and records
// the answer that comes back.
type fakeCloud struct {
	mu        sync.Mutex
	pending   []Message
	answers   map[string]string
	authSeen  bool
	replyCode int
}

func (f *fakeCloud) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages/pending", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.authSeen = r.Header.Get("Authorization") != ""
		_ = json.NewEncoder(w).Encode(map[string]any{"messages": f.pending})
	})
	mux.HandleFunc("/v1/messages/", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Answer string `json:"answer"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.replyCode != 0 {
			w.WriteHeader(f.replyCode)
			return
		}
		if f.answers == nil {
			f.answers = map[string]string{}
		}
		id := r.URL.Path[len("/v1/messages/") : len(r.URL.Path)-len("/reply")]
		f.answers[id] = body.Answer
		f.pending = nil
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

func newAgent(t *testing.T, store *storage.Storage, url string) *Agent {
	t.Helper()
	id, _ := ident.GenerateIdentity()
	return New(Config{
		Store:    store,
		Answerer: ask.New(store, llm.NewRouter(nil, llm.TierBasic)),
		Identity: id,
		CloudURL: url,
		Logger:   slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	})
}

func TestAnswersAPendingQuestion(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	seedSale(t, store)

	cloud := &fakeCloud{pending: []Message{{ID: "m1", From: "+923001234567", Text: "how much did we sell today?"}}}
	srv := httptest.NewServer(cloud.handler())
	defer srv.Close()

	n, err := newAgent(t, store, srv.URL).Tick(ctx)
	if err != nil || n != 1 {
		t.Fatalf("answered %d, err %v", n, err)
	}
	cloud.mu.Lock()
	answer := cloud.answers["m1"]
	auth := cloud.authSeen
	cloud.mu.Unlock()
	if !auth {
		t.Error("the station polled without authenticating")
	}
	if answer == "" || !contains(answer, "PKR 5,000.00") {
		t.Errorf("answer sent back = %q, want today's revenue", answer)
	}

	// The question and answer are kept locally so the owner can see them.
	log, err := store.RecentRemoteQuestions(ctx, 10)
	if err != nil || len(log) != 1 {
		t.Fatalf("question log = %v (err %v)", log, err)
	}
	if log[0].AskedBy != "+923001234567" || log[0].Route != "standard" {
		t.Errorf("logged %+v", log[0])
	}
}

func TestOffByDefaultAndTogglable(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	if Enabled(ctx, store) {
		t.Error("remote questions must be off until the owner turns them on")
	}
	if err := SetEnabled(ctx, store, true); err != nil {
		t.Fatal(err)
	}
	if !Enabled(ctx, store) {
		t.Error("turning it on did not stick")
	}
	if err := SetEnabled(ctx, store, false); err != nil {
		t.Fatal(err)
	}
	if Enabled(ctx, store) {
		t.Error("turning it off did not stick")
	}
}

// The loop must not answer anything while the switch is off.
func TestRunIgnoresQuestionsWhileOff(t *testing.T) {
	store := newStore(t)
	cloud := &fakeCloud{pending: []Message{{ID: "m1", Text: "how much did we sell today?"}}}
	srv := httptest.NewServer(cloud.handler())
	defer srv.Close()

	a := newAgent(t, store, srv.URL)
	a.cfg.Poll = 20 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	a.Run(ctx)

	cloud.mu.Lock()
	defer cloud.mu.Unlock()
	if len(cloud.answers) != 0 {
		t.Errorf("answered %d question(s) while switched off", len(cloud.answers))
	}
}

func TestReplyFailureIsReported(t *testing.T) {
	store := newStore(t)
	cloud := &fakeCloud{
		pending:   []Message{{ID: "m1", Text: "how much did we sell today?"}},
		replyCode: http.StatusInternalServerError,
	}
	srv := httptest.NewServer(cloud.handler())
	defer srv.Close()
	if _, err := newAgent(t, store, srv.URL).Tick(context.Background()); err == nil {
		t.Error("a failed reply should surface as an error")
	}
}

func seedSale(t *testing.T, s *storage.Storage) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.DB().ExecContext(ctx, `
		INSERT INTO raw_pos_transactions (pos_source_id, raw_payload, payload_hash, ingestion_batch_id)
		VALUES ('lane_1', '{}', 'h1', 'b1')`); err != nil {
		t.Fatal(err)
	}
	today := time.Now().Format("2006-01-02")
	if _, err := s.DB().ExecContext(ctx, `
		INSERT INTO daily_sales (date, product_code, volume_liters, revenue, transaction_count)
		VALUES (?, 'DIESEL', 20, 5000, 1)`, today); err != nil {
		t.Fatal(err)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || len(needle) == 0 ||
		func() bool {
			for i := 0; i+len(needle) <= len(haystack); i++ {
				if haystack[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		}())
}
