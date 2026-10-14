// Package handlers serves the proxy's HTTP endpoints.
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/budget"
	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/history"
	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/metrics"
	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/persona"
	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/pricing"
	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/providers"
	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/ratelimit"
	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/router"
)

const maxBodyBytes = 4 << 20 // long transcripts

// Chat serves the OpenAI-compatible POST /v1/chat/completions for character
// chat: auth -> rate limit -> daily budget -> persona prefix + rolling summary
// -> provider relay -> cost accounting.
//
// Two optional headers identify the caller: X-User-ID (budgets and rate
// limits; defaults to the API key) and X-Conversation-ID (rolling summaries
// and OpenAI cache routing). A request may name a stored persona with
// "persona_id" instead of sending the character card as a system message.
type Chat struct {
	Router   *router.Router
	Limiter  *ratelimit.Limiter
	APIKeys  map[string]bool // allowed client keys; empty allows any key
	Personas *persona.Store
	Budget   *budget.Budget      // nil disables budgets
	History  *history.Summarizer // nil disables rolling summaries
	Prices   pricing.Table
	// Tuned enables prompt-cache hints, rolling summaries and resumable
	// failover. Off, the proxy streams the way LLM-Gateway did (the baseline).
	Tuned         bool
	Relay         router.RelayOptions
	SoftMaxTokens int
	SoftModel     string
}

type chatRequest struct {
	providers.ChatRequest
	PersonaID string `json:"persona_id,omitempty"`
}

func (h *Chat) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := apiKey(r)
	if len(h.APIKeys) > 0 && !h.APIKeys[key] {
		writeError(w, http.StatusUnauthorized, "invalid_api_key", "invalid or missing API key")
		return
	}
	user := cmpOr(r.Header.Get("X-User-ID"), key)
	conv := r.Header.Get("X-Conversation-ID")
	if !h.Limiter.Allow(user) {
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "too many requests for this user")
		return
	}

	var req chatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid request body: "+err.Error())
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "messages must not be empty")
		return
	}
	ctx := r.Context()

	state := budget.OK
	if h.Budget != nil {
		var err error
		if state, _, err = h.Budget.Check(ctx, user); err != nil {
			slog.Warn("budget check failed, allowing request", "err", err) // fail open: Redis down should not take chat down
		}
		metrics.BudgetCheck(string(state))
		w.Header().Set("X-Budget-State", string(state))
		switch state {
		case budget.Hard:
			writeError(w, http.StatusTooManyRequests, "budget_exceeded", "daily token budget used up; it resets at 00:00 UTC")
			return
		case budget.Soft:
			if req.MaxTokens == 0 || req.MaxTokens > h.SoftMaxTokens {
				req.MaxTokens = h.SoftMaxTokens
			}
			if h.SoftModel != "" {
				req.Model = h.SoftModel
			}
		}
	}

	t, err := h.assemble(ctx, req, conv, user)
	if err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, persona.ErrNotFound) {
			code = http.StatusNotFound
		}
		writeError(w, code, "invalid_request_error", err.Error())
		return
	}
	if req.Stream {
		h.stream(w, r, t, state)
		return
	}
	resp, err := h.Router.Chat(ctx, t.req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	cost := h.spend(resp.Provider, resp.Model, "chat", resp.Usage)
	h.finish(t, resp.Choices[0].Message.Content, resp.Usage, user)
	metrics.Turn(resp.Provider, false, 0)
	w.Header().Set("X-Provider", resp.Provider)
	w.Header().Set("X-Cost-USD", formatUSD(cost))
	writeJSON(w, http.StatusOK, resp)
}

// turn is one assembled request plus what the proxy needs after it.
type turn struct {
	req            providers.ChatRequest
	conv, user     string
	character      string
	history        []providers.Message // the client's user/assistant turns, as sent
	summaryApplied bool
}

// assemble builds the provider request: persona (stable prefix), then the
// rolling summary if one covers the start of this transcript, then the
// remaining turns. In tuned mode it marks cache breakpoints after the persona,
// after the summary and on the newest message, and routes OpenAI's cache by
// conversation.
func (h *Chat) assemble(ctx context.Context, req chatRequest, conv, user string) (*turn, error) {
	t := &turn{req: req.ChatRequest, conv: conv, user: user, character: "Assistant"}
	var prefix []providers.Message
	if req.PersonaID != "" {
		if h.Personas == nil {
			return nil, errors.New("persona_id needs the persona store (Redis)")
		}
		p, err := h.Personas.Get(ctx, req.PersonaID)
		if err != nil {
			return nil, err
		}
		prefix = append(prefix, providers.Message{Role: "system", Content: p.Prompt})
		t.character = cmpOr(p.Name, t.character)
	}
	msgs := req.Messages
	for len(msgs) > 0 && msgs[0].Role == "system" {
		prefix = append(prefix, msgs[0])
		msgs = msgs[1:]
	}
	t.history = msgs

	rest := msgs
	if h.Tuned && h.History != nil && conv != "" {
		var sum *providers.Message
		if rest, sum = h.History.Apply(ctx, conv, msgs); sum != nil {
			prefix = append(prefix, *sum)
			t.summaryApplied = true
			metrics.Summary("applied")
		}
	}
	out := make([]providers.Message, 0, len(prefix)+len(rest))
	out = append(out, prefix...)
	if h.Tuned && len(prefix) > 0 {
		out[len(out)-1].Cache = true // persona (+ summary): identical across turns
		if t.summaryApplied && len(prefix) > 1 {
			out[len(out)-2].Cache = true
		}
	}
	out = append(out, rest...)
	if h.Tuned {
		out[len(out)-1].Cache = true // grows the cached prefix turn by turn
		t.req.PromptCacheKey = cmpOr(conv, user)
	}
	t.req.Messages = out
	return t, nil
}

func (h *Chat) stream(w http.ResponseWriter, r *http.Request, t *turn, state budget.State) {
	ctx := r.Context()
	var s *sse
	emit := func(content string) error {
		if s == nil { // headers wait for the first token, so a turn that never starts can still get a clean HTTP error
			s = startSSE(w, cmpOr(t.req.Model, "auto")) // the provider's model name is known once the turn ends
		}
		return s.send(content, "")
	}
	var (
		res *router.RelayResult
		err error
	)
	if h.Tuned {
		res, err = h.Router.Relay(ctx, t.req, h.Relay, emit)
	} else {
		res, err = h.Router.Passthrough(ctx, t.req, emit)
	}

	var total providers.Usage
	var cost float64
	if res != nil {
		for _, a := range res.Attempts {
			cost += h.spend(a.Provider, a.Model, "chat", a.Usage)
		}
		total = res.Usage()
		defer func() { h.budgetSpend(t.user, total) }()
	}
	if errors.Is(err, router.ErrClientGone) {
		return
	}
	provider := ""
	if res != nil {
		provider = res.Provider
	}
	if err != nil {
		slog.Warn("turn failed", "conversation", t.conv, "err", err)
		metrics.Turn(provider, true, 0)
		if s == nil {
			writeError(w, http.StatusBadGateway, "upstream_error", err.Error())
			return
		}
		s.fail(err) // the 200 is already sent, so the error goes in-band, as OpenAI does it
		return
	}
	if s == nil {
		s = startSSE(w, t.req.Model) // a reply with no text at all
	}
	if res.Model != "" {
		s.model = res.Model
	}
	metrics.Turn(provider, false, res.TTFT)
	if s.send("", cmpOr(res.FinishReason, "stop")) != nil {
		return
	}
	s.event(map[string]any{
		"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.model, "choices": []any{},
		"usage": total,
		"x_proxy": map[string]any{
			"provider": provider, "cost_usd": cost, "failovers": res.Failovers,
			"summary_applied": t.summaryApplied, "budget_state": state, "ttft_ms": res.TTFT.Milliseconds(),
		},
	})
	s.done()
	h.finish(t, res.Text, total, "")
}

// finish runs after a successful turn: rolling-summary upkeep, and the budget
// for non-streamed turns (streamed ones are charged in stream, failed attempts included).
func (h *Chat) finish(t *turn, reply string, u providers.Usage, chargeUser string) {
	if chargeUser != "" {
		h.budgetSpend(chargeUser, u)
	}
	if h.Tuned && h.History != nil && t.conv != "" {
		h.History.After(t.conv, t.character, append(t.history[:len(t.history):len(t.history)], providers.Message{Role: "assistant", Content: reply}))
	}
}

func (h *Chat) budgetSpend(user string, u providers.Usage) {
	if h.Budget == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := h.Budget.Spend(ctx, user, int64(u.PromptTokens+u.CompletionTokens)); err != nil {
		slog.Warn("budget spend failed", "err", err)
	}
}

// spend prices one provider call and records it.
func (h *Chat) spend(provider, model, purpose string, u providers.Usage) float64 {
	usd := h.Prices.Cost(model, u)
	if _, ok := h.Prices.Lookup(model); !ok && model != "" {
		slog.Debug("no price for model; counted as free", "model", model)
	}
	metrics.Spend(provider, purpose, usd, max(0, u.PromptTokens-u.Cached()-u.CacheWriteTokens), u.Cached(), u.CacheWriteTokens, u.CompletionTokens)
	return usd
}

// SummarySpent records what a rolling-summary call cost.
func (h *Chat) SummarySpent(resp *providers.ChatResponse) {
	h.spend(resp.Provider, resp.Model, "summary", resp.Usage)
	metrics.Summary("written")
}

// apiKey is the client's bearer token.
func apiKey(r *http.Request) string {
	if k, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && k != "" {
		return k
	}
	return "anonymous"
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// writeError writes an OpenAI-style error body.
func writeError(w http.ResponseWriter, code int, errType, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]any{"message": msg, "type": errType, "code": code}})
}
