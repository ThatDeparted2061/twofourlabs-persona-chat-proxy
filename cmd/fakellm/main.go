// Command fakellm runs one simulated LLM provider (OpenAI and Anthropic wire
// formats) for load tests. Run one process per provider so one can be killed
// while the other keeps serving.
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/fakellm"
)

func main() {
	var cfg fakellm.Config
	addr := flag.String("addr", ":9001", "listen address")
	flag.StringVar(&cfg.Name, "name", "openai", "provider name, for logs")
	flag.Float64Var(&cfg.TimeScale, "time-scale", 20, "simulated seconds per wall-clock second (cache TTLs and rate windows)")
	flag.DurationVar(&cfg.Latency.Base, "ttft-base", 180*time.Millisecond, "fixed part of time to first token")
	flag.DurationVar(&cfg.Latency.PerToken, "prefill-per-token", 80*time.Microsecond, "TTFT cost per uncached prompt token")
	flag.DurationVar(&cfg.Latency.PerCachedToken, "prefill-per-cached-token", 8*time.Microsecond, "TTFT cost per cached prompt token")
	flag.Float64Var(&cfg.Latency.Jitter, "jitter", 0.25, "lognormal sigma applied to TTFT")
	flag.Float64Var(&cfg.Latency.TokensPerSec, "tps", 120, "decode speed, tokens/second")
	flag.Float64Var(&cfg.Faults.RateLimited, "p429", 0, "probability of a 429")
	flag.Float64Var(&cfg.Faults.ServerError, "p5xx", 0, "probability of a 503/529")
	flag.Float64Var(&cfg.Faults.StallFirst, "pstall-first", 0, "probability a request never sends a byte")
	flag.Float64Var(&cfg.Faults.StallMid, "pstall-mid", 0, "probability a stream stalls halfway")
	flag.IntVar(&cfg.OpenAIMachines, "machines", 16, "OpenAI cache: machines behind the router")
	flag.IntVar(&cfg.OpenAIOverflow, "overflow-rpm", 15, "OpenAI cache: requests/min per prefix+key before overflow")
	flag.IntVar(&cfg.AnthropicMinimum, "anthropic-min-tokens", 512, "Anthropic cache: minimum cacheable prefix")
	flag.DurationVar(&cfg.CacheTTL, "cache-ttl", 5*time.Minute, "cache entry lifetime in simulated time")
	flag.Parse()

	slog.Info("fakellm listening", "addr", *addr, "name", cfg.Name, "faults", cfg.Faults)
	if err := http.ListenAndServe(*addr, fakellm.New(cfg).Handler()); err != nil {
		slog.Error("listen", "err", err)
		os.Exit(1)
	}
}
