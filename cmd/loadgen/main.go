// Command loadgen replays realistic character-chat sessions against the proxy:
// many concurrent sessions, each a multi-turn conversation with one persona,
// with think time between turns. It measures time to first token, checks that
// every streamed reply arrived whole (fakellm replies carry a checksum), can
// kill a provider mid-run, and writes a JSON result.
//
//	loadgen -label tuned -sessions 1000 -kill-url http://localhost:9001/admin/kill -kill-after 30s
//	loadgen -compare bench/results/baseline.json,bench/results/tuned.json > bench/RESULTS.md
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/fakellm"
)

type config struct {
	proxy, key, label, out, runID, killURL string
	sessions, turnsMin, turnsMax, personas int
	thinkMin, thinkMax, ramp, timeout      time.Duration
	killAfter                              time.Duration
	seed                                   uint64
}

func main() {
	var c config
	flag.StringVar(&c.proxy, "proxy", "http://localhost:8080", "proxy base URL")
	flag.StringVar(&c.key, "key", "demo-key", "proxy API key")
	flag.StringVar(&c.label, "label", "run", "name for this run (baseline, tuned, ...)")
	flag.StringVar(&c.out, "out", "", "write the JSON result here")
	flag.StringVar(&c.runID, "run-id", strconv.FormatInt(time.Now().Unix(), 36), "prefix for user and conversation IDs")
	flag.IntVar(&c.sessions, "sessions", 1000, "concurrent chat sessions")
	flag.IntVar(&c.turnsMin, "turns-min", 16, "fewest turns per session")
	flag.IntVar(&c.turnsMax, "turns-max", 48, "most turns per session")
	flag.IntVar(&c.personas, "personas", 8, "distinct personas")
	flag.DurationVar(&c.thinkMin, "think-min", 500*time.Millisecond, "shortest pause between turns")
	flag.DurationVar(&c.thinkMax, "think-max", 2500*time.Millisecond, "longest pause between turns")
	flag.DurationVar(&c.ramp, "ramp", 10*time.Second, "spread session starts over this long")
	flag.DurationVar(&c.timeout, "timeout", 30*time.Second, "a turn with no [DONE] by then counts as dropped")
	flag.StringVar(&c.killURL, "kill-url", "", "POST here mid-run to kill a provider (fakellm /admin/kill)")
	flag.DurationVar(&c.killAfter, "kill-after", 30*time.Second, "when to hit -kill-url")
	flag.Uint64Var(&c.seed, "seed", 1, "workload seed: same seed, same conversations")
	compare := flag.String("compare", "", "comma-separated result files: print a markdown comparison and exit")
	flag.Parse()

	if *compare != "" {
		if err := printComparison(os.Stdout, strings.Split(*compare, ",")); err != nil {
			log.Fatal(err)
		}
		return
	}
	res, err := run(c)
	if err != nil {
		log.Fatal(err)
	}
	b, _ := json.MarshalIndent(res, "", "  ")
	if c.out != "" {
		os.MkdirAll(filepath.Dir(c.out), 0o755)
		if err := os.WriteFile(c.out, b, 0o644); err != nil {
			log.Printf("could not write %s: %v (result printed below)", c.out, err)
		}
	}
	fmt.Println(string(b))
}

// Result is one run's outcome.
type Result struct {
	Label           string         `json:"label"`
	Sessions        int            `json:"sessions"`
	Turns           int            `json:"turns"`
	Completed       int            `json:"completed"`
	Dropped         int            `json:"dropped_streams"` // ended without [DONE], errored in-band, or timed out
	Corrupt         int            `json:"corrupt_replies"` // arrived but failed the checksum
	BudgetBlocked   int            `json:"budget_blocked"`
	HTTPErrors      int            `json:"http_errors"`
	TTFTms          Percentiles    `json:"ttft_ms"`
	TurnMs          Percentiles    `json:"turn_ms"`
	CostUSD         float64        `json:"cost_usd"` // from the proxy: turns, summaries, failed attempts
	CostPer1kTurns  float64        `json:"cost_per_1k_turns_usd"`
	Failovers       map[string]int `json:"failovers"`
	Summaries       map[string]int `json:"summaries"`
	PromptTokens    int64          `json:"prompt_tokens"`
	CachedTokens    int64          `json:"cached_prompt_tokens"`
	CompletionTkns  int64          `json:"completion_tokens"`
	KilledAt        string         `json:"provider_killed_at,omitempty"`
	InFlightAtKill  int64          `json:"streams_in_flight_at_kill,omitempty"`
	DurationSeconds float64        `json:"duration_seconds"`
}

type Percentiles struct {
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
}

func percentiles(ds []time.Duration) Percentiles {
	if len(ds) == 0 {
		return Percentiles{}
	}
	slices.Sort(ds)
	at := func(q float64) float64 {
		return float64(ds[min(len(ds)-1, int(q*float64(len(ds))))].Microseconds()) / 1000
	}
	return Percentiles{at(0.50), at(0.95), at(0.99)}
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// turnResult is what one request produced.
type turnResult struct {
	ttft, total            time.Duration
	text                   string
	done, inBandErr        bool
	status                 int
	prompt, cached, output int64
}

func run(c config) (*Result, error) {
	rng := rand.New(rand.NewPCG(c.seed, 99))
	ps := personas(c.personas, rng)
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: c.sessions + 16, MaxConnsPerHost: 0}}
	for _, p := range ps {
		if err := register(client, c, p); err != nil {
			return nil, err
		}
	}

	var (
		mu       sync.Mutex
		res      = &Result{Label: c.label, Sessions: c.sessions, Failovers: map[string]int{}, Summaries: map[string]int{}}
		ttfts    []time.Duration
		totals   []time.Duration
		inFlight atomic.Int64
	)
	start := time.Now()
	if c.killURL != "" {
		go func() {
			time.Sleep(c.killAfter)
			n, at := inFlight.Load(), time.Since(start).Round(time.Millisecond).String()
			mu.Lock()
			res.InFlightAtKill, res.KilledAt = n, at
			mu.Unlock()
			resp, err := http.Post(c.killURL, "application/json", nil)
			if err == nil {
				resp.Body.Close()
			}
			log.Printf("killed provider at %s with %d streams in flight", at, n)
		}()
	}

	users := max(1, c.sessions*7/10) // some users run more than one conversation
	var wg sync.WaitGroup
	for s := range c.sessions {
		srng := rand.New(rand.NewPCG(c.seed, uint64(s)+1000))
		p := ps[zipf(srng, len(ps))]
		user := fmt.Sprintf("%s-user-%d", c.runID, s%users)
		conv := fmt.Sprintf("%s-conv-%d", c.runID, s)
		turns := c.turnsMin + srng.IntN(c.turnsMax-c.turnsMin+1)
		wg.Go(func() {
			time.Sleep(time.Duration(srng.Int64N(int64(c.ramp) + 1)))
			var history []message
			for range turns {
				history = append(history, message{"user", userTurn(srng)})
				inFlight.Add(1)
				tr := turn(client, c, p.ID, user, conv, history)
				inFlight.Add(-1)

				mu.Lock()
				res.Turns++
				res.PromptTokens += tr.prompt
				res.CachedTokens += tr.cached
				res.CompletionTkns += tr.output
				switch {
				case tr.status == http.StatusTooManyRequests:
					res.BudgetBlocked++
				case tr.status != http.StatusOK:
					res.HTTPErrors++
				case !tr.done || tr.inBandErr:
					res.Dropped++
				case !fakellm.Verify(tr.text):
					res.Corrupt++
				default:
					res.Completed++
					ttfts = append(ttfts, tr.ttft)
					totals = append(totals, tr.total)
				}
				mu.Unlock()
				if tr.status == http.StatusTooManyRequests {
					return // out of budget for today
				}
				if tr.text == "" {
					history = history[:len(history)-1] // the app would let the user retry
				} else {
					history = append(history, message{"assistant", tr.text})
				}
				time.Sleep(c.thinkMin + time.Duration(srng.Int64N(int64(c.thinkMax-c.thinkMin)+1)))
			}
		})
	}
	wg.Wait()
	res.DurationSeconds = time.Since(start).Seconds()
	res.TTFTms = percentiles(ttfts)
	res.TurnMs = percentiles(totals)
	if err := scrape(client, c.proxy, res); err != nil {
		log.Printf("could not read proxy metrics: %v", err)
	}
	if res.Turns > 0 {
		res.CostPer1kTurns = res.CostUSD / float64(res.Turns) * 1000
	}
	return res, nil
}

// register stores a persona on the proxy, waiting up to 30 s for it to come up.
func register(client *http.Client, c config, p persona) error {
	body, _ := json.Marshal(map[string]string{"prompt": p.Prompt, "name": p.Name})
	var err error
	for range 60 {
		req, _ := http.NewRequest(http.MethodPut, c.proxy+"/v1/personas/"+p.ID, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+c.key)
		var resp *http.Response
		if resp, err = client.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode/100 == 2 {
				return nil
			}
			return fmt.Errorf("register persona %s: HTTP %d", p.ID, resp.StatusCode)
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("register persona: %w", err)
}

// turn sends one chat turn and reads the SSE stream to the end.
func turn(client *http.Client, c config, personaID, user, conv string, history []message) turnResult {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	body, _ := json.Marshal(map[string]any{"persona_id": personaID, "messages": history, "stream": true, "max_tokens": 400})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.proxy+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("X-User-ID", user)
	req.Header.Set("X-Conversation-ID", conv)
	start := time.Now()
	var tr turnResult
	resp, err := client.Do(req)
	if err != nil {
		return tr
	}
	defer resp.Body.Close()
	tr.status = resp.StatusCode
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return tr
	}
	var text strings.Builder
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		data, ok := bytes.CutPrefix(sc.Bytes(), []byte("data: "))
		if !ok {
			continue
		}
		if string(data) == "[DONE]" {
			tr.done = true
			break
		}
		var ch struct {
			Error   json.RawMessage `json:"error"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens        int64 `json:"prompt_tokens"`
				CompletionTokens    int64 `json:"completion_tokens"`
				PromptTokensDetails struct {
					CachedTokens int64 `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
			} `json:"usage"`
		}
		if json.Unmarshal(data, &ch) != nil {
			continue
		}
		if len(ch.Error) > 0 {
			tr.inBandErr = true
		}
		if len(ch.Choices) > 0 && ch.Choices[0].Delta.Content != "" {
			if text.Len() == 0 {
				tr.ttft = time.Since(start)
			}
			text.WriteString(ch.Choices[0].Delta.Content)
		}
		if ch.Usage != nil {
			tr.prompt, tr.cached, tr.output = ch.Usage.PromptTokens, ch.Usage.PromptTokensDetails.CachedTokens, ch.Usage.CompletionTokens
		}
	}
	tr.total = time.Since(start)
	tr.text = text.String()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		tr.done = false
	}
	return tr
}

// scrape reads the proxy's cost and failover counters from /metrics.
func scrape(client *http.Client, proxy string, res *Result) error {
	resp, err := client.Get(proxy + "/metrics")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		name, rest, _ := strings.Cut(line, "{")
		if strings.HasPrefix(line, "#") {
			continue
		}
		val := line[strings.LastIndexByte(line, ' ')+1:]
		v, err := strconv.ParseFloat(val, 64)
		if err != nil {
			continue
		}
		switch name {
		case "persona_cost_usd_total":
			res.CostUSD += v
		case "persona_failovers_total":
			res.Failovers[label(rest, "reason")] += int(v)
		case "persona_summaries_total":
			res.Summaries[label(rest, "result")] += int(v)
		}
	}
	return sc.Err()
}

// label extracts one label value from a Prometheus series' label set.
func label(labels, name string) string {
	_, after, ok := strings.Cut(labels, name+`="`)
	if !ok {
		return ""
	}
	v, _, _ := strings.Cut(after, `"`)
	return v
}
