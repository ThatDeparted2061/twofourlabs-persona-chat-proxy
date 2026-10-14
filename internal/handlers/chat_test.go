package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/budget"
	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/fakellm"
	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/persona"
	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/pricing"
	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/providers"
	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/ratelimit"
	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/router"
)

func upstream(t testing.TB, f fakellm.Faults) string {
	srv := httptest.NewServer(fakellm.New(fakellm.Config{
		TimeScale: 1, OpenAIMachines: 2, OpenAIOverflow: 15, AnthropicMinimum: 512, CacheTTL: time.Minute,
		Latency: fakellm.Latency{Base: time.Millisecond, TokensPerSec: 5000}, Faults: f,
	}).Handler())
	t.Cleanup(srv.Close)
	return srv.URL + "/v1"
}

func newChat(t testing.TB, tuned bool, primaryFaults fakellm.Faults) *Chat {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &Chat{
		Router: router.New(
			providers.NewOpenAI(upstream(t, primaryFaults), "k", "gpt-4.1-mini"),
			providers.NewAnthropic(upstream(t, fakellm.Faults{}), "k", "claude-haiku-5-5", true)),
		Limiter:       ratelimit.New(ctx, 100, 100),
		Prices:        pricing.Default,
		Tuned:         tuned,
		Relay:         router.RelayOptions{FirstTokenTimeout: 300 * time.Millisecond, StallTimeout: 200 * time.Millisecond, MaxAttempts: 4, Resume: true},
		SoftMaxTokens: 100,
	}
}

func post(h http.Handler, key, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+key)
	r.Header.Set("X-User-ID", "u1")
	r.Header.Set("X-Conversation-ID", "c1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

const chatBody = `{"stream":true,"max_tokens":400,"messages":[{"role":"system","content":"You are Kai."},{"role":"user","content":"tell me a story"}]}`

// readStream returns the streamed text, whether it ended with [DONE], whether
// an in-band error arrived, and the proxy's final x_proxy block.
func readStream(t *testing.T, body string) (text string, done, failed bool, xp map[string]any) {
	for _, line := range strings.Split(body, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		if data == "[DONE]" {
			done = true
			continue
		}
		var c struct {
			Error   any `json:"error"`
			Choices []struct {
				Delta struct{ Content string } `json:"delta"`
			} `json:"choices"`
			XProxy map[string]any `json:"x_proxy"`
		}
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			t.Fatalf("bad chunk %q: %v", data, err)
		}
		if c.Error != nil {
			failed = true
		}
		if len(c.Choices) > 0 {
			text += c.Choices[0].Delta.Content
		}
		if c.XProxy != nil {
			xp = c.XProxy
		}
	}
	return
}

func TestStreamReportsUsageAndCost(t *testing.T) {
	w := post(newChat(t, true, fakellm.Faults{}), "k", chatBody)
	text, done, failed, xp := readStream(t, w.Body.String())
	if w.Code != 200 || !done || failed || !fakellm.Verify(text) {
		t.Fatalf("code=%d done=%v failed=%v text=%q", w.Code, done, failed, text)
	}
	if xp["provider"] != "openai" || xp["cost_usd"].(float64) <= 0 {
		t.Fatalf("x_proxy = %v", xp)
	}
}

func TestTunedSurvivesStallBaselineDoesNot(t *testing.T) {
	stall := fakellm.Faults{StallMid: 1}
	w := post(newChat(t, true, stall), "k", chatBody)
	text, done, failed, xp := readStream(t, w.Body.String())
	if !done || failed || !fakellm.Verify(text) || xp["provider"] != "anthropic" {
		t.Fatalf("tuned should resume on the backup: done=%v failed=%v provider=%v", done, failed, xp["provider"])
	}

	base := newChat(t, false, stall)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody)).WithContext(ctx)
	bw := httptest.NewRecorder()
	base.ServeHTTP(bw, r) // the baseline has no stall timer: it hangs until the client gives up
	if text, done, _, _ := readStream(t, bw.Body.String()); done || fakellm.Verify(text) {
		t.Fatal("baseline should not complete a stalled stream")
	}
}

func TestAssembleMarksCacheBreakpoints(t *testing.T) {
	h := newChat(t, true, fakellm.Faults{})
	var req chatRequest
	json.Unmarshal([]byte(chatBody), &req)
	tr, err := h.assemble(context.Background(), req, "c1", "u1")
	if err != nil {
		t.Fatal(err)
	}
	m := tr.req.Messages
	if !m[0].Cache || !m[len(m)-1].Cache || tr.req.PromptCacheKey != "c1" {
		t.Fatalf("tuned mode should cache the persona and newest message, keyed by conversation: %+v key=%q", m, tr.req.PromptCacheKey)
	}
	h.Tuned = false
	tr, _ = h.assemble(context.Background(), req, "c1", "u1")
	for _, msg := range tr.req.Messages {
		if msg.Cache || tr.req.PromptCacheKey != "" {
			t.Fatal("baseline must send no cache hints")
		}
	}
}

func TestRejectsUnknownKeyBadBodyAndFlood(t *testing.T) {
	h := newChat(t, true, fakellm.Faults{})
	h.APIKeys = map[string]bool{"good": true}
	if w := post(h, "bad", chatBody); w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
	if w := post(h, "good", `{"messages":[]}`); w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.Limiter = ratelimit.New(ctx, 0.001, 1)
	post(h, "good", `{"messages":[{"role":"user","content":"hi"}]}`)
	if w := post(h, "good", `{"messages":[{"role":"user","content":"hi"}]}`); w.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429, got %d", w.Code)
	}
}

func redisOrSkip(t *testing.T) (*redis.Client, string) {
	url := os.Getenv("REDIS_URL")
	if url == "" {
		url = "redis://localhost:6379/0"
	}
	opt, _ := redis.ParseURL(url)
	rdb := redis.NewClient(opt)
	if rdb.Ping(context.Background()).Err() != nil {
		t.Skip("no Redis")
	}
	return rdb, "test-" + strconv.FormatInt(time.Now().UnixNano(), 36) + ":"
}

func TestPersonaAndBudget(t *testing.T) {
	rdb, prefix := redisOrSkip(t)
	h := newChat(t, true, fakellm.Faults{})
	h.Personas = persona.New(rdb, prefix)
	h.Budget = budget.New(rdb, prefix, 50, 100_000)
	h.Personas.Put(context.Background(), "mira", "Mira", strings.Repeat("You are Mira, a warm confidant. ", 100))

	body := `{"stream":true,"persona_id":"mira","messages":[{"role":"user","content":"hey"}]}`
	w := post(h, "k", body)
	if text, done, _, _ := readStream(t, w.Body.String()); !done || !fakellm.Verify(text) || w.Header().Get("X-Budget-State") != "ok" {
		t.Fatalf("persona turn failed: %d %s", w.Code, w.Body)
	}
	w = post(h, "k", body) // the first turn spent more than the 50-token soft limit
	if w.Header().Get("X-Budget-State") != "soft" {
		t.Fatalf("want soft budget state, got %q", w.Header().Get("X-Budget-State"))
	}
	if w = post(h, "k", `{"persona_id":"nobody","messages":[{"role":"user","content":"hi"}]}`); w.Code != http.StatusNotFound {
		t.Fatalf("unknown persona: want 404, got %d", w.Code)
	}
	h.Budget.Hard = 60
	if w = post(h, "k", body); w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "budget_exceeded") {
		t.Fatalf("want 429 budget_exceeded, got %d %s", w.Code, w.Body)
	}
}
