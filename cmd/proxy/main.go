// Command proxy runs persona-chat-proxy, an OpenAI-compatible streaming proxy
// tuned for character chat.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/budget"
	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/config"
	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/handlers"
	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/history"
	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/metrics"
	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/persona"
	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/providers"
	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/ratelimit"
	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/router"
	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/server"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "YAML config file (optional; env vars override it)")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		slog.Error("invalid config", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var ps []providers.Provider
	for _, name := range cfg.ProviderOrder() {
		switch name {
		case "openai":
			ps = append(ps, providers.NewOpenAI(cfg.OpenAI.BaseURL, cfg.OpenAI.APIKey, cfg.OpenAI.Model))
		case "anthropic":
			ps = append(ps, providers.NewAnthropic(cfg.Anthropic.BaseURL, cfg.Anthropic.APIKey, cfg.Anthropic.Model, cfg.Anthropic.DisableThinking))
		case "groq":
			ps = append(ps, providers.NewGroq(cfg.Groq.BaseURL, cfg.Groq.APIKey, cfg.Groq.Model))
		case "gemini":
			ps = append(ps, providers.NewGemini(cfg.Gemini.BaseURL, cfg.Gemini.APIKey, cfg.Gemini.Model))
		case "ollama":
			ps = append(ps, providers.NewOllama(cfg.Ollama.BaseURL, cfg.Ollama.Model))
		}
	}

	metrics.Init(cfg.ProviderOrder())

	opt, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		slog.Error("invalid redis_url", "err", err)
		os.Exit(1)
	}
	opt.PoolSize = 256
	rdb := redis.NewClient(opt)
	if err := rdb.Ping(ctx).Err(); err != nil {
		slog.Error("redis unreachable (personas, budgets and summaries need it)", "url", cfg.RedisURL, "err", err)
		os.Exit(1)
	}

	rt := router.New(ps...)
	chat := &handlers.Chat{
		Router:   rt,
		Limiter:  ratelimit.New(ctx, cfg.RateLimit.TokensPerSec, cfg.RateLimit.Burst),
		APIKeys:  map[string]bool{},
		Personas: persona.New(rdb, cfg.RedisPrefix),
		Prices:   cfg.Prices(),
		Tuned:    cfg.Mode == "tuned",
		Relay: router.RelayOptions{
			FirstTokenTimeout: cfg.Relay.FirstTokenTimeout,
			StallTimeout:      cfg.Relay.StallTimeout,
			MaxAttempts:       cfg.Relay.MaxAttempts,
			Resume:            true,
		},
		SoftMaxTokens: cfg.Budget.SoftMaxTokens,
		SoftModel:     cfg.Budget.SoftModel,
	}
	for _, k := range cfg.APIKeys {
		if k = strings.TrimSpace(k); k != "" {
			chat.APIKeys[k] = true
		}
	}
	if cfg.Budget.SoftTokens > 0 || cfg.Budget.HardTokens > 0 {
		chat.Budget = budget.New(rdb, cfg.RedisPrefix, cfg.Budget.SoftTokens, cfg.Budget.HardTokens)
	}
	if cfg.History.TriggerTokens > 0 {
		chat.History = history.New(rdb, cfg.RedisPrefix, cfg.History.TriggerTokens, cfg.History.KeepTokens)
		chat.History.Complete = rt.Chat
		chat.History.Spent = chat.SummarySpent
	}

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           server.New(chat, &handlers.Personas{Store: chat.Personas, APIKeys: chat.APIKeys}, handlers.Health(ps)),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No WriteTimeout: it would cut off long SSE streams.
	}
	go func() {
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server failed", "err", err)
			os.Exit(1)
		}
	}()
	slog.Info("persona-chat-proxy listening", "port", cfg.Port, "mode", cfg.Mode, "providers", cfg.ProviderOrder(),
		"first_token_timeout", cfg.Relay.FirstTokenTimeout, "stall_timeout", cfg.Relay.StallTimeout, "auth", len(chat.APIKeys) > 0)

	<-ctx.Done()
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown", "err", err)
	}
}
