// Package relay lets the owner ask about their station from away — over
// WhatsApp, SMS or the support console — without exposing the station to
// the internet.
//
// The station polls the control plane for questions addressed to it,
// answers them locally from its own data mart, and posts the answer back.
// Nothing listens on an inbound port, and the station stays in control:
// the relay only runs when the owner has switched it on.
//
// Privacy: an answer contains figures (today's revenue, credit totals),
// so the answer text passes through the control plane. That is why this
// is opt-in per station and off by default.
package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/fuelmind/fuelmind/internal/ask"
	"github.com/fuelmind/fuelmind/internal/ident"
	"github.com/fuelmind/fuelmind/internal/storage"
)

// ConfigKeyEnabled is the local_config key holding the owner's choice
// ("on" / "off"). Off unless the owner turns it on.
const ConfigKeyEnabled = "remote_ask"

// Message is one question waiting for this station.
type Message struct {
	ID       string `json:"id"`
	From     string `json:"from"`
	Text     string `json:"text"`
	Channel  string `json:"channel,omitempty"`
	Received string `json:"received_at,omitempty"`
}

// Enabled reports whether the owner switched remote questions on.
func Enabled(ctx context.Context, store *storage.Storage) bool {
	return strings.EqualFold(store.LocalConfigValue(ctx, ConfigKeyEnabled, "off"), "on")
}

// SetEnabled turns remote questions on or off.
func SetEnabled(ctx context.Context, store *storage.Storage, on bool) error {
	v := "off"
	if on {
		v = "on"
	}
	return store.SetLocalConfig(ctx, ConfigKeyEnabled, v, "", false)
}

// Config wires an Agent.
type Config struct {
	Store      *storage.Storage
	Answerer   *ask.Service
	Identity   ident.Identity
	CloudURL   string
	Logger     *slog.Logger
	Poll       time.Duration // default 20s
	HTTPClient *http.Client
	MaxPerPoll int // default 10
}

// Agent polls for questions and answers them.
type Agent struct{ cfg Config }

// New builds an Agent with defaults applied.
func New(cfg Config) *Agent {
	if cfg.Poll == 0 {
		cfg.Poll = 20 * time.Second
	}
	if cfg.MaxPerPoll == 0 {
		cfg.MaxPerPoll = 10
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	cfg.CloudURL = strings.TrimRight(cfg.CloudURL, "/")
	return &Agent{cfg: cfg}
}

// Run polls until ctx is done. It checks the owner's switch on every
// cycle, so turning remote questions off takes effect without a restart.
func (a *Agent) Run(ctx context.Context) {
	ticker := time.NewTicker(a.cfg.Poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Read the on/off flag with a context that shutdown cannot
			// cancel. Cancelling a one-row local read gains nothing, and
			// a query cancelled mid-flight leaves database/sql to close
			// its connection asynchronously — which on Windows means the
			// database file is still open after Close returns.
			if !Enabled(context.WithoutCancel(ctx), a.cfg.Store) {
				continue
			}
			if n, err := a.Tick(ctx); err != nil {
				a.cfg.Logger.Warn("remote questions: poll failed", "err", err)
			} else if n > 0 {
				a.cfg.Logger.Info("remote questions answered", "count", n)
			}
		}
	}
}

// Tick answers every question waiting right now and returns how many.
func (a *Agent) Tick(ctx context.Context) (int, error) {
	msgs, err := a.pending(ctx)
	if err != nil {
		return 0, err
	}
	answered := 0
	for i, msg := range msgs {
		if i >= a.cfg.MaxPerPoll {
			break
		}
		answer, path, aerr := a.cfg.Answerer.Answer(ctx, msg.Text)
		if aerr != nil {
			a.cfg.Logger.Warn("remote question: answering failed", "id", msg.ID, "err", aerr)
		}
		if err := a.reply(ctx, msg.ID, answer); err != nil {
			return answered, err
		}
		if err := a.cfg.Store.RecordRemoteQuestion(ctx, storage.RemoteQuestion{
			MessageID: msg.ID, AskedBy: msg.From, Question: msg.Text, Answer: answer, Route: path,
		}); err != nil {
			a.cfg.Logger.Warn("remote question: could not log it", "err", err)
		}
		a.cfg.Logger.Info("remote question answered", "from", msg.From, "path", path)
		answered++
	}
	return answered, nil
}

func (a *Agent) pending(ctx context.Context) ([]Message, error) {
	req, err := a.request(ctx, http.MethodGet,
		a.cfg.CloudURL+"/v1/messages/pending?station_id="+a.cfg.Identity.StationID.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil, nil
	case http.StatusOK:
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Messages []Message `json:"messages"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("bad message list: %w", err)
	}
	return out.Messages, nil
}

func (a *Agent) reply(ctx context.Context, id, answer string) error {
	body, err := json.Marshal(map[string]string{"answer": answer})
	if err != nil {
		return err
	}
	req, err := a.request(ctx, http.MethodPost,
		fmt.Sprintf("%s/v1/messages/%s/reply", a.cfg.CloudURL, id), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.cfg.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("reply rejected: http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

func (a *Agent) request(ctx context.Context, method, url string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.Identity.APIKey.String())
	return req, nil
}
