// Package llm is the local-LLM intent router for Phase 7. It provides
// three routing paths:
//
//  1. Standard (regex/keyword) — no LLM, no network, <50ms.
//  2. Parameterized (templated SQL against the data mart) — no LLM, no
//     network, <50ms.
//  3. Complex / open-ended (Ollama HTTP client + data-mart context) —
//     on-prem LLM, <8s target on Standard tier.
//
// The Ollama client is intentionally simple: a single POST to
// /api/generate. We rely on the prompt template to constrain the
// model's behavior so it can never fabricate numbers — every numeric
// answer must come from the pre-computed mart context block passed
// in by the caller.
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

// Tier is the LLM sizing tier. Values are "basic" / "standard" /
// "enhanced" / "pro" and mirror the hardware package's outputs.
type Tier string

const (
	TierBasic    Tier = "basic"
	TierStandard Tier = "standard"
	TierEnhanced Tier = "enhanced"
	TierPro      Tier = "pro"
)

// ModelForTier returns the default Ollama model name for a given
// hardware tier. These are the v1 default picks; the user can
// override per install via local_config.
func ModelForTier(t Tier) string {
	switch t {
	case TierBasic:
		return "" // no LLM in basic
	case TierStandard:
		return "qwen2.5:1.5b"
	case TierEnhanced:
		return "qwen2.5:7b"
	case TierPro:
		return "qwen2.5:14b"
	}
	return ""
}

// OllamaEndpoint is the well-known default; can be overridden.
const DefaultOllamaEndpoint = "http://127.0.0.1:11434"

// Client is the HTTP boundary the router uses to talk to Ollama. It
// is also an interface so tests can inject a fake without a real
// Ollama running.
type Client interface {
	// Generate sends a single-turn prompt and returns the model's
	// text reply. context.Background() with a per-call timeout
	// should be used by the caller; this method does not impose
	// its own timeout.
	Generate(ctx context.Context, model, prompt string) (string, error)
	// Ping is a cheap readiness check used at startup to decide
	// whether to advertise LLM-backed intent routing.
	Ping(ctx context.Context) error
}

// httpClient is the default Client implementation against a real
// Ollama instance.
type httpClient struct {
	endpoint string
	http     *http.Client
}

// NewHTTPClient builds the default Ollama-bound Client. The endpoint
// argument should be the base URL (e.g. "http://127.0.0.1:11434").
func NewHTTPClient(endpoint string) Client {
	if endpoint == "" {
		endpoint = DefaultOllamaEndpoint
	}
	return &httpClient{
		endpoint: endpoint,
		http:     &http.Client{Timeout: 30 * time.Second},
	}
}

// generateRequest is the wire format Ollama expects at POST
// /api/generate.
type generateRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Stream bool   `json:"stream"`
}

// generateResponse is the JSON body Ollama returns.
type generateResponse struct {
	Response string `json:"response"`
	Done     bool   `json:"done"`
}

// Generate sends a single prompt to Ollama and returns the model's
// reply. The first chunk is the entire reply because we set
// stream=false; this is fine for our small prompts.
func (c *httpClient) Generate(ctx context.Context, model, prompt string) (string, error) {
	body, err := json.Marshal(generateRequest{Model: model, Prompt: prompt, Stream: false})
	if err != nil {
		return "", fmt.Errorf("llm: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("llm: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("llm: post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("llm: http %d: %s", resp.StatusCode, string(buf))
	}
	var out generateResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("llm: decode: %w", err)
	}
	return strings.TrimSpace(out.Response), nil
}

// Ping does a GET /api/tags; success means Ollama is reachable. It
// does NOT verify a specific model is installed — that's a
// per-prompt concern that surfaces as a clear error from Generate.
func (c *httpClient) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+"/api/tags", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("llm: ping http %d", resp.StatusCode)
	}
	return nil
}
