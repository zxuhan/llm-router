//go:build ignore

// aggregate reads N per-strategy summary JSONs and writes one Markdown
// document on stdout. Two modes:
//
//   default ("single"): one JSON per strategy. Output is the same table as
//                       cmd/bench's --markdown.
//   --multi:            multiple JSONs per strategy (one per seed). Output
//                       includes mean and stddev for the headline metrics.
//
// Used by bench/scripts/real-llm.sh; gated by `//go:build ignore` so it does
// not participate in `go build ./...`.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/xzhou/llm-router/internal/trace"
)

func main() {
	multi := flag.Bool("multi", false, "treat inputs as multiple runs per strategy and report CIs")
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: aggregate [--multi] <file.json> [<file.json> ...]")
		os.Exit(2)
	}

	if !*multi {
		var summaries []trace.Summary
		for _, p := range args {
			summaries = append(summaries, mustReadOne(p)...)
		}
		if err := trace.WriteMarkdown(summaries, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	// Multi-run mode: group inputs by strategy, then aggregate.
	byStrat := map[string][]trace.Summary{}
	for _, p := range args {
		for _, s := range mustReadOne(p) {
			byStrat[s.Strategy] = append(byStrat[s.Strategy], s)
		}
	}

	// Stable strategy order matching the rest of the project.
	order := []string{"roundrobin", "random", "leastloaded", "prefixaware"}
	stratList := make([]string, 0, len(byStrat))
	seen := map[string]bool{}
	for _, s := range order {
		if _, ok := byStrat[s]; ok {
			stratList = append(stratList, s)
			seen[s] = true
		}
	}
	for s := range byStrat {
		if !seen[s] {
			stratList = append(stratList, s)
		}
	}

	w := os.Stdout
	fmt.Fprintln(w, "# LLM Router Benchmark Report (multi-run)")
	fmt.Fprintln(w)
	runs := len(byStrat[stratList[0]])
	fmt.Fprintf(w, "%d runs per strategy. Each run boots fresh workers; numbers are mean ± stddev across runs.\n\n", runs)

	fmt.Fprintln(w, "## Headline metrics (mean ± stddev)")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "| Strategy | Hit rate | KV cached | TTFT p50 | TTFT p95 | TTFT p99 | RPS |")
	fmt.Fprintln(w, "| --- | ---: | ---: | ---: | ---: | ---: | ---: |")
	for _, st := range stratList {
		runs := byStrat[st]
		hr := meanStdF(extractF(runs, func(s trace.Summary) float64 { return s.HitRate * 100 }))
		ck := meanStdF(extractF(runs, func(s trace.Summary) float64 { return s.CacheTokenRate * 100 }))
		t50 := meanStdD(extractD(runs, func(s trace.Summary) time.Duration { return s.TTFT.P50 }))
		t95 := meanStdD(extractD(runs, func(s trace.Summary) time.Duration { return s.TTFT.P95 }))
		t99 := meanStdD(extractD(runs, func(s trace.Summary) time.Duration { return s.TTFT.P99 }))
		rps := meanStdF(extractF(runs, func(s trace.Summary) float64 { return s.ThroughputRPS }))
		fmt.Fprintf(w, "| %s | %s%% | %s%% | %s | %s | %s | %s |\n",
			st, fmtMeanStd(hr, 2), fmtMeanStd(ck, 2),
			fmtDur(t50.mean)+" ± "+fmtDur(t50.std),
			fmtDur(t95.mean)+" ± "+fmtDur(t95.std),
			fmtDur(t99.mean)+" ± "+fmtDur(t99.std),
			fmtMeanStd(rps, 2))
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "## Per-run detail")
	fmt.Fprintln(w)
	for _, st := range stratList {
		fmt.Fprintf(w, "### %s\n\n", st)
		fmt.Fprintln(w, "| Run | Successful | Hit rate | KV cached | TTFT p50 | TTFT p95 | RPS |")
		fmt.Fprintln(w, "| ---: | ---: | ---: | ---: | ---: | ---: | ---: |")
		for i, s := range byStrat[st] {
			fmt.Fprintf(w, "| %d | %d | %.2f%% | %.2f%% | %s | %s | %.2f |\n",
				i+1, s.Successful, s.HitRate*100, s.CacheTokenRate*100,
				fmtDur(s.TTFT.P50), fmtDur(s.TTFT.P95), s.ThroughputRPS)
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintln(w, "## Notes")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "- Each run uses a different seed (SEED, SEED+1, ..., SEED+RUNS-1) and")
	fmt.Fprintln(w, "  fresh workers, so runs are statistically independent in their tail")
	fmt.Fprintln(w, "  observations.")
	fmt.Fprintln(w, "- KV cached is the upstream cached_tokens / prompt_tokens averaged over")
	fmt.Fprintln(w, "  successful requests in each run.")
	fmt.Fprintln(w, "- Stddev is computed with the corrected (sample) formula (n-1).")
}

// mustReadOne loads either a {\"summaries\": [...]} doc or a raw Summary.
func mustReadOne(path string) []trace.Summary {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read %s: %v\n", path, err)
		os.Exit(1)
	}
	var doc struct {
		Summaries []trace.Summary `json:"summaries"`
	}
	if err := json.Unmarshal(data, &doc); err == nil && len(doc.Summaries) > 0 {
		return doc.Summaries
	}
	var single trace.Summary
	if err := json.Unmarshal(data, &single); err != nil {
		fmt.Fprintf(os.Stderr, "parse %s: %v\n", path, err)
		os.Exit(1)
	}
	return []trace.Summary{single}
}

type stat struct {
	mean float64
	std  float64
}
type statD struct {
	mean time.Duration
	std  time.Duration
}

func extractF(rs []trace.Summary, get func(trace.Summary) float64) []float64 {
	out := make([]float64, len(rs))
	for i, r := range rs {
		out[i] = get(r)
	}
	return out
}

func extractD(rs []trace.Summary, get func(trace.Summary) time.Duration) []time.Duration {
	out := make([]time.Duration, len(rs))
	for i, r := range rs {
		out[i] = get(r)
	}
	return out
}

func meanStdF(xs []float64) stat {
	if len(xs) == 0 {
		return stat{}
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	mean := sum / float64(len(xs))
	if len(xs) == 1 {
		return stat{mean: mean}
	}
	var sq float64
	for _, x := range xs {
		sq += (x - mean) * (x - mean)
	}
	return stat{mean: mean, std: math.Sqrt(sq / float64(len(xs)-1))}
}

func meanStdD(xs []time.Duration) statD {
	if len(xs) == 0 {
		return statD{}
	}
	var sum time.Duration
	for _, x := range xs {
		sum += x
	}
	mean := sum / time.Duration(len(xs))
	if len(xs) == 1 {
		return statD{mean: mean}
	}
	var sq float64
	for _, x := range xs {
		d := float64(x - mean)
		sq += d * d
	}
	return statD{mean: mean, std: time.Duration(math.Sqrt(sq / float64(len(xs)-1)))}
}

func fmtMeanStd(s stat, prec int) string {
	if s.std == 0 {
		return fmt.Sprintf("%.*f", prec, s.mean)
	}
	return fmt.Sprintf("%.*f ± %.*f", prec, s.mean, prec, s.std)
}

func fmtDur(d time.Duration) string {
	if d == 0 {
		return "-"
	}
	if d < time.Microsecond {
		return fmt.Sprintf("%dns", d.Nanoseconds())
	}
	if d < time.Millisecond {
		return fmt.Sprintf("%.1fµs", float64(d.Nanoseconds())/1e3)
	}
	if d < time.Second {
		return fmt.Sprintf("%.0fms", float64(d.Nanoseconds())/1e6)
	}
	return fmt.Sprintf("%.2fs", d.Seconds())
}

// keep imports tidy when changes happen elsewhere
var _ = sort.Strings
var _ = strings.HasPrefix
