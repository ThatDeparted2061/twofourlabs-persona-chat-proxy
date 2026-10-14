package history

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/providers"
	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/tok"
)

func TestRollingSummary(t *testing.T) {
	url := os.Getenv("REDIS_URL")
	if url == "" {
		url = "redis://localhost:6379/0"
	}
	opt, _ := redis.ParseURL(url)
	rdb := redis.NewClient(opt)
	if rdb.Ping(context.Background()).Err() != nil {
		t.Skip("no Redis")
	}
	s := New(rdb, "test-"+strconv.FormatInt(time.Now().UnixNano(), 36)+":", 1000, 300)
	calls := 0
	s.Complete = func(_ context.Context, req providers.ChatRequest) (*providers.ChatResponse, error) {
		calls++
		return &providers.ChatResponse{Choices: []providers.Choice{{Message: providers.Message{Content: "They talked about lighthouses."}}}}, nil
	}

	var h []providers.Message
	for i := range 20 { // 20 messages x ~100 tokens = ~2000 tokens
		role := map[bool]string{true: "user", false: "assistant"}[i%2 == 0]
		h = append(h, providers.Message{Role: role, Content: strings.Repeat("word ", 80) + strconv.Itoa(i)})
	}
	if got, sum := s.Apply(context.Background(), "c1", h); sum != nil || len(got) != len(h) {
		t.Fatal("no summary yet: history must pass through")
	}
	s.After("c1", "Mira", h)
	deadline := time.Now().Add(2 * time.Second)
	for {
		got, sum := s.Apply(context.Background(), "c1", h)
		if sum != nil {
			n := 0
			for _, m := range got {
				n += tok.Count(m.Content)
			}
			if n > 300 || got[0].Role != "user" || !strings.HasPrefix(sum.Content, Prefix) {
				t.Fatalf("kept %d tokens starting with %s", n, got[0].Role)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("summary never appeared")
		}
		time.Sleep(10 * time.Millisecond)
	}
	edited := append([]providers.Message{{Role: "user", Content: "edited"}}, h[1:]...)
	if _, sum := s.Apply(context.Background(), "c1", edited); sum != nil {
		t.Fatal("a rewritten transcript must not get the old summary")
	}
	if calls != 1 {
		t.Fatalf("want 1 summarization call, got %d", calls)
	}
}
