package fakellm

import (
	"strings"
	"testing"
	"time"

	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/continuation"
)

func TestReplyVerifies(t *testing.T) {
	r := Reply("persona", "hello", 0)
	if !Verify(r) {
		t.Fatalf("reply does not verify: %q", r)
	}
	if Reply("persona", "hello", 0) != r {
		t.Fatal("reply is not deterministic")
	}
	if Verify(r[:len(r)/2]+r[len(r)/2+3:]) || Verify(r[:len(r)/2]+r[len(r)/3:]) {
		t.Fatal("verify accepted a damaged reply")
	}
}

func TestContinuationResumes(t *testing.T) {
	full := Reply("p", "u", 0)
	partial := full[:len(full)/3]
	c := conversation{persona: "p", lastUser: "u"}
	c.partial, _ = continuation.Parse(continuation.Instruction(partial))
	if got := partial + c.answer(); got != full {
		t.Fatalf("resumed reply differs:\n got %q\nwant %q", got, full)
	}
}

func TestOpenAICacheKeyAvoidsOverflow(t *testing.T) {
	persona := strings.Repeat("persona ", 600) // ~1200 tokens, shared by every session
	now := time.Now()
	hits := func(withKey bool) int {
		c := newOpenAICache(16, 15, 5*time.Minute)
		total := 0
		for turn := range 3 {
			for sess := range 100 { // 100 sessions, same persona, same minute
				history := strings.Repeat("s"+string(rune('a'+sess%26))+" turn ", 120*(turn+1))
				key := ""
				if withKey {
					key = "conv-" + string(rune(sess))
				}
				total += c.serve([]byte(persona+history), key, now.Add(time.Duration(turn)*time.Second))
			}
		}
		return total
	}
	without, with := hits(false), hits(true)
	if with <= without {
		t.Fatalf("prompt_cache_key should raise cached tokens: with=%d without=%d", with, without)
	}
}

func TestAnthropicCacheNeedsBreakpointsAndLooksBack(t *testing.T) {
	c := newAnthropicCache(512, 5*time.Minute)
	now := time.Now()
	sys := block{strings.Repeat("x", 4000), true} // 1000 tokens
	turn := func(n int) []block {
		bs := []block{sys}
		for i := range n {
			bs = append(bs, block{strings.Repeat("m", 400+i), false})
		}
		bs[len(bs)-1].breakpoint = true
		return bs
	}
	if in, w, r := c.serve([]block{{sys.text, false}, {"hi", false}}, now); w != 0 || r != 0 || in == 0 {
		t.Fatalf("no breakpoints must mean no caching, got in=%d write=%d read=%d", in, w, r)
	}
	_, w1, r1 := c.serve(turn(1), now)
	_, w2, r2 := c.serve(turn(3), now)
	if r1 != 0 || w1 == 0 {
		t.Fatalf("first turn should only write: write=%d read=%d", w1, r1)
	}
	if r2 < w1 || w2 >= w1 {
		t.Fatalf("second turn should read the first turn's prefix: read=%d write=%d (first wrote %d)", r2, w2, w1)
	}
	if _, _, r := c.serve(turn(3), now.Add(6*time.Minute)); r != 0 {
		t.Fatalf("entries must expire after the TTL, read=%d", r)
	}
}
