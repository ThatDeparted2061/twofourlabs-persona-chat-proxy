package providers

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/fakellm"
)

// These tests run the real adapters against the simulated upstream, which
// speaks both wire formats.
func fakeUpstream(t *testing.T) string {
	srv := httptest.NewServer(fakellm.New(fakellm.Config{
		TimeScale: 1, OpenAIMachines: 4, OpenAIOverflow: 15, AnthropicMinimum: 512, CacheTTL: time.Minute,
		Latency: fakellm.Latency{Base: time.Millisecond, TokensPerSec: 1e6},
	}).Handler())
	t.Cleanup(srv.Close)
	return srv.URL + "/v1"
}

func collect(t *testing.T, p Provider, req ChatRequest) (string, Usage) {
	t.Helper()
	ch, err := p.ChatStream(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	var u Usage
	for c := range ch {
		if c.Err != nil {
			t.Fatal(c.Err)
		}
		text.WriteString(c.Content)
		if c.Usage != nil {
			u = *c.Usage
		}
	}
	return text.String(), u
}

func chatReq() ChatRequest {
	return ChatRequest{Messages: []Message{
		{Role: "system", Content: strings.Repeat("You are Mira, a warm confidant. ", 200), Cache: true},
		{Role: "user", Content: "hey, you up?", Cache: true},
	}, MaxTokens: 300, PromptCacheKey: "conv-1"}
}

func TestAnthropicStreamAgainstFake(t *testing.T) {
	p := NewAnthropic(fakeUpstream(t), "key", "claude-haiku-5-5", true)
	text, u := collect(t, p, chatReq())
	if !fakellm.Verify(text) {
		t.Fatalf("reply damaged: %q", text)
	}
	if u.CacheWriteTokens == 0 || u.Cached() != 0 || u.CompletionTokens == 0 {
		t.Fatalf("first call should write the cache: %+v", u)
	}
	_, u = collect(t, p, chatReq())
	if u.Cached() == 0 || u.CacheWriteTokens != 0 {
		t.Fatalf("second call should read the cache: %+v", u)
	}
	resp, err := p.Chat(context.Background(), chatReq())
	if err != nil || !fakellm.Verify(resp.Choices[0].Message.Content) || resp.Choices[0].FinishReason != "stop" {
		t.Fatalf("non-streaming chat: %v %+v", err, resp)
	}
}

func TestOpenAIStreamAgainstFake(t *testing.T) {
	p := NewOpenAI(fakeUpstream(t), "key", "gpt-4.1-mini")
	text, u := collect(t, p, chatReq())
	if !fakellm.Verify(text) || u.PromptTokens == 0 {
		t.Fatalf("bad first reply %q %+v", text, u)
	}
	_, u = collect(t, p, chatReq())
	if u.Cached() < 1024 {
		t.Fatalf("repeat prompt with the same key should be cached: %+v", u)
	}
}
