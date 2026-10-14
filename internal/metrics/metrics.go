// Package metrics defines the proxy's Prometheus metrics. They live on the
// default registry and are served at /metrics.
package metrics

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/providers"
)

var (
	requests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "llm_requests_total",
		Help: "Upstream provider calls by provider and outcome: ok, the upstream HTTP status, canceled (client went away), or error.",
	}, []string{"provider", "status"})

	// p50/p99: histogram_quantile(0.99, sum by (le, provider) (rate(llm_request_duration_seconds_bucket[5m])))
	duration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "llm_request_duration_seconds",
		Help:    "Upstream call latency by provider (time to first byte for streams).",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 4, 8, 16, 32, 60, 120}, // local models under load can take minutes
	}, []string{"provider"})
)

// ObserveCall records one upstream attempt.
func ObserveCall(provider string, err error, d time.Duration) {
	requests.WithLabelValues(provider, status(err)).Inc()
	duration.WithLabelValues(provider).Observe(d.Seconds())
}

func status(err error) string {
	var se *providers.StatusError
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.As(err, &se):
		return strconv.Itoa(se.Code)
	default:
		return "error"
	}
}

// Persona-chat metrics. The dashboard derives cost per 1,000 turns as
// 1000 * rate(persona_cost_usd_total) / rate(persona_turns_total).
var (
	turns = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "persona_turns_total",
		Help: "Chat turns served, by the provider that finished the reply and outcome (ok, dropped).",
	}, []string{"provider", "outcome"})

	cost = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "persona_cost_usd_total",
		Help: "Provider spend in USD, by provider and purpose (chat, summary). Includes failed attempts.",
	}, []string{"provider", "purpose"})

	ttft = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "persona_ttft_seconds",
		Help:    "Time from request to first streamed token, as the client sees it (failovers included).",
		Buckets: []float64{0.05, 0.1, 0.15, 0.2, 0.25, 0.3, 0.4, 0.5, 0.75, 1, 1.5, 2, 3, 5, 10},
	})

	failovers = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "persona_failovers_total",
		Help: "Provider switches within a turn, by reason (429, 5xx, connect_error, first_token_timeout, stall, stream_error, eof).",
	}, []string{"from", "to", "reason"})

	promptTokens = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "persona_prompt_tokens_total",
		Help: "Prompt tokens by provider and kind: uncached, cached (read), cache_write.",
	}, []string{"provider", "kind"})

	completionTokens = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "persona_completion_tokens_total",
		Help: "Completion tokens by provider.",
	}, []string{"provider"})

	budgetChecks = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "persona_budget_checks_total",
		Help: "Daily token budget checks by result: ok, soft (degraded), hard (refused).",
	}, []string{"state"})

	summaries = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "persona_summaries_total",
		Help: "Rolling history summaries: written, applied (used in a prompt), failed.",
	}, []string{"result"})

	breakerOpen = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "persona_breaker_open",
		Help: "1 while a provider's circuit breaker is open.",
	}, []string{"provider"})
)

// Turn records one finished chat turn.
func Turn(provider string, dropped bool, firstToken time.Duration) {
	outcome := "ok"
	if dropped {
		outcome = "dropped"
	}
	turns.WithLabelValues(provider, outcome).Inc()
	if firstToken > 0 {
		ttft.Observe(firstToken.Seconds())
	}
}

// Spend records one provider call's usage and cost.
func Spend(provider, purpose string, usd float64, uncached, cached, cacheWrite, completion int) {
	cost.WithLabelValues(provider, purpose).Add(usd)
	promptTokens.WithLabelValues(provider, "uncached").Add(float64(uncached))
	promptTokens.WithLabelValues(provider, "cached").Add(float64(cached))
	promptTokens.WithLabelValues(provider, "cache_write").Add(float64(cacheWrite))
	completionTokens.WithLabelValues(provider).Add(float64(completion))
}

func Failover(from, to, reason string) { failovers.WithLabelValues(from, to, reason).Inc() }
func BudgetCheck(state string)         { budgetChecks.WithLabelValues(state).Inc() }
func Summary(result string)            { summaries.WithLabelValues(result).Inc() }

func BreakerOpen(provider string, open bool) {
	v := 0.0
	if open {
		v = 1
	}
	breakerOpen.WithLabelValues(provider).Set(v)
}

// Init creates the failover and turn series at zero, so the first failure of
// each kind shows up in rate(): Prometheus ignores a counter's initial jump
// when the series did not exist before.
func Init(providers []string) {
	for _, from := range providers {
		for _, to := range providers {
			if from == to {
				continue
			}
			for _, r := range []string{"429", "5xx", "4xx", "connect_error", "first_token_timeout", "stall", "stream_error", "eof"} {
				failovers.WithLabelValues(from, to, r)
			}
		}
		turns.WithLabelValues(from, "ok")
		turns.WithLabelValues(from, "dropped")
		breakerOpen.WithLabelValues(from).Set(0)
	}
	for _, s := range []string{"ok", "soft", "hard"} {
		budgetChecks.WithLabelValues(s)
	}
	for _, r := range []string{"written", "applied"} {
		summaries.WithLabelValues(r)
	}
}
