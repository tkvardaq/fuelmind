package llm

import (
	"context"
	"strings"
	"testing"
)

// fakeConfigStore is a local_config in a map.
type fakeConfigStore map[string]string

func (f fakeConfigStore) LocalConfigValue(_ context.Context, key, def string) string {
	if v, ok := f[key]; ok {
		return v
	}
	return def
}

func (f fakeConfigStore) SetLocalConfig(_ context.Context, key, value, _ string, _ bool) error {
	f[key] = value
	return nil
}

func TestLoadSettingsDefaultsByTier(t *testing.T) {
	ctx := context.Background()

	// A PC that can run a model keeps everything local by default.
	s := LoadSettings(ctx, fakeConfigStore{}, "http://127.0.0.1:11434", TierStandard)
	if s.Provider != ProviderOllama {
		t.Errorf("provider = %q, want ollama by default", s.Provider)
	}
	if s.Model != ModelForTier(TierStandard) {
		t.Errorf("model = %q, want the tier's model", s.Model)
	}
	if s.BaseURL != "http://127.0.0.1:11434" {
		t.Errorf("base url = %q, want the configured Ollama endpoint", s.BaseURL)
	}

	// A PC that cannot gets the fixed answers rather than a model that
	// would make the dashboard unusable.
	if s := LoadSettings(ctx, fakeConfigStore{}, "", TierBasic); s.Provider != ProviderNone {
		t.Errorf("basic tier provider = %q, want none", s.Provider)
	}
}

// What the owner sets in the dashboard has to outlast a restart without
// anyone editing a service definition.
func TestSaveAndLoadRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := fakeConfigStore{}

	err := SaveSettings(ctx, store, Settings{
		Provider: ProviderAnthropic,
		Model:    "claude-haiku-4-5-20251001",
		APIKey:   "sk-ant-secret",
	})
	if err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	got := LoadSettings(ctx, store, "", TierStandard)
	if got.Provider != ProviderAnthropic || got.APIKey != "sk-ant-secret" {
		t.Errorf("round trip = %+v", got)
	}
	// Nothing about the model settings may be marked for the control
	// plane; the key is the station's own secret.
	if store[ConfigKeyAPIKey] != "sk-ant-secret" {
		t.Errorf("key not stored: %q", store[ConfigKeyAPIKey])
	}
}

// The Settings page shows a masked key. Saving the form again must not
// wipe the real key just because the browser never had it.
func TestSaveKeepsTheExistingKey(t *testing.T) {
	ctx := context.Background()
	store := fakeConfigStore{ConfigKeyAPIKey: "sk-original"}

	if err := SaveSettings(ctx, store, Settings{
		Provider: ProviderOpenAI, Model: "gpt-4o-mini", APIKey: KeepExistingKey,
	}); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	if store[ConfigKeyAPIKey] != "sk-original" {
		t.Errorf("key = %q, want the original to be kept", store[ConfigKeyAPIKey])
	}
}

func TestSaveRejectsWhatCannotWork(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		in     Settings
		store  fakeConfigStore
		wantIn string
	}{
		{
			"hosted provider with no key",
			Settings{Provider: ProviderOpenAI, Model: "gpt-4o-mini"},
			fakeConfigStore{},
			"enter the API key",
		},
		{
			"a base url that is not a url",
			Settings{Provider: ProviderOpenAI, APIKey: "k", BaseURL: "api.openai.com"},
			fakeConfigStore{},
			"not a valid URL",
		},
		{
			"a key sent in the clear",
			Settings{Provider: ProviderOpenAI, APIKey: "k", BaseURL: "http://api.example.com/v1"},
			fakeConfigStore{},
			"use https://",
		},
		{
			"an unknown provider",
			Settings{Provider: Provider("magic")},
			fakeConfigStore{},
			"choose where answers should come from",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := SaveSettings(ctx, c.store, c.in)
			if err == nil {
				t.Fatal("expected a refusal")
			}
			if !strings.Contains(err.Error(), c.wantIn) {
				t.Errorf("error = %q, want it to mention %q", err, c.wantIn)
			}
		})
	}
}

// Plain http to a model on this same machine is fine: nothing leaves it.
func TestSaveAllowsLocalHTTP(t *testing.T) {
	err := SaveSettings(context.Background(), fakeConfigStore{}, Settings{
		Provider: ProviderOllama, BaseURL: "http://127.0.0.1:11434",
	})
	if err != nil {
		t.Errorf("a local Ollama over http was refused: %v", err)
	}
}
