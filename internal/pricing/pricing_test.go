package pricing

import (
	"math"
	"testing"

	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/providers"
)

func TestCost(t *testing.T) {
	u := providers.Usage{PromptTokens: 3000, CompletionTokens: 200, CacheWriteTokens: 500,
		PromptTokensDetails: &providers.PromptTokensDetails{CachedTokens: 2000}}
	// 500 uncached*0.10 + 2000 cached*0.01 + 500 write*0.125 + 200 out*0.50, per 1M
	want := (500*0.10 + 2000*0.01 + 500*0.125 + 200*0.50) / 1e6
	if got := Default.Cost("claude-haiku-5-5", u); math.Abs(got-want) > 1e-12 {
		t.Fatalf("got %v want %v", got, want)
	}
	if Default.Cost("gpt-4.1-mini-2025-04-14", u) == 0 {
		t.Fatal("dated snapshot should match its prefix")
	}
	if Default.Cost("llama3.2", u) != 0 {
		t.Fatal("unknown model should cost 0")
	}
}
