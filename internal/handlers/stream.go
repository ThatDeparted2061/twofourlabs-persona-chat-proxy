package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/providers"
)

// sse writes OpenAI chat.completion.chunk events.
type sse struct {
	w        http.ResponseWriter
	rc       *http.ResponseController
	id       string
	model    string
	created  int64
	sentRole bool
}

type chunkDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

type chunkChoice struct {
	Index        int        `json:"index"`
	Delta        chunkDelta `json:"delta"`
	FinishReason *string    `json:"finish_reason"`
}

type chunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
}

func startSSE(w http.ResponseWriter, model string) *sse {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // stop reverse proxies from buffering the stream
	w.WriteHeader(http.StatusOK)
	return &sse{w: w, rc: http.NewResponseController(w), id: providers.NewID(), model: model, created: time.Now().Unix()}
}

func (s *sse) send(content, finishReason string) error {
	c := chunkChoice{Delta: chunkDelta{Content: content}}
	if !s.sentRole {
		c.Delta.Role, s.sentRole = "assistant", true
	}
	if finishReason != "" {
		c.FinishReason = &finishReason
	}
	return s.event(chunk{ID: s.id, Object: "chat.completion.chunk", Created: s.created, Model: s.model, Choices: []chunkChoice{c}})
}

// fail reports a mid-stream upstream error. The 200 status is already sent, so
// the error goes in-band, the way OpenAI does it.
func (s *sse) fail(err error) {
	s.event(map[string]any{"error": map[string]any{"message": err.Error(), "type": "upstream_error"}})
}

func (s *sse) done() {
	fmt.Fprint(s.w, "data: [DONE]\n\n")
	s.rc.Flush()
}

func (s *sse) event(v any) error {
	b, _ := json.Marshal(v)
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", b); err != nil {
		return err
	}
	return s.rc.Flush()
}

func formatUSD(v float64) string { return strconv.FormatFloat(v, 'f', 8, 64) }
