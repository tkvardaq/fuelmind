package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAIClientSendsChatShape(t *testing.T) {
	var gotAuth, gotPath string
	var body openAIRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Today's revenue is PKR 125,000.50."}}]}`))
	}))
	defer srv.Close()

	c := NewClient(Settings{Provider: ProviderOpenAI, APIKey: "sk-test", BaseURL: srv.URL})
	got, err := c.Generate(context.Background(), "gpt-4o-mini", buildPrompt("how much today?", sampleContext()))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got != "Today's revenue is PKR 125,000.50." {
		t.Errorf("answer = %q", got)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("path = %q, want /chat/completions", gotPath)
	}
	// The grounding rules must arrive as a system message, not buried in
	// the user's question.
	if len(body.Messages) != 2 || body.Messages[0].Role != "system" {
		t.Fatalf("messages = %+v, want a system message then the question", body.Messages)
	}
	if !strings.Contains(body.Messages[0].Content, "Only use the numbers provided") {
		t.Errorf("system message is missing the grounding rule: %q", body.Messages[0].Content)
	}
	if !strings.Contains(body.Messages[1].Content, "how much today?") {
		t.Errorf("the question did not reach the model: %q", body.Messages[1].Content)
	}
	if body.Temperature != 0 {
		t.Errorf("temperature = %v, want 0", body.Temperature)
	}
}

func TestAnthropicClientSendsMessagesShape(t *testing.T) {
	var gotKey, gotVersion string
	var body anthropicRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotVersion = r.Header.Get("x-api-key"), r.Header.Get("anthropic-version")
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"Revenue today was PKR 125,000.50."}]}`))
	}))
	defer srv.Close()

	c := NewClient(Settings{Provider: ProviderAnthropic, APIKey: "sk-ant-test", BaseURL: srv.URL})
	got, err := c.Generate(context.Background(), "claude-haiku-4-5-20251001", buildPrompt("how much today?", sampleContext()))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got != "Revenue today was PKR 125,000.50." {
		t.Errorf("answer = %q", got)
	}
	if gotKey != "sk-ant-test" {
		t.Errorf("x-api-key = %q", gotKey)
	}
	if gotVersion == "" {
		t.Error("anthropic-version header is required and was not sent")
	}
	if !strings.Contains(body.System, "Only use the numbers provided") {
		t.Errorf("system prompt missing the grounding rule: %q", body.System)
	}
	if len(body.Messages) != 1 || body.Messages[0].Role != "user" {
		t.Errorf("messages = %+v, want one user message", body.Messages)
	}
	if body.MaxTokens == 0 {
		t.Error("max_tokens is required by the Messages API and was not set")
	}
}

// An owner who mistypes a key needs to be told that, not "http 401".
func TestAPIErrorsAreExplained(t *testing.T) {
	cases := []struct {
		status int
		wantIn string
	}{
		{http.StatusUnauthorized, "API key was not accepted"},
		{http.StatusTooManyRequests, "rate limiting"},
		{http.StatusNotFound, "model name was not found"},
		{http.StatusInternalServerError, "provider had a problem"},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(`{"error":{"message":"nope"}}`))
		}))
		client := NewClient(Settings{Provider: ProviderOpenAI, APIKey: "sk-test", BaseURL: srv.URL})
		_, err := client.Generate(context.Background(), "m", "p")
		srv.Close()
		if err == nil {
			t.Fatalf("status %d: expected an error", c.status)
		}
		if !strings.Contains(err.Error(), c.wantIn) {
			t.Errorf("status %d: error = %q, want it to mention %q", c.status, err, c.wantIn)
		}
	}
}

// A provider with no key configured must not produce a half-working
// client that fails on every question.
func TestNewClientRefusesAnEmptyKey(t *testing.T) {
	for _, p := range []Provider{ProviderOpenAI, ProviderAnthropic} {
		if c := NewClient(Settings{Provider: p}); c != nil {
			t.Errorf("NewClient(%s) with no key returned a client", p)
		}
	}
	if c := NewClient(Settings{Provider: ProviderNone}); c != nil {
		t.Error("NewClient(none) returned a client")
	}
	if c := NewClient(Settings{Provider: ProviderOllama}); c == nil {
		t.Error("NewClient(ollama) needs no key and should return a client")
	}
}

// A key must never reach a log line or the owner's screen.
func TestMaskKeyHidesTheSecret(t *testing.T) {
	got := MaskKey("sk-proj-abcdefghijklmnop1234")
	if strings.Contains(got, "abcdefghijklmnop") {
		t.Errorf("MaskKey leaked the key: %q", got)
	}
	if !strings.HasPrefix(got, "sk-") || !strings.HasSuffix(got, "1234") {
		t.Errorf("MaskKey = %q, want enough to recognise the key", got)
	}
	if MaskKey("") != "" {
		t.Error("MaskKey(empty) should stay empty")
	}
	if strings.Contains(MaskKey("short"), "short") {
		t.Error("a short key was shown in full")
	}
}
