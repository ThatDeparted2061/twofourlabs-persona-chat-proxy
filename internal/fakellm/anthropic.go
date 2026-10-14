package fakellm

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/continuation"
	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/tok"
)

type anthropicBlock struct {
	Type         string          `json:"type"`
	Text         string          `json:"text"`
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
}

type anthropicRequest struct {
	Model    string          `json:"model"`
	System   json.RawMessage `json:"system"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	MaxTokens    int             `json:"max_tokens"`
	Stream       bool            `json:"stream"`
	CacheControl json.RawMessage `json:"cache_control"` // top-level automatic caching
}

// blocks decodes a content field: a plain string or an array of text blocks.
func blocks(raw json.RawMessage) []anthropicBlock {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []anthropicBlock{{Type: "text", Text: s}}
	}
	var bs []anthropicBlock
	json.Unmarshal(raw, &bs)
	return bs
}

func anthropicError(w http.ResponseWriter, code int, typ, msg string) {
	writeJSON(w, code, map[string]any{"type": "error", "error": map[string]any{"type": typ, "message": msg}})
}

func (s *Server) anthropic(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("x-api-key") == "" || r.Header.Get("anthropic-version") == "" {
		anthropicError(w, 401, "authentication_error", "x-api-key and anthropic-version headers are required")
		return
	}
	var req anthropicRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Messages) == 0 || req.MaxTokens <= 0 {
		anthropicError(w, 400, "invalid_request_error", "messages and max_tokens are required")
		return
	}
	s.stats.requests.Add(1)

	conv := conversation{maxTokens: req.MaxTokens}
	var bs []block
	for _, b := range blocks(req.System) {
		if conv.persona == "" {
			conv.persona = b.Text
		}
		bs = append(bs, block{"<system>" + b.Text + "\n", len(b.CacheControl) > 0})
	}
	for i, m := range req.Messages {
		for _, b := range blocks(m.Content) {
			bs = append(bs, block{"<" + m.Role + ">" + b.Text + "\n", len(b.CacheControl) > 0})
			switch {
			case m.Role == "user":
				conv.lastUser = b.Text
			case m.Role == "system" && i == len(req.Messages)-1:
				conv.partial, _ = continuation.Parse(b.Text)
			}
		}
	}
	if len(req.CacheControl) > 0 && len(bs) > 0 {
		bs[len(bs)-1].breakpoint = true
	}

	f := s.roll()
	switch f {
	case rateLimited:
		w.Header().Set("retry-after", "1")
		anthropicError(w, 429, "rate_limit_error", "Number of request tokens has exceeded your per-minute rate limit")
		return
	case serverError:
		anthropicError(w, 529, "overloaded_error", "Overloaded")
		return
	case stallFirst:
		<-r.Context().Done()
		return
	}

	input, write, read := s.an.serve(bs, s.clock.now())
	text := conv.answer()
	out := tok.Count(text)
	usage := map[string]int{"input_tokens": input, "cache_creation_input_tokens": write, "cache_read_input_tokens": read, "output_tokens": out}
	if !sleep(r, s.ttft(input+write, read)) {
		return
	}
	s.stats.promptTokens.Add(int64(input + write + read))
	s.stats.cachedTokens.Add(int64(read))
	s.stats.outputTokens.Add(int64(out))

	id := "msg_fake" + time.Now().Format("150405.000000")
	if !req.Stream {
		writeJSON(w, 200, map[string]any{
			"id": id, "type": "message", "role": "assistant", "model": req.Model,
			"content": []any{map[string]any{"type": "text", "text": text}}, "stop_reason": "end_turn", "usage": usage,
		})
		return
	}

	send := sse(w)
	start := map[string]int{"input_tokens": input, "cache_creation_input_tokens": write, "cache_read_input_tokens": read, "output_tokens": 1}
	send("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": req.Model, "content": []any{}, "stop_reason": nil, "usage": start}})
	send("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
	send("ping", map[string]any{"type": "ping"})
	ok := s.pace(r, text, f, func(p string) error {
		return send("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": p}})
	})
	if !ok {
		return
	}
	send("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	send("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]int{"output_tokens": out}})
	send("message_stop", map[string]any{"type": "message_stop"})
}
