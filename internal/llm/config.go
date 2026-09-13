package llm

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// local_config keys holding the station's model settings. The key lives
// in the station database, which sits in the ACL-restricted data folder
// and is never included in a heartbeat or in telemetry.
const (
	ConfigKeyProvider = "llm_provider"
	ConfigKeyModel    = "llm_model"
	ConfigKeyAPIKey   = "llm_api_key"
	ConfigKeyBaseURL  = "llm_base_url"
)

// ConfigStore is the slice of storage these helpers need.
type ConfigStore interface {
	LocalConfigValue(ctx context.Context, key, def string) string
	SetLocalConfig(ctx context.Context, key, value, configJSON string, synchronized bool) error
}

// LoadSettings reads the station's model settings, falling back to the
// values the service was started with. The database wins over the
// environment: the owner setting this in the dashboard is a deliberate
// act, and it must survive a restart without editing a service.
func LoadSettings(ctx context.Context, store ConfigStore, envOllamaURL string, tier Tier) Settings {
	p := Provider(strings.ToLower(strings.TrimSpace(store.LocalConfigValue(ctx, ConfigKeyProvider, ""))))
	if !p.Valid() || p == "" {
		// Nothing chosen yet. A station whose PC can run a model gets the
		// local one; a Basic-tier PC gets the fixed answers, because
		// running a model on it would make the dashboard unusable.
		p = ProviderOllama
		if tier == TierBasic {
			p = ProviderNone
		}
	}
	s := Settings{
		Provider: p,
		Model:    strings.TrimSpace(store.LocalConfigValue(ctx, ConfigKeyModel, "")),
		APIKey:   strings.TrimSpace(store.LocalConfigValue(ctx, ConfigKeyAPIKey, "")),
		BaseURL:  strings.TrimSpace(store.LocalConfigValue(ctx, ConfigKeyBaseURL, "")),
		Timeout:  30 * time.Second,
	}
	if s.Model == "" {
		s.Model = DefaultModelFor(p)
		if p == ProviderOllama {
			s.Model = ModelForTier(tier)
		}
	}
	if s.BaseURL == "" && p == ProviderOllama {
		s.BaseURL = envOllamaURL
	}
	return s
}

// SaveSettings validates and stores the owner's choice. An API key of
// KeepExistingKey means "leave the saved key alone", so the Settings
// page can show a masked key without ever having to send the real one
// back to the browser.
const KeepExistingKey = "\x00keep"

// SaveSettings writes the settings after checking them. It returns an
// error worded for the owner.
func SaveSettings(ctx context.Context, store ConfigStore, s Settings) error {
	if !s.Provider.Valid() {
		return fmt.Errorf("choose where answers should come from")
	}
	if s.BaseURL != "" {
		u, err := url.Parse(s.BaseURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("the address %q is not a valid URL. It should look like https://api.example.com/v1", s.BaseURL)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("the address must start with http:// or https://")
		}
		// A key sent over plain http to anywhere but this machine is
		// readable by everyone on the shop network.
		if u.Scheme == "http" && s.Provider.SendsDataOffSite() && !isLocalHost(u.Hostname()) {
			return fmt.Errorf("use https:// for %s — over plain http your API key would travel in the clear", u.Host)
		}
	}
	if s.Provider.SendsDataOffSite() {
		existing := store.LocalConfigValue(ctx, ConfigKeyAPIKey, "")
		if s.APIKey == KeepExistingKey {
			s.APIKey = existing
		}
		if strings.TrimSpace(s.APIKey) == "" {
			return fmt.Errorf("enter the API key for %s", s.Provider.Label())
		}
	}
	if s.APIKey == KeepExistingKey {
		s.APIKey = store.LocalConfigValue(ctx, ConfigKeyAPIKey, "")
	}

	for _, kv := range []struct{ k, v string }{
		{ConfigKeyProvider, string(s.Provider)},
		{ConfigKeyModel, strings.TrimSpace(s.Model)},
		{ConfigKeyAPIKey, s.APIKey},
		{ConfigKeyBaseURL, strings.TrimRight(strings.TrimSpace(s.BaseURL), "/")},
	} {
		// synchronized=false: none of this is ever sent to the control
		// plane. The API key in particular is the station's own secret.
		if err := store.SetLocalConfig(ctx, kv.k, kv.v, "", false); err != nil {
			return err
		}
	}
	return nil
}

func isLocalHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// MaskKey renders an API key for the screen. It shows just enough for
// the owner to recognise which key is saved, and never enough to use.
func MaskKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	if len(key) <= 8 {
		return strings.Repeat("*", len(key))
	}
	return key[:3] + strings.Repeat("*", 8) + key[len(key)-4:]
}
