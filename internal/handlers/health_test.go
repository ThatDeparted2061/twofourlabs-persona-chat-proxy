package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/fakellm"
	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/providers"
)

type down struct{ providers.Provider }

func (down) Name() string               { return "down" }
func (down) Ping(context.Context) error { return errors.New("connection refused") }

func TestHealthDegradedWhenSomeProvidersDown(t *testing.T) {
	up := providers.NewOpenAI(upstream(t, fakellm.Faults{}), "k", "m")
	w := httptest.NewRecorder()
	Health([]providers.Provider{up, down{up}})(w, httptest.NewRequest(http.MethodGet, "/health", nil))
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, `"status":"degraded"`) || !strings.Contains(body, `"error":"connection refused"`) {
		t.Fatalf("%d %s", w.Code, body)
	}

	w = httptest.NewRecorder()
	Health([]providers.Provider{down{up}})(w, httptest.NewRequest(http.MethodGet, "/health", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("all down: want 503, got %d", w.Code)
	}
}
