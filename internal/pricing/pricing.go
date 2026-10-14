// Package pricing turns provider usage into dollars.
package pricing

import (
	"strings"

	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/providers"
)

// Price is USD per million tokens. Zero CachedInput or CacheWrite means the
// provider bills those tokens at the Input price.
type Price struct {
	Input       float64 `yaml:"input" json:"input"`
	CachedInput float64 `yaml:"cached_input" json:"cached_input"`
	CacheWrite  float64 `yaml:"cache_write" json:"cache_write"`
	Output      float64 `yaml:"output" json:"output"`
}

// Table maps model names (or model-name prefixes) to prices.
type Table map[string]Price

// Default holds list prices at the time of writing; override them in config.
var Default = Table{
	"gpt-4.1-mini":     {Input: 0.40, CachedInput: 0.10, Output: 1.60},
	"gpt-4.1-nano":     {Input: 0.10, CachedInput: 0.025, Output: 0.40},
	"claude-haiku-5-5": {Input: 0.10, CachedInput: 0.01, CacheWrite: 0.125, Output: 0.50},
	"llama-3.1-8b":     {Input: 0.05, Output: 0.08},
	"gemini-2.5-flash": {Input: 0.30, Output: 2.50},
}

// Lookup finds model's price by exact name, then by the longest matching
// prefix (so dated snapshots like gpt-4.1-mini-2025-04-14 resolve).
func (t Table) Lookup(model string) (Price, bool) {
	if p, ok := t[model]; ok {
		return p, true
	}
	best, found := "", false
	for k := range t {
		if strings.HasPrefix(model, k) && len(k) > len(best) {
			best, found = k, true
		}
	}
	return t[best], found
}

// Cost prices one call. Unknown models cost 0 (local models, or a missing
// table entry, which the caller should log).
func (t Table) Cost(model string, u providers.Usage) float64 {
	p, ok := t.Lookup(model)
	if !ok {
		return 0
	}
	cached, write := u.Cached(), u.CacheWriteTokens
	uncached := max(0, u.PromptTokens-cached-write)
	or := func(v, fallback float64) float64 {
		if v == 0 {
			return fallback
		}
		return v
	}
	return (float64(uncached)*p.Input +
		float64(cached)*or(p.CachedInput, p.Input) +
		float64(write)*or(p.CacheWrite, p.Input) +
		float64(u.CompletionTokens)*p.Output) / 1e6
}
