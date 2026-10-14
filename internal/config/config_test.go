package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestLoadYAMLThenEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "primary_provider: openai\nopenai:\n  api_key: from-yaml\nrelay:\n  stall_timeout: 3s\npricing:\n  my-model: {input: 1, output: 2}\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENAI_API_KEY", "from-env")
	t.Setenv("ANTHROPIC_API_KEY", "a")
	t.Setenv("FIRST_TOKEN_TIMEOUT", "750ms")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OpenAI.APIKey != "from-env" || cfg.Relay.StallTimeout != 3*time.Second || cfg.Relay.FirstTokenTimeout != 750*time.Millisecond {
		t.Fatalf("overrides not applied: %+v", cfg)
	}
	if cfg.Anthropic.Model != "claude-haiku-5-5" || !cfg.Anthropic.DisableThinking {
		t.Fatalf("anthropic defaults lost: %+v", cfg.Anthropic)
	}
	if got := cfg.ProviderOrder(); !slices.Equal(got, []string{"openai", "anthropic"}) {
		t.Fatalf("order = %v", got)
	}
	if p := cfg.Prices(); p["my-model"].Output != 2 || p["gpt-4.1-mini"].Input == 0 {
		t.Fatal("pricing overrides should merge over defaults")
	}
}

func TestLoadRejectsBadConfig(t *testing.T) {
	t.Setenv("PRIMARY_PROVIDER", "gemini")
	t.Setenv("GEMINI_API_KEY", "") // empty = unset
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("expected error for primary provider without api key")
	}
	t.Setenv("PRIMARY_PROVIDER", "openai")
	t.Setenv("OPENAI_API_KEY", "k")
	t.Setenv("PROXY_MODE", "fast")
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("expected error for unknown mode")
	}
}
