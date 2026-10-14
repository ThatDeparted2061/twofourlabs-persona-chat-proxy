// Package config loads proxy settings from an optional YAML file, then applies
// environment-variable overrides on top.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/pricing"
)

// Config is the full proxy configuration.
type Config struct {
	Port int `yaml:"port"`
	// Mode is "tuned" (everything this fork adds) or "baseline" (how
	// LLM-Gateway streamed: no cache hints, no summaries, failover only
	// before the first byte). Baseline exists to measure the difference.
	Mode            string `yaml:"mode"`
	PrimaryProvider string `yaml:"primary_provider"` // openai | anthropic | groq | gemini | ollama
	// APIKeys are the client keys allowed to call the proxy. Empty means open access.
	APIKeys []string `yaml:"api_keys"`

	RedisURL    string `yaml:"redis_url"`
	RedisPrefix string `yaml:"redis_prefix"` // namespaces every key, e.g. per environment

	OpenAI    ProviderConfig `yaml:"openai"`
	Anthropic ProviderConfig `yaml:"anthropic"`
	Groq      ProviderConfig `yaml:"groq"`
	Gemini    ProviderConfig `yaml:"gemini"`
	Ollama    ProviderConfig `yaml:"ollama"`

	Relay     RelayConfig     `yaml:"relay"`
	Budget    BudgetConfig    `yaml:"budget"`
	History   HistoryConfig   `yaml:"history"`
	RateLimit RateLimitConfig `yaml:"rate_limit"`
	Pricing   pricing.Table   `yaml:"pricing"` // merged over pricing.Default
}

// ProviderConfig holds one upstream's settings. Model is the default used when a
// request doesn't name one (and always on fallback, since model names are provider-specific).
type ProviderConfig struct {
	APIKey  string `yaml:"api_key"`
	BaseURL string `yaml:"base_url"`
	Model   string `yaml:"model"`
	// DisableThinking turns off extended thinking (Anthropic) to keep TTFT low.
	DisableThinking bool `yaml:"disable_thinking"`
}

// RelayConfig tunes streaming failover.
type RelayConfig struct {
	FirstTokenTimeout time.Duration `yaml:"first_token_timeout"`
	StallTimeout      time.Duration `yaml:"stall_timeout"`
	MaxAttempts       int           `yaml:"max_attempts"`
}

// BudgetConfig is the per-user daily token budget. Zero disables a limit.
type BudgetConfig struct {
	SoftTokens    int64  `yaml:"soft_tokens"`
	HardTokens    int64  `yaml:"hard_tokens"`
	SoftMaxTokens int    `yaml:"soft_max_tokens"` // reply cap once over the soft limit
	SoftModel     string `yaml:"soft_model"`      // cheaper primary-provider model once over the soft limit
}

// HistoryConfig controls rolling summaries.
type HistoryConfig struct {
	TriggerTokens int `yaml:"trigger_tokens"` // summarize when unsummarized history passes this
	KeepTokens    int `yaml:"keep_tokens"`    // newest history kept verbatim
}

// RateLimitConfig is a per-user token bucket: each request costs one token.
type RateLimitConfig struct {
	TokensPerSec float64 `yaml:"tokens_per_sec"`
	Burst        int     `yaml:"burst"`
}

// Default returns the configuration used for anything the YAML file and env leave unset.
func Default() *Config {
	return &Config{
		Port:            8080,
		Mode:            "tuned",
		PrimaryProvider: "openai",
		RedisURL:        "redis://localhost:6379/0",
		OpenAI:          ProviderConfig{BaseURL: "https://api.openai.com/v1", Model: "gpt-4.1-mini"},
		Anthropic:       ProviderConfig{BaseURL: "https://api.anthropic.com/v1", Model: "claude-haiku-5-5", DisableThinking: true},
		Groq:            ProviderConfig{BaseURL: "https://api.groq.com/openai/v1", Model: "llama-3.1-8b-instant"},
		Gemini:          ProviderConfig{BaseURL: "https://generativelanguage.googleapis.com/v1beta", Model: "gemini-2.5-flash"},
		Ollama:          ProviderConfig{Model: "llama3.2"}, // set base_url to enable
		Relay:           RelayConfig{FirstTokenTimeout: 2 * time.Second, StallTimeout: 1500 * time.Millisecond, MaxAttempts: 4},
		Budget:          BudgetConfig{SoftTokens: 400_000, HardTokens: 500_000, SoftMaxTokens: 150},
		History:         HistoryConfig{TriggerTokens: 3000, KeepTokens: 1200},
		RateLimit:       RateLimitConfig{TokensPerSec: 2, Burst: 10},
	}
}

// Load reads path (a missing file is fine: env-only config, e.g. in Docker),
// applies env overrides, and validates the result.
func Load(path string) (*Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	if err := cfg.applyEnv(); err != nil {
		return nil, err
	}
	return cfg, cfg.validate()
}

// Prices is the default price table with the configured overrides on top.
func (c *Config) Prices() pricing.Table {
	t := pricing.Table{}
	for k, v := range pricing.Default {
		t[k] = v
	}
	for k, v := range c.Pricing {
		t[k] = v
	}
	return t
}

func (c *Config) applyEnv() error {
	str := func(dst *string, key string) {
		if v := os.Getenv(key); v != "" {
			*dst = v
		}
	}
	str(&c.Mode, "PROXY_MODE")
	str(&c.PrimaryProvider, "PRIMARY_PROVIDER")
	str(&c.RedisURL, "REDIS_URL")
	str(&c.RedisPrefix, "REDIS_PREFIX")
	for _, p := range []struct {
		env string
		cfg *ProviderConfig
	}{{"OPENAI", &c.OpenAI}, {"ANTHROPIC", &c.Anthropic}, {"GROQ", &c.Groq}, {"GEMINI", &c.Gemini}, {"OLLAMA", &c.Ollama}} {
		str(&p.cfg.APIKey, p.env+"_API_KEY")
		str(&p.cfg.BaseURL, p.env+"_BASE_URL")
		str(&p.cfg.Model, p.env+"_MODEL")
	}
	str(&c.Budget.SoftModel, "BUDGET_SOFT_MODEL")
	if v := os.Getenv("GATEWAY_API_KEYS"); v != "" {
		c.APIKeys = strings.Split(v, ",")
	}

	var errs []error
	parse := func(key string, set func(string) error) {
		if v := os.Getenv(key); v != "" {
			if err := set(v); err != nil {
				errs = append(errs, fmt.Errorf("env %s: %w", key, err))
			}
		}
	}
	dur := func(dst *time.Duration) func(string) error {
		return func(v string) (err error) { *dst, err = time.ParseDuration(v); return }
	}
	num := func(dst *int) func(string) error {
		return func(v string) (err error) { *dst, err = strconv.Atoi(v); return }
	}
	num64 := func(dst *int64) func(string) error {
		return func(v string) (err error) { *dst, err = strconv.ParseInt(v, 10, 64); return }
	}
	parse("GATEWAY_PORT", num(&c.Port))
	parse("FIRST_TOKEN_TIMEOUT", dur(&c.Relay.FirstTokenTimeout))
	parse("STALL_TIMEOUT", dur(&c.Relay.StallTimeout))
	parse("MAX_ATTEMPTS", num(&c.Relay.MaxAttempts))
	parse("BUDGET_SOFT_TOKENS", num64(&c.Budget.SoftTokens))
	parse("BUDGET_HARD_TOKENS", num64(&c.Budget.HardTokens))
	parse("SUMMARY_TRIGGER_TOKENS", num(&c.History.TriggerTokens))
	parse("SUMMARY_KEEP_TOKENS", num(&c.History.KeepTokens))
	parse("RATE_LIMIT_TPS", func(v string) (err error) { c.RateLimit.TokensPerSec, err = strconv.ParseFloat(v, 64); return })
	parse("RATE_LIMIT_BURST", num(&c.RateLimit.Burst))
	return errors.Join(errs...)
}

func (c *Config) validate() error {
	var errs []error
	if c.Port < 1 || c.Port > 65535 {
		errs = append(errs, fmt.Errorf("port %d out of range", c.Port))
	}
	if c.Mode != "tuned" && c.Mode != "baseline" {
		errs = append(errs, fmt.Errorf("mode %q must be tuned or baseline", c.Mode))
	}
	if !c.Enabled(c.PrimaryProvider) {
		errs = append(errs, fmt.Errorf("primary_provider %q is unknown or not configured (openai/anthropic/groq/gemini need an api_key, ollama a base_url)", c.PrimaryProvider))
	}
	if c.RateLimit.TokensPerSec <= 0 || c.RateLimit.Burst < 1 {
		errs = append(errs, errors.New("rate_limit.tokens_per_sec must be > 0 and burst >= 1"))
	}
	if c.Relay.MaxAttempts < 1 {
		errs = append(errs, errors.New("relay.max_attempts must be >= 1"))
	}
	if c.Budget.HardTokens > 0 && c.Budget.SoftTokens > c.Budget.HardTokens {
		errs = append(errs, errors.New("budget.soft_tokens must not exceed budget.hard_tokens"))
	}
	if c.History.KeepTokens >= c.History.TriggerTokens && c.History.TriggerTokens > 0 {
		errs = append(errs, errors.New("history.keep_tokens must be below history.trigger_tokens"))
	}
	return errors.Join(errs...)
}

// Enabled reports whether a provider has enough configuration to be called.
func (c *Config) Enabled(name string) bool {
	switch name {
	case "openai":
		return c.OpenAI.APIKey != ""
	case "anthropic":
		return c.Anthropic.APIKey != ""
	case "groq":
		return c.Groq.APIKey != ""
	case "gemini":
		return c.Gemini.APIKey != ""
	case "ollama":
		return c.Ollama.BaseURL != ""
	}
	return false
}

// ProviderOrder is the failover order: the primary first, then every other
// configured provider.
func (c *Config) ProviderOrder() []string {
	order := []string{c.PrimaryProvider}
	for _, name := range []string{"openai", "anthropic", "groq", "gemini", "ollama"} {
		if name != c.PrimaryProvider && c.Enabled(name) {
			order = append(order, name)
		}
	}
	return order
}
