package budget

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// redisForTest returns a client for REDIS_URL (default localhost:6379), or skips.
func redisForTest(t *testing.T) *redis.Client {
	url := os.Getenv("REDIS_URL")
	if url == "" {
		url = "redis://localhost:6379/0"
	}
	opt, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(opt)
	if rdb.Ping(context.Background()).Err() != nil {
		t.Skip("no Redis at " + url)
	}
	return rdb
}

func TestBudgetStates(t *testing.T) {
	ctx := context.Background()
	b := New(redisForTest(t), "test-"+strconv.FormatInt(time.Now().UnixNano(), 36)+":", 100, 150)
	for _, step := range []struct {
		spend int64
		want  State
	}{{0, OK}, {99, OK}, {1, Soft}, {49, Soft}, {1, Hard}} {
		if err := b.Spend(ctx, "u1", step.spend); err != nil {
			t.Fatal(err)
		}
		if got, used, _ := b.Check(ctx, "u1"); got != step.want {
			t.Fatalf("after spending to %d: got %s want %s", used, got, step.want)
		}
	}
	if s, _, _ := b.Check(ctx, "someone-else"); s != OK {
		t.Fatal("budgets are per user")
	}
}
