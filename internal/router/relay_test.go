package router

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/fakellm"
	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/providers"
)

func upstream(t *testing.T, f fakellm.Faults) *httptest.Server {
	srv := httptest.NewServer(fakellm.New(fakellm.Config{
		TimeScale: 1, OpenAIMachines: 2, OpenAIOverflow: 15, AnthropicMinimum: 512, CacheTTL: time.Minute,
		Latency: fakellm.Latency{Base: 5 * time.Millisecond, TokensPerSec: 2000}, Faults: f,
	}).Handler())
	t.Cleanup(srv.Close)
	return srv
}

func turnReq() providers.ChatRequest {
	return providers.ChatRequest{Messages: []providers.Message{
		{Role: "system", Content: "You are Kai."},
		{Role: "user", Content: "tell me a long story about the lighthouse"},
	}, MaxTokens: 400}
}

func relay(t *testing.T, r *Router, opt RelayOptions) (*RelayResult, string, error) {
	var got strings.Builder
	res, err := r.Relay(context.Background(), turnReq(), opt, func(s string) error { got.WriteString(s); return nil })
	return res, got.String(), err
}

var opts = RelayOptions{FirstTokenTimeout: 300 * time.Millisecond, StallTimeout: 200 * time.Millisecond, MaxAttempts: 4, Resume: true}

func TestRelayResumesAfterMidStreamStall(t *testing.T) {
	primary := upstream(t, fakellm.Faults{StallMid: 1})
	backup := upstream(t, fakellm.Faults{})
	r := New(providers.NewOpenAI(primary.URL+"/v1", "k", "gpt-4.1-mini"), providers.NewAnthropic(backup.URL+"/v1", "k", "claude-haiku-5-5", true))
	res, text, err := relay(t, r, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !fakellm.Verify(text) || text != res.Text {
		t.Fatalf("client did not get one whole reply: %q", text)
	}
	if len(res.Failovers) != 1 || res.Failovers[0].Reason != "stall" || res.Provider != "anthropic" {
		t.Fatalf("want one stall failover to anthropic, got %+v provider=%s", res.Failovers, res.Provider)
	}
	if len(res.Attempts) != 2 || !res.Attempts[0].Estimated {
		t.Fatalf("the stalled attempt must still be billed (estimated): %+v", res.Attempts)
	}
}

func TestRelayResumesWhenProviderDies(t *testing.T) {
	primary := upstream(t, fakellm.Faults{StallMid: 1})
	backup := upstream(t, fakellm.Faults{})
	r := New(providers.NewOpenAI(primary.URL+"/v1", "k", "m"), providers.NewOpenAI(backup.URL+"/v1", "k", "m"))
	go func() { time.Sleep(100 * time.Millisecond); primary.CloseClientConnections() }()
	o := opts
	o.StallTimeout = 10 * time.Second // the kill, not the stall timer, must trigger failover
	res, text, err := relay(t, r, o)
	if err != nil || !fakellm.Verify(text) {
		t.Fatalf("err=%v text=%q", err, text)
	}
	if len(res.Failovers) != 1 || (res.Failovers[0].Reason != "stream_error" && res.Failovers[0].Reason != "eof") {
		t.Fatalf("want a stream_error/eof failover, got %+v", res.Failovers)
	}
}

func TestRelayFailsOverOnSlowFirstToken(t *testing.T) {
	primary := upstream(t, fakellm.Faults{StallFirst: 1})
	backup := upstream(t, fakellm.Faults{})
	r := New(providers.NewOpenAI(primary.URL+"/v1", "k", "m"), providers.NewOpenAI(backup.URL+"/v1", "k", "m"))
	res, text, err := relay(t, r, opts)
	if err != nil || !fakellm.Verify(text) || res.Failovers[0].Reason != "first_token_timeout" {
		t.Fatalf("err=%v failovers=%+v", err, res.Failovers)
	}
	if res.TTFT < opts.FirstTokenTimeout {
		t.Fatalf("TTFT should include the wasted wait, got %v", res.TTFT)
	}
}

func TestBreakerSkipsDeadProvider(t *testing.T) {
	primary := upstream(t, fakellm.Faults{ServerError: 1})
	backup := upstream(t, fakellm.Faults{})
	r := New(providers.NewOpenAI(primary.URL+"/v1", "k", "m"), providers.NewAnthropic(backup.URL+"/v1", "k", "m", true)) // breakers are per provider name
	for range 3 {
		if _, _, err := relay(t, r, opts); err != nil {
			t.Fatal(err)
		}
	}
	res, _, _ := relay(t, r, opts)
	if len(res.Failovers) != 0 {
		t.Fatalf("breaker should route straight to the backup after 3 failures, got %+v", res.Failovers)
	}
}

func TestPassthroughLeavesMidStreamFailuresToTheClient(t *testing.T) {
	primary := upstream(t, fakellm.Faults{StallMid: 1})
	backup := upstream(t, fakellm.Faults{})
	r := New(providers.NewOpenAI(primary.URL+"/v1", "k", "m"), providers.NewOpenAI(backup.URL+"/v1", "k", "m"))
	go func() { time.Sleep(100 * time.Millisecond); primary.CloseClientConnections() }()
	res, err := r.Passthrough(context.Background(), turnReq(), func(string) error { return nil })
	if err == nil && fakellm.Verify(res.Text) {
		t.Fatal("baseline should not recover a broken stream")
	}
}

func TestBreakerProbesOncePerCooldown(t *testing.T) {
	b := &breaker{name: "x", threshold: 3, cooldown: 50 * time.Millisecond}
	for range 3 {
		b.record(false)
	}
	if b.allow() {
		t.Fatal("open breaker must refuse")
	}
	time.Sleep(60 * time.Millisecond)
	if !b.allow() || b.allow() {
		t.Fatal("after the cooldown exactly one probe may pass")
	}
	b.record(true)
	if !b.allow() || !b.allow() {
		t.Fatal("a successful probe closes the breaker")
	}
}
