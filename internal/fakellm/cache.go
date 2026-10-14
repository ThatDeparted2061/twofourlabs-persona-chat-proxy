package fakellm

import (
	"hash/fnv"
	"math/rand/v2"
	"sync"
	"time"
)

// clock runs simulated time Scale times faster than the wall clock. Only the
// gaps between turns are compressed in a benchmark (a user who replies after
// 40 s replies after 2 s), so cache TTLs and per-minute rate windows have to be
// compressed by the same factor to behave like production.
type clock struct {
	start time.Time
	scale float64
}

func (c clock) now() time.Time {
	return c.start.Add(time.Duration(float64(time.Since(c.start)) * c.scale))
}

// openAICache models OpenAI's automatic prompt caching as documented in
// https://platform.openai.com/docs/guides/prompt-caching:
//   - prompts of 1024+ tokens are cached; hits come in 128-token steps;
//   - a request is routed to a machine by a hash of its first ~256 tokens,
//     combined with prompt_cache_key when the caller sets one;
//   - when one prefix+key combination exceeds ~15 requests per minute, the
//     overflow spills to other machines, which do not have its cache;
//   - entries live about 5-10 minutes after last use.
//
// The overflow rule is why a key matters for character chat: hundreds of
// sessions share each persona's first 256 tokens, so without a key they all
// collide on one routing slot, overflow, and lose their history cache.
type openAICache struct {
	mu          sync.Mutex // ponytail: one lock for every machine; a few µs per request
	machines    []map[uint64]time.Time
	recent      map[uint64][]time.Time // routing key -> request times in the last minute
	overflowRPM int
	ttl         time.Duration
}

func newOpenAICache(machines, overflowRPM int, ttl time.Duration) *openAICache {
	c := &openAICache{recent: map[uint64][]time.Time{}, overflowRPM: overflowRPM, ttl: ttl}
	for range machines {
		c.machines = append(c.machines, map[uint64]time.Time{})
	}
	return c
}

// serve returns how many tokens of prompt were already cached on the machine
// the request lands on, and caches the rest of it there.
func (c *openAICache) serve(prompt []byte, cacheKey string, now time.Time) (cached int) {
	route := fnv.New64a()
	route.Write(prompt[:min(len(prompt), 256*4)])
	route.Write([]byte{0})
	route.Write([]byte(cacheKey))
	key := route.Sum64()

	c.mu.Lock()
	defer c.mu.Unlock()
	ts := c.recent[key]
	for len(ts) > 0 && now.Sub(ts[0]) > time.Minute {
		ts = ts[1:]
	}
	c.recent[key] = append(ts, now)
	m := c.machines[key%uint64(len(c.machines))]
	if len(ts) >= c.overflowRPM {
		m = c.machines[rand.IntN(len(c.machines))]
	}

	total := (len(prompt) + 3) / 4
	h := fnv.New64a()
	pos := 0
	for b := 1024; b <= total; b += 128 {
		end := min(b*4, len(prompt))
		h.Write(prompt[pos:end])
		pos = end
		k := h.Sum64()
		if exp, ok := m[k]; ok && now.Before(exp) {
			cached = b
		}
		m[k] = now.Add(c.ttl)
	}
	return cached
}

func (c *openAICache) sweep(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.machines {
		for k, exp := range m {
			if now.After(exp) {
				delete(m, k)
			}
		}
	}
	for k, ts := range c.recent {
		if len(ts) == 0 || now.Sub(ts[len(ts)-1]) > time.Minute {
			delete(c.recent, k)
		}
	}
}

// block is one rendered content block of an Anthropic request.
type block struct {
	text       string
	breakpoint bool // carries cache_control
}

// anthropicCache models Anthropic prompt caching
// (https://docs.anthropic.com/en/docs/build-with-claude/prompt-caching):
// nothing is cached unless the request marks breakpoints with cache_control;
// each breakpoint looks back up to 20 block boundaries for an entry an
// earlier request wrote; prefixes under the model's minimum (512 tokens on
// Claude Haiku 5.5) are not cached; writes bill 1.25x, reads 0.1x; a read
// refreshes the 5-minute TTL.
type anthropicCache struct {
	mu        sync.Mutex
	entries   map[uint64]time.Time
	minTokens int
	ttl       time.Duration
}

func newAnthropicCache(minTokens int, ttl time.Duration) *anthropicCache {
	return &anthropicCache{entries: map[uint64]time.Time{}, minTokens: minTokens, ttl: ttl}
}

// serve returns Anthropic's three input-token buckets for blocks.
func (c *anthropicCache) serve(blocks []block, now time.Time) (input, write, read int) {
	type end struct {
		pos  int // tokens up to the end of this block
		hash uint64
	}
	ends := make([]end, len(blocks))
	h := fnv.New64a()
	n := 0
	for i, b := range blocks {
		h.Write([]byte(b.text))
		n += len(b.text)
		ends[i] = end{(n + 3) / 4, h.Sum64()}
	}
	total := (n + 3) / 4

	c.mu.Lock()
	defer c.mu.Unlock()
	last := 0 // position of the last usable breakpoint
	var writes []uint64
	for i, b := range blocks {
		if !b.breakpoint || ends[i].pos < c.minTokens {
			continue
		}
		last = ends[i].pos
		for j := i; j >= max(0, i-20); j-- {
			if exp, ok := c.entries[ends[j].hash]; ok && now.Before(exp) {
				read = max(read, ends[j].pos)
				c.entries[ends[j].hash] = now.Add(c.ttl)
				break
			}
		}
		writes = append(writes, ends[i].hash) // readable from the next request on
	}
	for _, k := range writes {
		c.entries[k] = now.Add(c.ttl)
	}
	write = max(0, last-read)
	input = total - max(last, read)
	return input, write, read
}

func (c *anthropicCache) sweep(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, exp := range c.entries {
		if now.After(exp) {
			delete(c.entries, k)
		}
	}
}
