package web

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/fuelmind/fuelmind/internal/llm"
	"github.com/fuelmind/fuelmind/internal/storage"
)

// ModelConfigurator lets the dashboard change where answers come from
// without restarting the service. *llm.Router implements it.
type ModelConfigurator interface {
	Configure(s llm.Settings)
	HasModel() bool
}

// providerChoice is one option on the Settings page.
type providerChoice struct {
	Value       string
	Label       string
	Selected    bool
	OffSite     bool
	NeedsKey    bool
	Description string
}

// modelProviderChoices describes each way of answering open-ended
// questions, including what it costs the owner in privacy. The choice is
// theirs to make, so the page has to be honest about it.
func modelProviderChoices(current llm.Provider) []providerChoice {
	return []providerChoice{
		{
			Value: string(llm.ProviderNone), Label: "Set questions only",
			Selected:    current == llm.ProviderNone,
			Description: "Answers the fixed questions about sales, credit, margin and the score. Nothing ever leaves this PC.",
		},
		{
			Value: string(llm.ProviderOllama), Label: "A model on this PC (Ollama)",
			Selected:    current == llm.ProviderOllama,
			Description: "Answers questions in your own words using a model running on this machine. Nothing leaves the shop, but the PC needs the memory to run it.",
		},
		{
			Value: string(llm.ProviderOpenAI), Label: "OpenAI-compatible API",
			Selected: current == llm.ProviderOpenAI, OffSite: true, NeedsKey: true,
			Description: "Uses a hosted model. Works with OpenAI, and with anything that speaks the same API. Your question and the figures needed to answer it are sent to that provider.",
		},
		{
			Value: string(llm.ProviderAnthropic), Label: "Anthropic API",
			Selected: current == llm.ProviderAnthropic, OffSite: true, NeedsKey: true,
			Description: "Uses a hosted Claude model. Your question and the figures needed to answer it are sent to Anthropic.",
		},
	}
}

// formModelSettings reads the model settings off the Settings form.
//
// The API key is the careful part: the page shows a masked key, so a
// blank box means "keep the one you have" rather than "delete it". Only
// a box with something typed in it replaces the saved key.
func formModelSettings(r *http.Request) llm.Settings {
	s := llm.Settings{
		Provider: llm.Provider(strings.ToLower(strings.TrimSpace(r.FormValue("provider")))),
		Model:    strings.TrimSpace(r.FormValue("model")),
		BaseURL:  strings.TrimSpace(r.FormValue("base_url")),
		Timeout:  30 * time.Second,
	}
	key := strings.TrimSpace(r.FormValue("api_key"))
	if key == "" {
		s.APIKey = llm.KeepExistingKey
	} else {
		s.APIKey = key
	}
	if s.Model == "" {
		s.Model = llm.DefaultModelFor(s.Provider)
	}
	return s
}

// saveModelSettings stores the owner's choice and applies it to the
// running answerer, so the next question uses it.
func (s *Server) saveModelSettings(r *http.Request) (notice, formError string) {
	settings := formModelSettings(r)
	if err := llm.SaveSettings(r.Context(), s.store, settings); err != nil {
		return "", err.Error()
	}
	// Re-read, so what is applied is exactly what was saved — including
	// the existing key when the box was left blank.
	applied := s.reloadModel(r.Context())

	detail := fmt.Sprintf("Set answers to come from %s", applied.Provider.Label())
	if applied.Model != "" {
		detail += " using " + applied.Model
	}
	s.audit(r, storage.ActionModelChanged, string(applied.Provider), detail)

	if applied.Provider == llm.ProviderNone {
		return "Saved. FuelMind will answer the set questions only.", ""
	}
	if applied.Provider.SendsDataOffSite() {
		return fmt.Sprintf(
			"Saved. Questions in your own words now go to %s. Press Test to check the key works.",
			applied.Provider.Label()), ""
	}
	return "Saved. Press Test to check the model on this PC is reachable.", ""
}

// testModel checks the configured model actually answers, so the owner
// finds out here rather than the first time they ask a real question
// from their phone.
func (s *Server) testModel(r *http.Request) (notice, formError string) {
	settings := llm.LoadSettings(r.Context(), s.store, "", llm.Tier(s.hardwareTier(r.Context())))
	if settings.Provider == llm.ProviderNone {
		return "", "There is nothing to test: FuelMind is set to answer the set questions only."
	}
	client := llm.NewClient(settings)
	if client == nil {
		return "", "That provider needs an API key before it can be tested."
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := client.Ping(ctx); err != nil {
		if settings.Provider == llm.ProviderOllama {
			return "", fmt.Sprintf(
				"Could not reach a model on this PC (%v). Check Ollama is installed and running, then try again.", err)
		}
		return "", fmt.Sprintf("Could not use that key: %v", err)
	}
	return fmt.Sprintf("%s answered. Questions in your own words will work.", settings.Provider.Label()), ""
}

// reloadModel re-reads the saved settings and hands them to the running
// answerer. Returns what was applied.
func (s *Server) reloadModel(ctx context.Context) llm.Settings {
	settings := llm.LoadSettings(ctx, s.store, "", llm.Tier(s.hardwareTier(ctx)))
	if s.Model != nil {
		s.Model.Configure(settings)
	}
	return settings
}

// hardwareTier is what this PC was measured as, used to pick a sensible
// local model when the owner has not named one.
func (s *Server) hardwareTier(ctx context.Context) string {
	return s.store.LocalConfigValue(ctx, "hardware_tier", "standard")
}
