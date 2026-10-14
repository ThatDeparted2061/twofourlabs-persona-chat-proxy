// Package fakellm is a simulated LLM provider for load tests. It speaks the
// OpenAI chat-completions API and the Anthropic Messages API (streaming and
// not), models each provider's documented prompt-caching behaviour, charges
// time to first token for uncached prompt tokens, and injects faults: 429s,
// 5xx, streams that never start, streams that stall halfway, and a kill switch.
//
// Replies are deterministic (see Reply), which is what lets a second provider
// resume a reply the first one dropped, and lets the load generator check
// that every reply arrived whole.
package fakellm

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/tok"
)

// Latency is the performance model.
type Latency struct {
	Base           time.Duration // fixed time to first token
	PerToken       time.Duration // prefill cost per uncached prompt token
	PerCachedToken time.Duration // prefill cost per cached prompt token
	Jitter         float64       // sigma of the lognormal multiplier on TTFT
	TokensPerSec   float64       // decode speed
}

// Faults are per-request probabilities, settable at runtime via /admin/faults.
type Faults struct {
	RateLimited float64 `json:"rate_limited"` // 429
	ServerError float64 `json:"server_error"` // 503 (OpenAI) / 529 overloaded (Anthropic)
	StallFirst  float64 `json:"stall_first"`  // accept the request, never send a byte
	StallMid    float64 `json:"stall_mid"`    // stop sending halfway through the reply
}

// Config configures a Server.
type Config struct {
	Name             string
	TimeScale        float64
	Latency          Latency
	Faults           Faults
	OpenAIMachines   int
	OpenAIOverflow   int // requests/minute per prefix+key before overflow
	AnthropicMinimum int // minimum cacheable prefix, tokens
	CacheTTL         time.Duration
}

// Server is one simulated provider.
type Server struct {
	cfg    Config
	clock  clock
	oa     *openAICache
	an     *anthropicCache
	mu     sync.Mutex
	faults Faults
	stats  struct{ requests, promptTokens, cachedTokens, outputTokens, faults atomic.Int64 }
}

func New(cfg Config) *Server {
	s := &Server{
		cfg:    cfg,
		clock:  clock{start: time.Now(), scale: cfg.TimeScale},
		oa:     newOpenAICache(cfg.OpenAIMachines, cfg.OpenAIOverflow, cfg.CacheTTL),
		an:     newAnthropicCache(cfg.AnthropicMinimum, cfg.CacheTTL),
		faults: cfg.Faults,
	}
	go func() {
		for range time.Tick(5 * time.Second) {
			now := s.clock.now()
			s.oa.sweep(now)
			s.an.sweep(now)
		}
	}()
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.openAI)
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]any{"data": []any{}}) })
	mux.HandleFunc("POST /v1/messages", s.anthropic)
	mux.HandleFunc("GET /admin/faults", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, s.currentFaults()) })
	mux.HandleFunc("POST /admin/faults", s.setFaults)
	mux.HandleFunc("GET /admin/stats", s.statsHandler)
	mux.HandleFunc("POST /admin/kill", func(w http.ResponseWriter, r *http.Request) {
		slog.Warn("killed via /admin/kill", "provider", s.cfg.Name)
		w.WriteHeader(http.StatusAccepted)
		w.(http.Flusher).Flush()
		go func() { time.Sleep(20 * time.Millisecond); os.Exit(137) }() // sockets die with the process, like kill -9
	})
	return mux
}

func (s *Server) currentFaults() Faults {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.faults
}

func (s *Server) setFaults(w http.ResponseWriter, r *http.Request) {
	var f Faults
	if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.faults = f
	s.mu.Unlock()
	writeJSON(w, 200, f)
}

func (s *Server) statsHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]int64{
		"requests": s.stats.requests.Load(), "prompt_tokens": s.stats.promptTokens.Load(),
		"cached_tokens": s.stats.cachedTokens.Load(), "output_tokens": s.stats.outputTokens.Load(),
		"faults": s.stats.faults.Load(),
	})
}

// fault decides this request's fate.
type fault int

const (
	none fault = iota
	rateLimited
	serverError
	stallFirst
	stallMid
)

func (s *Server) roll() fault {
	f := s.currentFaults()
	x := rand.Float64()
	for _, c := range []struct {
		p float64
		f fault
	}{{f.RateLimited, rateLimited}, {f.ServerError, serverError}, {f.StallFirst, stallFirst}, {f.StallMid, stallMid}} {
		if x < c.p {
			s.stats.faults.Add(1)
			return c.f
		}
		x -= c.p
	}
	return none
}

// ttft is the simulated time to first token.
func (s *Server) ttft(uncached, cached int) time.Duration {
	l := s.cfg.Latency
	d := l.Base + time.Duration(uncached)*l.PerToken + time.Duration(cached)*l.PerCachedToken
	return time.Duration(float64(d) * math.Exp(rand.NormFloat64()*l.Jitter))
}

// conversation is what a provider-specific request boils down to.
type conversation struct {
	persona, lastUser, partial string
	maxTokens                  int
}

// answer is the text this request should stream: the whole reply, or the rest
// of it after a continuation instruction.
func (c conversation) answer() string {
	full := Reply(c.persona, c.lastUser, c.maxTokens)
	if c.partial != "" {
		if rest, ok := strings.CutPrefix(full, c.partial); ok {
			return rest
		}
	}
	return full
}

// pace sends pieces of text at the decode rate. It returns false if the
// stream should stall: the caller then hangs until the client gives up.
func (s *Server) pace(r *http.Request, text string, f fault, send func(piece string) error) bool {
	ps := pieces(text)
	stallAt := -1
	if f == stallMid {
		stallAt = len(ps) / 2
	}
	for i, p := range ps {
		if i == stallAt {
			<-r.Context().Done()
			return false
		}
		if p == "" {
			continue
		}
		if err := send(p); err != nil {
			return false
		}
		select {
		case <-time.After(time.Duration(float64(tok.Count(p)) / s.cfg.Latency.TokensPerSec * float64(time.Second))):
		case <-r.Context().Done():
			return false
		}
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func sse(w http.ResponseWriter) func(event string, v any) error {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	return func(event string, v any) error {
		b, _ := json.Marshal(v)
		if event != "" {
			fmt.Fprintf(w, "event: %s\n", event)
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return err
		}
		return rc.Flush()
	}
}

func sleep(r *http.Request, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-r.Context().Done():
		return false
	}
}
