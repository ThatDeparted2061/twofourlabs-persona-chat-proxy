package fakellm

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/continuation"
	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/tok"
)

type openAIRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Stream              bool   `json:"stream"`
	MaxTokens           int    `json:"max_tokens"`
	MaxCompletionTokens int    `json:"max_completion_tokens"`
	PromptCacheKey      string `json:"prompt_cache_key"`
	StreamOptions       *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

type openAIUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func openAIError(w http.ResponseWriter, code int, typ, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]any{"message": msg, "type": typ, "code": nil}})
}

func (s *Server) openAI(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		openAIError(w, 401, "invalid_request_error", "missing API key")
		return
	}
	var req openAIRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Messages) == 0 {
		openAIError(w, 400, "invalid_request_error", "bad request body")
		return
	}
	s.stats.requests.Add(1)

	var prompt strings.Builder
	conv := conversation{maxTokens: max(req.MaxTokens, req.MaxCompletionTokens)}
	for i, m := range req.Messages {
		prompt.WriteString("<|" + m.Role + "|>" + m.Content + "\n")
		switch {
		case m.Role == "system" && conv.persona == "":
			conv.persona = m.Content
		case m.Role == "user":
			conv.lastUser = m.Content
		case m.Role == "system" && i == len(req.Messages)-1:
			conv.partial, _ = continuation.Parse(m.Content)
		}
	}

	f := s.roll()
	switch f {
	case rateLimited:
		w.Header().Set("Retry-After", "1")
		openAIError(w, 429, "rate_limit_exceeded", "Rate limit reached for requests")
		return
	case serverError:
		openAIError(w, 503, "server_error", "The server is overloaded. Please try again later.")
		return
	case stallFirst:
		<-r.Context().Done()
		return
	}

	cached := s.oa.serve([]byte(prompt.String()), req.PromptCacheKey, s.clock.now())
	total := tok.Count(prompt.String())
	text := conv.answer()
	usage := openAIUsage{PromptTokens: total, CompletionTokens: tok.Count(text), TotalTokens: total + tok.Count(text)}
	usage.PromptTokensDetails.CachedTokens = cached
	if !sleep(r, s.ttft(total-cached, cached)) {
		return
	}
	s.stats.promptTokens.Add(int64(total))
	s.stats.cachedTokens.Add(int64(cached))
	s.stats.outputTokens.Add(int64(usage.CompletionTokens))

	model := req.Model
	id := "chatcmpl-fake" + time.Now().Format("150405.000000")
	if !req.Stream {
		writeJSON(w, 200, map[string]any{
			"id": id, "object": "chat.completion", "created": time.Now().Unix(), "model": model,
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": text}, "finish_reason": "stop"}},
			"usage":   usage,
		})
		return
	}

	send := sse(w)
	chunk := func(delta map[string]any, finish any) map[string]any {
		return map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
	}
	first := true
	ok := s.pace(r, text, f, func(p string) error {
		d := map[string]any{"content": p}
		if first {
			d["role"], first = "assistant", false
		}
		return send("", chunk(d, nil))
	})
	if !ok {
		return
	}
	send("", chunk(map[string]any{}, "stop"))
	if req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
		send("", map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{}, "usage": usage})
	}
	w.Write([]byte("data: [DONE]\n\n"))
}
