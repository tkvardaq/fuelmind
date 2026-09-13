package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Provider is where the station's answers come from when a question
// needs more than the fixed answers.
//
// Ollama is the local-first default: nothing leaves the shop PC. The
// hosted providers are for stations whose PC cannot run a model — most
// of them, on the hardware a petrol station actually has — and they are
// an explicit, opt-in trade: the question and the station's figures for
// that question are sent to that provider.
type Provider string

const (
	// ProviderNone means only the fixed answers are available.
	ProviderNone Provider = "none"
	// ProviderOllama talks to a model running on this machine.
	ProviderOllama Provider = "ollama"
	// ProviderOpenAI is any OpenAI-compatible /v1/chat/completions
	// endpoint: OpenAI itself, and also Groq, OpenRouter, Together,
	// vLLM and most other hosts, by changing the base URL.
	ProviderOpenAI Provider = "openai"
	// ProviderAnthropic is the Anthropic Messages API.
	ProviderAnthropic Provider = "anthropic"
)

// Valid reports whether p is a provider FuelMind knows how to talk to.
func (p Provider) Valid() bool {
	switch p {
	case ProviderNone, ProviderOllama, ProviderOpenAI, ProviderAnthropic:
		return true
	}
	return false
}

// Label is the provider's name as the owner should see it.
func (p Provider) Label() string {
	switch p {
	case ProviderOllama:
		return "On this PC (Ollama)"
	case ProviderOpenAI:
		return "OpenAI-compatible API"
	case ProviderAnthropic:
		return "Anthropic API"
	default:
		return "Set questions only"
	}
}

// SendsDataOffSite reports whether using this provider means the
// station's figures leave the shop. The Settings page says so plainly
// next to the choice, because it is the owner's call to make.
func (p Provider) SendsDataOffSite() bool {
	return p == ProviderOpenAI || p == ProviderAnthropic
}

// Default endpoints. Each is overridable so a station can point at a
// compatible host, a regional endpoint, or a gateway of its own.
const (
	DefaultOpenAIBaseURL    = "https://api.openai.com/v1"
	DefaultAnthropicBaseURL = "https://api.anthropic.com/v1"
	// anthropicVersion is the API version header the Messages API
	// requires. It is pinned rather than tracked, so an upstream change
	// cannot alter how a station behaves without a FuelMind release.
	anthropicVersion = "2023-06-01"
)

// DefaultModelFor is a sensible starting model per provider: small and
// cheap, because the work here is summarizing a handful of numbers that
// are already computed, not reasoning from scratch.
func DefaultModelFor(p Provider) string {
	switch p {
	case ProviderOpenAI:
		return "gpt-4o-mini"
	case ProviderAnthropic:
		return "claude-haiku-4-5-20251001"
	case ProviderOllama:
		return ModelForTier(TierStandard)
	}
	return ""
}

// Settings is everything needed to reach a model. It is built from the
// station's saved configuration.
type Settings struct {
	Provider Provider
	Model    string
	APIKey   string
	BaseURL  string
	// Timeout bounds one call. A question asked over WhatsApp is being
	// waited on by a person, so this stays short.
	Timeout time.Duration
}

// maxAnswerTokens caps a reply. The answers are two to four sentences
// about numbers the model was handed, so this is generous.
const maxAnswerTokens = 400

// NewClient builds the Client for these settings. It returns nil when
// no model is configured, which the router reads as "fixed answers
// only" and says so to the owner.
func NewClient(s Settings) Client {
	switch s.Provider {
	case ProviderOllama:
		base := s.BaseURL
		if base == "" {
			base = DefaultOllamaEndpoint
		}
		return NewHTTPClient(base)
	case ProviderOpenAI:
		if s.APIKey == "" {
			return nil
		}
		base := s.BaseURL
		if base == "" {
			base = DefaultOpenAIBaseURL
		}
		return &openAIClient{base: strings.TrimRight(base, "/"), key: s.APIKey, http: apiHTTPClient(s.Timeout)}
	case ProviderAnthropic:
		if s.APIKey == "" {
			return nil
		}
		base := s.BaseURL
		if base == "" {
			base = DefaultAnthropicBaseURL
		}
		return &anthropicClient{base: strings.TrimRight(base, "/"), key: s.APIKey, http: apiHTTPClient(s.Timeout)}
	}
	return nil
}

func apiHTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &http.Client{Timeout: timeout}
}

// --- OpenAI-compatible ------------------------------------------------

type openAIClient struct {
	base string
	key  string
	http *http.Client
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIRequest struct {
	Model     string          `json:"model"`
	Messages  []openAIMessage `json:"messages"`
	MaxTokens int             `json:"max_tokens,omitempty"`
	// Temperature 0 keeps the model close to the numbers it was given,
	// which is the whole contract here.
	Temperature float64 `json:"temperature"`
}

type openAIResponse struct {
	Choices []struct {
		Message openAIMessage `json:"message"`
	} `json:"choices"`
	Error *apiErrorBody `json:"error"`
}

// Generate asks an OpenAI-compatible endpoint. The system prompt is sent
// as a system message rather than glued to the front of the question, so
// the "only use these numbers" rule carries the weight the API gives it.
func (c *openAIClient) Generate(ctx context.Context, model, prompt string) (string, error) {
	system, user := splitPrompt(prompt)
	body, err := json.Marshal(openAIRequest{
		Model: model,
		Messages: []openAIMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		MaxTokens:   maxAnswerTokens,
		Temperature: 0,
	})
	if err != nil {
		return "", fmt.Errorf("llm: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.key)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("llm: openai: %w", redactKey(err, c.key))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var out openAIResponse
	if err := json.Unmarshal(raw, &out); err != nil && resp.StatusCode/100 == 2 {
		return "", fmt.Errorf("llm: openai: unreadable reply: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("llm: openai: %s", apiError(resp.StatusCode, errMessage(out.Error), raw))
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("llm: openai: the model returned nothing")
	}
	return strings.TrimSpace(out.Choices[0].Message.Content), nil
}

// Ping checks the key works, without spending a real request on a model
// that may not exist: listing models is the cheapest authenticated call.
func (c *openAIClient) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/models", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	resp, err := c.http.Do(req)
	if err != nil {
		return redactKey(err, c.key)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("llm: openai: %s", apiError(resp.StatusCode, "", raw))
	}
	return nil
}

// --- Anthropic --------------------------------------------------------

type anthropicClient struct {
	base string
	key  string
	http *http.Client
}

type anthropicRequest struct {
	Model     string          `json:"model"`
	System    string          `json:"system,omitempty"`
	Messages  []openAIMessage `json:"messages"`
	MaxTokens int             `json:"max_tokens"`
	// Temperature 0: repeat the given numbers, do not be creative.
	Temperature float64 `json:"temperature"`
}

// apiErrorBody is the error shape both providers return, close enough to
// share: a message and a machine-readable type.
type apiErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Error *apiErrorBody `json:"error"`
}

func (c *anthropicClient) Generate(ctx context.Context, model, prompt string) (string, error) {
	system, user := splitPrompt(prompt)
	body, err := json.Marshal(anthropicRequest{
		Model:       model,
		System:      system,
		Messages:    []openAIMessage{{Role: "user", Content: user}},
		MaxTokens:   maxAnswerTokens,
		Temperature: 0,
	})
	if err != nil {
		return "", fmt.Errorf("llm: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/messages", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.key)
	req.Header.Set("anthropic-version", anthropicVersion)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("llm: anthropic: %w", redactKey(err, c.key))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var out anthropicResponse
	if err := json.Unmarshal(raw, &out); err != nil && resp.StatusCode/100 == 2 {
		return "", fmt.Errorf("llm: anthropic: unreadable reply: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("llm: anthropic: %s", apiError(resp.StatusCode, errMessage(out.Error), raw))
	}
	var b strings.Builder
	for _, part := range out.Content {
		if part.Type == "text" {
			b.WriteString(part.Text)
		}
	}
	if b.Len() == 0 {
		return "", fmt.Errorf("llm: anthropic: the model returned nothing")
	}
	return strings.TrimSpace(b.String()), nil
}

// Ping sends the smallest possible real request. Anthropic has no free
// listing endpoint, so this costs one token or two — once, when the
// owner presses Test.
func (c *anthropicClient) Ping(ctx context.Context) error {
	body, _ := json.Marshal(anthropicRequest{
		Model:     DefaultModelFor(ProviderAnthropic),
		Messages:  []openAIMessage{{Role: "user", Content: "ping"}},
		MaxTokens: 1,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/messages", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.key)
	req.Header.Set("anthropic-version", anthropicVersion)

	resp, err := c.http.Do(req)
	if err != nil {
		return redactKey(err, c.key)
	}
	defer resp.Body.Close()
	// A 400 here means the request shape was wrong but the key was
	// accepted, which is all Ping is asked to establish.
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("llm: anthropic: %s", apiError(resp.StatusCode, "", raw))
	}
	return nil
}

// --- shared -----------------------------------------------------------

// splitPrompt divides the router's single prompt into the system rules
// and the question, so chat-shaped APIs get it the way they expect.
// buildPrompt always starts with SystemPromptPrefix.
func splitPrompt(prompt string) (system, user string) {
	if rest, ok := strings.CutPrefix(prompt, SystemPromptPrefix); ok {
		return SystemPromptPrefix, strings.TrimSpace(rest)
	}
	return SystemPromptPrefix, prompt
}

func errMessage(e *apiErrorBody) string {
	if e == nil {
		return ""
	}
	return e.Message
}

// apiError turns a failed call into something an owner can act on. The
// status code alone ("http 401") tells them nothing.
func apiError(status int, message string, raw []byte) string {
	if message == "" {
		message = strings.TrimSpace(string(raw))
		if len(message) > 200 {
			message = message[:200]
		}
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return "the API key was not accepted. Check it was copied in full and has not been revoked."
	case http.StatusTooManyRequests:
		return "the provider is rate limiting this key. Try again in a minute, or check the account's usage limits."
	case http.StatusNotFound:
		return "that model name was not found. Check the model in Settings."
	case http.StatusPaymentRequired:
		return "the provider refused the request for billing reasons. Check the account has credit."
	}
	if status >= 500 {
		return fmt.Sprintf("the provider had a problem (HTTP %d). This is usually temporary.", status)
	}
	return fmt.Sprintf("HTTP %d: %s", status, message)
}

// redactKey makes sure an API key never reaches a log or the screen. Go
// puts the request URL in transport errors, and a key in a query string
// (some compatible hosts accept that) would ride along with it.
func redactKey(err error, key string) error {
	if err == nil || key == "" {
		return err
	}
	msg := err.Error()
	if !strings.Contains(msg, key) {
		return err
	}
	return fmt.Errorf("%s", strings.ReplaceAll(msg, key, "***"))
}
