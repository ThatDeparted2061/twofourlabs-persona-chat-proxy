package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// printComparison writes a markdown table comparing result files, with the
// change from the first run to the last.
func printComparison(w io.Writer, paths []string) error {
	var rs []Result
	for _, p := range paths {
		b, err := os.ReadFile(strings.TrimSpace(p))
		if err != nil {
			return err
		}
		var r Result
		if err := json.Unmarshal(b, &r); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		rs = append(rs, r)
	}
	first, last := rs[0], rs[len(rs)-1]
	head := "| | " + strings.Join(func() []string {
		var ls []string
		for _, r := range rs {
			ls = append(ls, r.Label)
		}
		return ls
	}(), " | ") + " | change |"
	fmt.Fprintln(w, head)
	fmt.Fprintln(w, "|---"+strings.Repeat("|---:", len(rs)+1)+"|")
	row := func(name string, get func(Result) float64, format string) {
		fmt.Fprintf(w, "| %s |", name)
		for _, r := range rs {
			fmt.Fprintf(w, " "+format+" |", get(r))
		}
		a, b := get(first), get(last)
		switch {
		case a == 0 && b == 0:
			fmt.Fprintln(w, " = |")
		case a == 0:
			fmt.Fprintln(w, " new |")
		default:
			fmt.Fprintf(w, " %+.1f%% |\n", (b-a)/a*100)
		}
	}
	row("Cost per 1,000 turns (USD)", func(r Result) float64 { return r.CostPer1kTurns }, "$%.4f")
	row("TTFT p50 (ms)", func(r Result) float64 { return r.TTFTms.P50 }, "%.0f")
	row("TTFT p95 (ms)", func(r Result) float64 { return r.TTFTms.P95 }, "%.0f")
	row("TTFT p99 (ms)", func(r Result) float64 { return r.TTFTms.P99 }, "%.0f")
	row("Dropped streams", func(r Result) float64 { return float64(r.Dropped) }, "%.0f")
	row("Corrupt replies", func(r Result) float64 { return float64(r.Corrupt) }, "%.0f")
	row("Turns", func(r Result) float64 { return float64(r.Turns) }, "%.0f")
	row("Budget-blocked or rate-limited turns", func(r Result) float64 { return float64(r.BudgetBlocked + r.RateLimited) }, "%.0f")
	row("Concurrent sessions", func(r Result) float64 { return float64(r.Sessions) }, "%.0f")
	row("Streams in flight at kill", func(r Result) float64 { return float64(r.InFlightAtKill) }, "%.0f")
	row("Cached share of prompt tokens", func(r Result) float64 {
		if r.PromptTokens == 0 {
			return 0
		}
		return float64(r.CachedTokens) / float64(r.PromptTokens) * 100
	}, "%.1f%%")
	row("Prompt tokens per turn", func(r Result) float64 { return float64(r.PromptTokens) / float64(max(1, r.Turns)) }, "%.0f")
	row("Summaries applied to prompts", func(r Result) float64 { return float64(r.Summaries["applied"]) }, "%.0f")
	row("Provider failovers", func(r Result) float64 {
		n := 0
		for _, v := range r.Failovers {
			n += v
		}
		return float64(n)
	}, "%.0f")
	return nil
}
