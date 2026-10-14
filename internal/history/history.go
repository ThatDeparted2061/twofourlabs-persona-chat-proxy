// Package history keeps long conversations cheap with a rolling summary.
//
// Clients send the full transcript every turn (the OpenAI convention). Once
// the part of it that has not been summarized grows past Trigger tokens, the
// proxy summarizes everything except the last ~Keep tokens in the background
// and stores the summary in Redis with a hash of the messages it covers. On
// later turns, if the client's transcript still starts with exactly those
// messages, they are replaced by the summary. The summary sits right after the
// persona and changes only when it is rewritten, so between rewrites the whole
// prefix stays cacheable.
package history

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/providers"
	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/tok"
)

// Summarizer applies and maintains rolling summaries.
type Summarizer struct {
	rdb           *redis.Client
	prefix        string
	Trigger, Keep int // tokens
	// Complete runs the summarization call (the router's non-streaming Chat).
	Complete func(ctx context.Context, req providers.ChatRequest) (*providers.ChatResponse, error)
	// Spent is told what each summary cost, for metrics and budgets.
	Spent func(resp *providers.ChatResponse)
}

func New(rdb *redis.Client, prefix string, trigger, keep int) *Summarizer {
	return &Summarizer{rdb: rdb, prefix: prefix + "summary:", Trigger: trigger, Keep: keep}
}

type stored struct {
	Covers int    `json:"covers"` // messages the summary replaces
	Hash   string `json:"hash"`   // hash of those messages
	Text   string `json:"text"`
}

// Prefix is how the summary is presented to the model.
const Prefix = "Summary of the earlier conversation (older messages were condensed):\n"

func hash(msgs []providers.Message) string {
	h := sha256.New()
	for _, m := range msgs {
		h.Write([]byte(m.Role))
		h.Write([]byte{0})
		h.Write([]byte(m.Content))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:12])
}

func (s *Summarizer) load(ctx context.Context, conv string) (stored, bool) {
	var st stored
	b, err := s.rdb.Get(ctx, s.prefix+conv).Bytes()
	if err != nil || json.Unmarshal(b, &st) != nil {
		return stored{}, false
	}
	return st, true
}

// Apply replaces the summarized head of history (user/assistant turns only)
// with a system message holding the summary. It returns history unchanged
// if there is no summary or the transcript no longer matches it.
func (s *Summarizer) Apply(ctx context.Context, conv string, history []providers.Message) ([]providers.Message, *providers.Message) {
	st, ok := s.load(ctx, conv)
	if !ok || st.Covers > len(history) || hash(history[:st.Covers]) != st.Hash {
		return history, nil
	}
	return history[st.Covers:], &providers.Message{Role: "system", Content: Prefix + st.Text}
}

// After looks at the transcript once a turn has finished (history ends with
// the new assistant reply) and, if the unsummarized part is past Trigger,
// starts a background summarization.
func (s *Summarizer) After(conv, character string, history []providers.Message) {
	ctx := context.Background()
	st, _ := s.load(ctx, conv)
	if st.Covers > len(history) || (st.Covers > 0 && hash(history[:st.Covers]) != st.Hash) {
		st = stored{} // the client rewrote history: start over
	}
	tail := 0
	for _, m := range history[st.Covers:] {
		tail += tok.Count(m.Content)
	}
	if tail <= s.Trigger {
		return
	}
	// Keep the newest ~Keep tokens verbatim, starting on a user message.
	cut, kept := len(history), 0
	for cut > st.Covers && kept+tok.Count(history[cut-1].Content) <= s.Keep {
		cut--
		kept += tok.Count(history[cut].Content)
	}
	for cut < len(history) && history[cut].Role != "user" {
		cut++
	}
	if cut <= st.Covers {
		return
	}
	lock := s.prefix + conv + ":lock"
	if ok, err := s.rdb.SetNX(ctx, lock, 1, time.Minute).Result(); err != nil || !ok {
		return // a summary for this conversation is already being written
	}
	go func() {
		defer s.rdb.Del(context.Background(), lock)
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if err := s.summarize(ctx, conv, character, st, history[:cut]); err != nil {
			slog.Warn("summary failed", "conversation", conv, "err", err)
		}
	}()
}

func (s *Summarizer) summarize(ctx context.Context, conv, character string, prev stored, head []providers.Message) error {
	var b strings.Builder
	if prev.Text != "" {
		fmt.Fprintf(&b, "Summary so far:\n%s\n\nNew messages:\n", prev.Text)
	}
	for _, m := range head[prev.Covers:] {
		who := "User"
		if m.Role == "assistant" {
			who = character
		}
		fmt.Fprintf(&b, "%s: %s\n", who, m.Content)
	}
	resp, err := s.Complete(ctx, providers.ChatRequest{MaxTokens: 300, Messages: []providers.Message{
		{Role: "system", Content: "You maintain the long-term memory of a character chat. Merge the summary so far with the new messages into one summary of at most 180 words, written in the third person. Keep names, facts the user shared about their life, promises, running jokes and the emotional tone. Drop small talk."},
		{Role: "user", Content: b.String()},
	}})
	if err != nil {
		return err
	}
	if s.Spent != nil {
		s.Spent(resp)
	}
	if len(resp.Choices) == 0 || resp.Choices[0].Message.Content == "" {
		return fmt.Errorf("empty summary")
	}
	st := stored{Covers: len(head), Hash: hash(head), Text: resp.Choices[0].Message.Content}
	v, _ := json.Marshal(st)
	return s.rdb.Set(ctx, s.prefix+conv, v, 24*time.Hour).Err()
}
