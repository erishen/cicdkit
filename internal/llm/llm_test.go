package llm

import (
	"os"
	"path/filepath"
	"testing"
)

// clear LLM env so tests don't depend on the operator's local .env.
func clearLLMEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{envLLMEnabled, envLLMBaseURL, envLLMAPIKey, envLLMModel} {
		t.Setenv(k, "")
	}
}

func TestApplyEnvFallbackFillsEmpty(t *testing.T) {
	clearLLMEnv(t)
	t.Setenv(envLLMBaseURL, "https://api.example.com/v1")
	t.Setenv(envLLMAPIKey, "sk-secret")
	t.Setenv(envLLMModel, "gpt-4o-mini")
	t.Setenv(envLLMEnabled, "true")

	c := &Config{}
	applyEnvFallback(c)
	if c.BaseURL != "https://api.example.com/v1" {
		t.Errorf("BaseURL = %q", c.BaseURL)
	}
	if c.APIKey != "sk-secret" {
		t.Errorf("APIKey = %q", c.APIKey)
	}
	if c.Model != "gpt-4o-mini" {
		t.Errorf("Model = %q", c.Model)
	}
	if !c.Enabled {
		t.Errorf("Enabled = false, want true")
	}
}

func TestApplyEnvFallbackPersistedWins(t *testing.T) {
	clearLLMEnv(t)
	t.Setenv(envLLMBaseURL, "https://env.example.com/v1")
	t.Setenv(envLLMModel, "env-model")

	// A value already in the config (e.g. from llm.json) must beat the env default.
	c := &Config{BaseURL: "https://file.example.com/v1", Model: "file-model"}
	applyEnvFallback(c)
	if c.BaseURL != "https://file.example.com/v1" {
		t.Errorf("BaseURL overridden by env: %q", c.BaseURL)
	}
	if c.Model != "file-model" {
		t.Errorf("Model overridden by env: %q", c.Model)
	}
}

func TestLoadEnvFallback(t *testing.T) {
	clearLLMEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "llm.json")
	// llm.json has only enabled; base_url/model come from env.
	if err := os.WriteFile(path, []byte(`{"enabled":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envLLMBaseURL, "https://api.example.com/v1")
	t.Setenv(envLLMModel, "gpt-4o-mini")

	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Enabled {
		t.Errorf("Enabled = false, want true (from file)")
	}
	if c.BaseURL != "https://api.example.com/v1" {
		t.Errorf("BaseURL = %q, want env value", c.BaseURL)
	}
	if c.Model != "gpt-4o-mini" {
		t.Errorf("Model = %q, want env value", c.Model)
	}
}

func TestMaskedDoesNotLeakKey(t *testing.T) {
	c := Config{APIKey: "sk-topsecret", BaseURL: "https://x/v1", Model: "m"}
	m := c.Masked()
	if _, ok := m["api_key"]; ok {
		t.Errorf("Masked() must not expose api_key")
	}
	if m["api_key_set"] != true {
		t.Errorf("api_key_set = %v, want true", m["api_key_set"])
	}
}

// TestLoadEnvFallbackWhenFileMissing covers the fresh-install case: there is no
// data/llm.json yet, so Load must still apply the CICD_LLM_* env fallback. This
// guards the bug where the fallback only ran after a successful file unmarshal.
func TestLoadEnvFallbackWhenFileMissing(t *testing.T) {
	clearLLMEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist.json")
	t.Setenv(envLLMBaseURL, "https://api.example.com/v1")
	t.Setenv(envLLMModel, "gpt-4o-mini")
	t.Setenv(envLLMEnabled, "true")

	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Enabled {
		t.Errorf("Enabled = false, want true (from env)")
	}
	if c.BaseURL != "https://api.example.com/v1" {
		t.Errorf("BaseURL = %q, want env value", c.BaseURL)
	}
	if c.Model != "gpt-4o-mini" {
		t.Errorf("Model = %q, want env value", c.Model)
	}
}
