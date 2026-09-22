package inference

import "testing"

func TestDefaultModelForProviders_UsesCurrentShippedDefaults(t *testing.T) {
	cat, err := LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		providers []string
		want      string
	}{
		{[]string{"anthropic"}, "anthropic/claude-opus-5-5"},
		{[]string{"openai"}, "openai/gpt-6-sol"},
		{[]string{"google"}, "google/gemini-3.1-pro-preview"},
		{[]string{"unknown", "anthropic"}, "anthropic/claude-opus-5-5"},
		{nil, ""},
	}
	for _, tc := range cases {
		got, err := DefaultModelForProviders(cat, tc.providers)
		if err != nil {
			t.Fatalf("DefaultModelForProviders(%v): %v", tc.providers, err)
		}
		if got != tc.want {
			t.Errorf("DefaultModelForProviders(%v) = %q, want %q", tc.providers, got, tc.want)
		}
	}
}

func TestShippedOpenAIChoicesUseGPT6(t *testing.T) {
	cat, err := LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"openai/gpt-6-astra": true, "openai/gpt-6-sol": true, "openai/gpt-6-luna": true}
	for _, model := range cat.Models {
		if model.Provider == "openai" && model.Available && !want[model.ID] {
			t.Errorf("available OpenAI model = %q, want GPT-6 family", model.ID)
		}
	}
}

func TestValidateCatalog_DefaultMustBeUniqueAvailableAndCloud(t *testing.T) {
	base := Model{ID: "anthropic/a", Provider: "anthropic", Label: "A", ContextWindow: 100, MaxOutputTokens: 10, Available: true, Default: true}
	cases := []struct {
		name   string
		models []Model
	}{
		{"duplicate", []Model{base, {ID: "anthropic/b", Provider: "anthropic", Label: "B", ContextWindow: 100, MaxOutputTokens: 10, Available: true, Default: true}}},
		{"retired", []Model{{ID: "anthropic/a", Provider: "anthropic", Label: "A", ContextWindow: 100, MaxOutputTokens: 10, Default: true}}},
		{"local", []Model{{ID: "ollama/a", Provider: "ollama", Label: "A", ContextWindow: 100, MaxOutputTokens: 10, Available: true, Local: true, Default: true}}},
	}
	for _, tc := range cases {
		if err := ValidateCatalog(&Catalog{Models: tc.models}); err == nil {
			t.Errorf("%s default was accepted", tc.name)
		}
	}
}
