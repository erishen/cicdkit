package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadDotEnvReadsDotEnv writes a .env into a temp dir, chdirs there, and
// verifies LoadDotEnv exports the keys into the process environment. This is
// the exact path the running server relies on to surface CICD_LLM_* values in
// the UI.
func TestLoadDotEnvReadsDotEnv(t *testing.T) {
	dir := t.TempDir()
	// Remove any pre-existing CICD_LLM_* from the real env so the file can set
	// them (LoadDotEnv skips keys already present in the process env). We use
	// os.Unsetenv rather than t.Setenv("","") which would leave the key present.
	for _, k := range []string{"CICD_LLM_ENABLED", "CICD_LLM_BASE_URL", "CICD_LLM_API_KEY", "CICD_LLM_MODEL"} {
		os.Unsetenv(k)
	}
	env := "CICD_LLM_ENABLED=true\nCICD_LLM_BASE_URL=https://example.com/v1\nCICD_LLM_API_KEY=sk-test\nCICD_LLM_MODEL=test-model\n"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}

	old, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old)

	if err := LoadDotEnv(); err != nil {
		t.Fatalf("LoadDotEnv error: %v", err)
	}

	if got := os.Getenv("CICD_LLM_BASE_URL"); got != "https://example.com/v1" {
		t.Errorf("CICD_LLM_BASE_URL = %q, want https://example.com/v1", got)
	}
	if got := os.Getenv("CICD_LLM_MODEL"); got != "test-model" {
		t.Errorf("CICD_LLM_MODEL = %q, want test-model", got)
	}
	if got := os.Getenv("CICD_LLM_ENABLED"); got != "true" {
		t.Errorf("CICD_LLM_ENABLED = %q, want true", got)
	}
}
