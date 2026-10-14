package router

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/continuation"
	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/metrics"
	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/providers"
	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/tok"
)

// RelayOptions tune streaming failover.
type RelayOptions struct {
	FirstTokenTimeout time.Duration // drop a provider that has not produced a token by then (0 = wait forever)
	StallTimeout      time.Duration // drop a stream that goes quiet this long (0 = wait forever)
	MaxAttempts       int           // total provider attempts per turn
	Resume            bool          // on a mid-stream failure, continue the reply on the next provider
}

// Failover is one switch from a failing provider to the next.
type Failover struct{ From, To, Reason string }

// Attempt is one provider call and what it cost. Usage is estimated when the
// attempt died before the provider reported it.
type Attempt struct {
	Provider, Model string
	Usage           providers.Usage
	Estimated       bool
}

// RelayResult describes a streamed turn.
type RelayResult struct {
	Provider, Model string // the provider that finished the reply
	Text            string
	FinishReason    string
	TTFT            time.Duration
	Attempts        []Attempt
	Failovers       []Failover
}

// Usage sums every attempt, including failed ones that were still billed.
func (r *RelayResult) Usage() providers.Usage {
	var u providers.Usage
	for _, a := range r.Attempts {
		u.PromptTokens += a.Usage.PromptTokens
		u.CompletionTokens += a.Usage.CompletionTokens
		u.TotalTokens += a.Usage.TotalTokens
		u.CacheWriteTokens += a.Usage.CacheWriteTokens
		if c := a.Usage.Cached(); c > 0 {
			if u.PromptTokensDetails == nil {
				u.PromptTokensDetails = &providers.PromptTokensDetails{}
			}
			u.PromptTokensDetails.CachedTokens += c
		}
	}
	return u
}

// ErrClientGone means emit failed: the caller's client disconnected.
var ErrClientGone = errors.New("client went away")

// Relay streams a completion through emit, failing over between providers
// without the client noticing. Before the first token it fails over on errors
// and on FirstTokenTimeout. After it, a stream that errors, ends early or
// stalls for StallTimeout is cancelled and, with Resume, the next provider is
// asked to continue the reply from the last byte the client received. The
// client sees one uninterrupted stream.
func (r *Router) Relay(ctx context.Context, req providers.ChatRequest, opt RelayOptions, emit func(string) error) (*RelayResult, error) {
	start := time.Now()
	res := &RelayResult{Failovers: []Failover{}}
	var sent strings.Builder // everything the client has received
	order := r.available()
	for i := 0; i < max(1, opt.MaxAttempts); i++ {
		p := order[i%len(order)]
		areq := req
		if p != r.providers[0] {
			areq.Model = "" // model names are provider-specific
		}
		if sent.Len() > 0 {
			if !opt.Resume {
				break
			}
			areq.Messages = append(slices.Clone(req.Messages), providers.Message{Role: "system", Content: continuation.Instruction(sent.String())})
		}
		reason, err := r.attempt(ctx, p, areq, opt, start, emit, &sent, res)
		if err == nil {
			r.breaker(p.Name()).record(true)
			res.Provider, res.Text = p.Name(), sent.String()
			return res, nil
		}
		if errors.Is(err, ErrClientGone) || ctx.Err() != nil {
			return res, ErrClientGone
		}
		r.breaker(p.Name()).record(false)
		next := order[(i+1)%len(order)]
		res.Failovers = append(res.Failovers, Failover{From: p.Name(), To: next.Name(), Reason: reason})
		metrics.Failover(p.Name(), next.Name(), reason)
	}
	res.Text = sent.String()
	return res, errors.New("all providers failed")
}

// attempt runs one provider. It returns a failure reason and error, or nil
// once the provider finished the reply.
func (r *Router) attempt(ctx context.Context, p providers.Provider, req providers.ChatRequest, opt RelayOptions,
	start time.Time, emit func(string) error, sent *strings.Builder, res *RelayResult) (string, error) {
	actx, cancel := context.WithCancel(ctx)
	defer cancel()

	// One timer covers both phases: first token, then gaps between tokens.
	var streaming atomic.Bool
	var why atomic.Value
	timer := (*time.Timer)(nil)
	if opt.FirstTokenTimeout > 0 || opt.StallTimeout > 0 {
		first := opt.FirstTokenTimeout
		if first == 0 {
			first = 24 * time.Hour
		}
		timer = time.AfterFunc(first, func() {
			why.Store(map[bool]string{false: "first_token_timeout", true: "stall"}[streaming.Load()])
			cancel()
		})
		defer timer.Stop()
	}
	reason := func(fallback string) string {
		if w, ok := why.Load().(string); ok {
			return w
		}
		return fallback
	}

	callStart := time.Now()
	ch, err := p.ChatStream(actx, req)
	metrics.ObserveCall(p.Name(), err, time.Since(callStart))
	if err != nil {
		var se *providers.StatusError
		if errors.As(err, &se) {
			return classify(se), err // rejected up front: not billed
		}
		res.Attempts = append(res.Attempts, estimate(p.Name(), req, "")) // the provider may have started prefill
		return reason("connect_error"), err
	}

	var (
		here   strings.Builder
		usage  *providers.Usage
		model  string
		finish string
		failed string
	)
	for c := range ch {
		if c.Err != nil {
			failed = "stream_error"
			break
		}
		if c.Model != "" {
			model = c.Model
		}
		if c.Usage != nil {
			usage = c.Usage
		}
		if c.Content != "" {
			if why.Load() != nil {
				break // timed out while this chunk was in flight
			}
			streaming.Store(true)
			if timer != nil && opt.StallTimeout > 0 {
				timer.Reset(opt.StallTimeout)
			} else if timer != nil {
				timer.Stop()
			}
			if res.TTFT == 0 {
				res.TTFT = time.Since(start)
			}
			here.WriteString(c.Content)
			sent.WriteString(c.Content)
			if err := emit(c.Content); err != nil {
				return "", ErrClientGone
			}
		}
		if c.FinishReason != "" {
			finish = c.FinishReason
		}
	}
	if ctx.Err() != nil {
		return "", ErrClientGone
	}
	switch {
	case why.Load() != nil:
		failed = reason("")
	case failed == "" && finish == "":
		failed = "eof" // the connection ended without a finish reason
	}
	if failed != "" {
		if usage != nil {
			res.Attempts = append(res.Attempts, Attempt{Provider: p.Name(), Model: model, Usage: *usage})
		} else {
			res.Attempts = append(res.Attempts, estimate(p.Name(), req, here.String()))
		}
		return failed, errors.New(failed)
	}
	a := Attempt{Provider: p.Name(), Model: model}
	if usage != nil {
		a.Usage = *usage
	} else {
		a = estimate(p.Name(), req, here.String())
		a.Model = model
	}
	res.Attempts = append(res.Attempts, a)
	res.Model, res.FinishReason = model, finish
	return "", nil
}

// estimate bills an attempt the provider never reported: every prompt token
// as uncached (the worst case) plus what it streamed.
func estimate(provider string, req providers.ChatRequest, out string) Attempt {
	in := 0
	for _, m := range req.Messages {
		in += tok.Count(m.Content) + 4
	}
	o := tok.Count(out)
	return Attempt{Provider: provider, Model: req.Model, Estimated: true,
		Usage: providers.Usage{PromptTokens: in, CompletionTokens: o, TotalTokens: in + o}}
}

func classify(se *providers.StatusError) string {
	switch {
	case se.Code == 429:
		return "429"
	case se.Code >= 500:
		return "5xx"
	}
	return "4xx"
}

// available lists providers whose breaker is closed, in failover order. If
// every breaker is open it returns them all: trying beats failing outright.
func (r *Router) available() []providers.Provider {
	var out []providers.Provider
	for _, p := range r.providers {
		if r.breaker(p.Name()).allow() {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return r.providers
	}
	return out
}

func (r *Router) breaker(name string) *breaker {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.breakers == nil {
		r.breakers = map[string]*breaker{}
	}
	b, ok := r.breakers[name]
	if !ok {
		b = &breaker{name: name, threshold: 3, cooldown: 5 * time.Second}
		r.breakers[name] = b
	}
	return b
}

// breaker skips a provider for cooldown after threshold consecutive failures,
// so a dead provider costs one failed probe per cooldown instead of a failed
// attempt per request. After the cooldown it lets a single request through;
// a success closes it, a failure reopens it.
type breaker struct {
	name      string
	mu        sync.Mutex
	fails     int
	halfOpen  bool
	openUntil time.Time
	threshold int
	cooldown  time.Duration
}

func (b *breaker) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if now.Before(b.openUntil) {
		return false
	}
	if b.halfOpen {
		b.openUntil = now.Add(b.cooldown) // this request is the probe; the rest wait for its result
	}
	return true
}

func (b *breaker) record(ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ok {
		b.fails, b.halfOpen, b.openUntil = 0, false, time.Time{}
		metrics.BreakerOpen(b.name, false)
		return
	}
	b.fails++
	if b.fails >= b.threshold {
		b.halfOpen, b.openUntil = true, time.Now().Add(b.cooldown)
		metrics.BreakerOpen(b.name, true)
	}
}

// Passthrough is how LLM-Gateway streamed before this fork, kept as the
// benchmark baseline: failover (with retries) only until a stream opens, no
// timeouts, and a stream that breaks midway is the client's problem.
func (r *Router) Passthrough(ctx context.Context, req providers.ChatRequest, emit func(string) error) (*RelayResult, error) {
	start := time.Now()
	res := &RelayResult{}
	ch, name, err := r.ChatStream(ctx, req)
	if err != nil {
		return res, err
	}
	res.Provider = name
	var (
		text  strings.Builder
		usage *providers.Usage
	)
	bill := func() {
		if usage != nil {
			res.Attempts = append(res.Attempts, Attempt{Provider: name, Model: res.Model, Usage: *usage})
		} else {
			res.Attempts = append(res.Attempts, estimate(name, req, text.String()))
		}
		res.Text = text.String()
	}
	for c := range ch {
		if c.Err != nil {
			bill()
			return res, c.Err
		}
		if c.Model != "" {
			res.Model = c.Model
		}
		if c.Usage != nil {
			usage = c.Usage
		}
		if c.Content != "" {
			if res.TTFT == 0 {
				res.TTFT = time.Since(start)
			}
			text.WriteString(c.Content)
			if err := emit(c.Content); err != nil {
				bill()
				return res, ErrClientGone
			}
		}
		if c.FinishReason != "" {
			res.FinishReason = c.FinishReason
		}
	}
	bill()
	if ctx.Err() != nil {
		return res, ErrClientGone
	}
	return res, nil // an early EOF still ends the stream "successfully", as it always did
}
