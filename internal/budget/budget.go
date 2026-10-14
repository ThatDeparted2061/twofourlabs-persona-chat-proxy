// Package budget enforces a per-user daily token budget in Redis.
package budget

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// State is where a user stands against today's budget.
type State string

const (
	OK   State = "ok"
	Soft State = "soft" // over the soft limit: serve a cheaper, shorter reply
	Hard State = "hard" // over the hard limit: refuse until tomorrow (UTC)
)

// Budget counts tokens (prompt + completion, as the provider reported them)
// per user per UTC day.
type Budget struct {
	rdb        *redis.Client
	prefix     string
	Soft, Hard int64
}

func New(rdb *redis.Client, prefix string, soft, hard int64) *Budget {
	return &Budget{rdb: rdb, prefix: prefix + "budget:", Soft: soft, Hard: hard}
}

func (b *Budget) key(user string, now time.Time) string {
	return b.prefix + user + ":" + now.UTC().Format("2006-01-02")
}

// Check reports the user's state and tokens used today.
// ponytail: check-then-spend, so concurrent turns from one user can overshoot
// the hard limit by at most the turns already in flight. Reserve tokens up
// front with a Lua script if the limit must be exact.
func (b *Budget) Check(ctx context.Context, user string) (State, int64, error) {
	used, err := b.rdb.Get(ctx, b.key(user, time.Now())).Int64()
	if err != nil && err != redis.Nil {
		return OK, 0, err
	}
	switch {
	case b.Hard > 0 && used >= b.Hard:
		return Hard, used, nil
	case b.Soft > 0 && used >= b.Soft:
		return Soft, used, nil
	}
	return OK, used, nil
}

// Spend adds tokens to today's total.
func (b *Budget) Spend(ctx context.Context, user string, tokens int64) error {
	if tokens <= 0 {
		return nil
	}
	k := b.key(user, time.Now())
	pipe := b.rdb.TxPipeline()
	pipe.IncrBy(ctx, k, tokens)
	pipe.Expire(ctx, k, 48*time.Hour)
	_, err := pipe.Exec(ctx)
	return err
}
