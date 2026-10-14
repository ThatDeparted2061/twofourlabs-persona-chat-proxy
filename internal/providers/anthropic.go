package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// anthropic adapts the Anthropic Messages API (https://api.anthropic.com/v1)
// to the OpenAI shape. Messages marked Cache become cache_control breakpoints:
// Anthropic caches nothing unless asked, then bills reads at 0.1x and 5-minute
// writes at 1.25x of the input price.
type anthropic struct {
	baseURL, apiKey, model string
	disableThinking        bool
}

// NewAnthropic returns the Anthropic adapter. With disableThinking the request
// turns extended thinking off, which keeps time to first token low for chat
// (Claude Haiku 5.5 accepts this at its default effort).
func NewAnthropic(baseURL, apiKey, model string, disableThinking bool) Provider {
	return &anthropic{baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, model: model, disableThinking: disableThinking}
}

type anthropicText struct {
	Type         string        `json:"type"`
	Text         string        `json:"text"`
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

type cacheControl struct {
	Type string `json:"type"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content []anthropicText `json:"content"`
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    []anthropicText    `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
	Stream    bool               `json:"stream,omitempty"`
	Thinking  *struct {
		Type string `json:"type"`
	} `json:"thinking,omitempty"`
}

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	OutputTokens             int `json:"output_tokens"`
}

func (u anthropicUsage) usage() Usage {
	prompt := u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
	return Usage{
		PromptTokens:        prompt,
		CompletionTokens:    u.OutputTokens,
		TotalTokens:         prompt + u.OutputTokens,
		PromptTokensDetails: &PromptTokensDetails{CachedTokens: u.CacheReadInputTokens},
		CacheWriteTokens:    u.CacheCreationInputTokens,
	}
}

func (p *anthropic) Name() string { return "anthropic" }

func (p *anthropic) headers() map[string]string {
	return map[string]string{"x-api-key": p.apiKey, "anthropic-version": "2023-06-01"}
}

// toAnthropic maps OpenAI messages: leading system messages become the system
// prompt, later ones stay in messages as mid-conversation system messages
// (used for the continuation instruction). Sampling parameters are dropped:
// current Claude models reject non-default values.
func (p *anthropic) toAnthropic(req ChatRequest, stream bool) anthropicRequest {
	out := anthropicRequest{Model: req.Model, MaxTokens: req.MaxTokens, Stream: stream}
	if out.Model == "" {
		out.Model = p.model
	}
	if out.MaxTokens == 0 {
		out.MaxTokens = 1024
	}
	if p.disableThinking {
		out.Thinking = &struct {
			Type string `json:"type"`
		}{"disabled"}
	}
	leading := true
	for _, m := range req.Messages {
		t := anthropicText{Type: "text", Text: m.Content}
		if m.Cache {
			t.CacheControl = &cacheControl{Type: "ephemeral"}
		}
		if m.Role == "system" && leading {
			out.System = append(out.System, t)
			continue
		}
		leading = false
		out.Messages = append(out.Messages, anthropicMessage{Role: m.Role, Content: []anthropicText{t}})
	}
	return out
}

func finishReason(stop string) string {
	switch stop {
	case "max_tokens":
		return "length"
	case "refusal":
		return "content_filter"
	case "":
		return ""
	}
	return "stop" // end_turn, stop_sequence
}

func (p *anthropic) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	resp, err := do(ctx, "anthropic", http.MethodPost, p.baseURL+"/messages", p.headers(), p.toAnthropic(req, false))
	if err != nil {
		return nil, err
	}
	var r struct {
		ID         string          `json:"id"`
		Model      string          `json:"model"`
		Content    []anthropicText `json:"content"`
		StopReason string          `json:"stop_reason"`
		Usage      anthropicUsage  `json:"usage"`
	}
	if err := decode(resp, &r); err != nil {
		return nil, fmt.Errorf("anthropic: decode response: %w", err)
	}
	var text strings.Builder
	for _, c := range r.Content {
		if c.Type == "text" {
			text.WriteString(c.Text)
		}
	}
	return &ChatResponse{
		ID: r.ID, Object: "chat.completion", Created: time.Now().Unix(), Model: r.Model,
		Choices: []Choice{{Message: Message{Role: "assistant", Content: text.String()}, FinishReason: finishReason(r.StopReason)}},
		Usage:   r.Usage.usage(),
	}, nil
}

// ChatStream maps Anthropic's typed SSE events (message_start, content_block_delta,
// message_delta, error) to StreamChunks. Prompt usage arrives on message_start,
// output usage on message_delta; the final chunk carries both.
func (p *anthropic) ChatStream(ctx context.Context, req ChatRequest) (<-chan StreamChunk, error) {
	resp, err := do(ctx, "anthropic", http.MethodPost, p.baseURL+"/messages", p.headers(), p.toAnthropic(req, true))
	if err != nil {
		return nil, err
	}
	var usage anthropicUsage
	return streamSSE(ctx, resp.Body, func(data []byte) (StreamChunk, error) {
		var ev struct {
			Type    string `json:"type"`
			Message struct {
				Model string         `json:"model"`
				Usage anthropicUsage `json:"usage"`
			} `json:"message"`
			Delta struct {
				Type       string `json:"type"`
				Text       string `json:"text"`
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
			Usage *anthropicUsage `json:"usage"`
			Error *struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			return StreamChunk{}, fmt.Errorf("anthropic: bad stream event: %w", err)
		}
		switch ev.Type {
		case "message_start":
			usage = ev.Message.Usage
			return StreamChunk{Model: ev.Message.Model}, nil
		case "content_block_delta":
			if ev.Delta.Type == "text_delta" {
				return StreamChunk{Content: ev.Delta.Text}, nil
			}
		case "message_delta":
			if ev.Usage != nil {
				usage.OutputTokens = ev.Usage.OutputTokens
			}
			u := usage.usage()
			return StreamChunk{FinishReason: finishReason(ev.Delta.StopReason), Usage: &u}, nil
		case "error":
			msg := "stream error"
			if ev.Error != nil {
				msg = ev.Error.Type + ": " + ev.Error.Message
			}
			return StreamChunk{}, fmt.Errorf("anthropic: %s", msg)
		}
		return StreamChunk{}, nil // ping, content_block_start/stop, message_stop
	}), nil
}

func (p *anthropic) Ping(ctx context.Context) error {
	resp, err := do(ctx, "anthropic", http.MethodGet, p.baseURL+"/models", p.headers(), nil)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}
