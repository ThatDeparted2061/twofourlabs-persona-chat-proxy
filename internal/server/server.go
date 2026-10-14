// Package server builds the proxy's HTTP routes.
package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/handlers"
)

// New returns the chi router serving the proxy API. personas may be nil.
func New(chat http.Handler, personas *handlers.Personas, health http.HandlerFunc) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Recoverer)
	r.Method(http.MethodPost, "/v1/chat/completions", chat)
	if personas != nil {
		r.Put("/v1/personas/{id}", personas.Put)
		r.Get("/v1/personas/{id}", personas.Get)
	}
	r.Method(http.MethodGet, "/metrics", promhttp.Handler())
	r.Get("/health", health)
	return r
}
