package trace

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Percentiles bundles the latency distribution numbers we report. Values are
// computed once per Summary.
type Percentiles struct {
	P50  time.Duration `json:"p50"`
	P95  time.Duration `json:"p95"`
	P99  time.Duration `json:"p99"`
	Mean time.Duration `json:"mean"`
}

// Summary aggregates one strategy's run. All durations are wall-clock measured
// at the replayer (i.e. as the client experiences them).
//
// The decision-class fields are mutually exclusive and partition the
// successful requests:
//
//	Successful = PinnedRequests + SpilledRequests + FallbackRequests + NeutralRequests
//
// HitRequests = PinnedRequests + SpilledRequests (any prefix-driven
// decision); kept as a single rollup for back-compat and for the headline
// table. HitRate = HitRequests / Successful.
type Summary struct {
	Strategy         string         `json:"strategy"`
	Requests         int            `json:"requests"`
	Successful       int            `json:"successful"`
	Errors           int            `json:"errors"`
	HitRequests      int            `json:"hit_requests"`
	HitRate          float64        `json:"hit_rate"`
	PinnedRequests   int            `json:"pinned_requests"`   // strict longest-prefix
	SpilledRequests  int            `json:"spilled_requests"`  // spilled-from-saturated
	FallbackRequests int            `json:"fallback_requests"` // fallback-* and spilled-fallback-*
	NeutralRequests  int            `json:"neutral_requests"`  // non-prefix routers (round-robin etc)
	BackendCounts    map[string]int `json:"backend_counts"`
	TTFT             Percentiles    `json:"ttft"`
	Total            Percentiles    `json:"total"`
	ThroughputRPS    float64        `json:"throughput_rps"`
	WallTimeSeconds  float64        `json:"wall_time_seconds"`
	BytesIn          int64          `json:"bytes_in"`
	// PromptTokens and CachedTokens are summed across requests when the
	// upstream returns OpenAI-style usage. CacheTokenRate is
	// CachedTokens/PromptTokens. UpstreamHits counts requests where
	// CachedTokens > 0 (i.e. the upstream actually skipped *some* prefill).
	// These are the *outcome* signal, distinct from the router-decision
	// signals above.
	PromptTokens    int     `json:"prompt_tokens"`
	CachedTokens    int     `json:"cached_tokens"`
	CacheTokenRate  float64 `json:"cache_token_rate"`
	UpstreamHits    int     `json:"upstream_hits"`
	UpstreamHitRate float64 `json:"upstream_hit_rate"`
}

// Summarise computes a Summary over the given results. Each successful
// request is classified into exactly one decision class based on the
// X-Router-Reason header surfaced by the proxy:
//
//   - "longest-prefix"          -> Pinned   (the prefix-aware logic chose the worker with the longest match)
//   - "spilled-from-saturated"  -> Spilled  (best-match worker was saturated; spilled to a non-saturated next-best match)
//   - "fallback-*"              -> Fallback (no worker met the match threshold)
//   - "spilled-fallback-*"      -> Fallback (best-match saturated and no fallback candidate met threshold)
//   - everything else           -> Neutral  (non-prefix routers)
//
// HitRequests is the convenience rollup Pinned+Spilled. The orthogonal
// outcome signal (UpstreamHits, CacheTokenRate) is computed from
// upstream-reported `usage.prompt_tokens_details.cached_tokens` when
// available; it is independent of the routing decision class.
func Summarise(strategy string, results []Result) Summary {
	s := Summary{Strategy: strategy, BackendCounts: map[string]int{}}
	if len(results) == 0 {
		return s
	}
	s.Requests = len(results)

	ttftSamples := make([]time.Duration, 0, len(results))
	totalSamples := make([]time.Duration, 0, len(results))
	var firstStart, lastEnd time.Time
	for _, r := range results {
		// Treat both transport errors AND non-2xx upstream responses as failures.
		// Folding e.g. a 405 into the percentile bucket would compute "TTFT" of
		// the time-to-error (often <1ms) and call it a successful request, which
		// silently inflates throughput and crushes the latency numbers - the
		// exact thing that caused us to ship a bench run where one dead worker
		// was reported as "PA p50 282µs" because every PA request hit it and
		// was rejected at the FastAPI router level.
		if r.Err != "" || r.StatusCode == 0 || r.StatusCode >= 400 {
			s.Errors++
			continue
		}
		s.Successful++
		s.BytesIn += r.BytesIn
		s.PromptTokens += r.PromptTokens
		s.CachedTokens += r.CachedTokens
		if r.BackendID != "" {
			s.BackendCounts[r.BackendID]++
		}
		if r.CachedTokens > 0 {
			s.UpstreamHits++
		}
		switch {
		case r.Reason == "longest-prefix":
			s.PinnedRequests++
			s.HitRequests++
		case r.Reason == "spilled-from-saturated":
			s.SpilledRequests++
			s.HitRequests++
		case strings.HasPrefix(r.Reason, "fallback-"),
			strings.HasPrefix(r.Reason, "spilled-fallback-"):
			s.FallbackRequests++
		default:
			s.NeutralRequests++
		}
		if r.TTFT > 0 {
			ttftSamples = append(ttftSamples, r.TTFT)
		}
		if r.Total > 0 {
			totalSamples = append(totalSamples, r.Total)
		}
		if firstStart.IsZero() || r.StartedAt.Before(firstStart) {
			firstStart = r.StartedAt
		}
		if r.CompletedAt.After(lastEnd) {
			lastEnd = r.CompletedAt
		}
	}
	if s.Successful > 0 {
		s.HitRate = float64(s.HitRequests) / float64(s.Successful)
		s.UpstreamHitRate = float64(s.UpstreamHits) / float64(s.Successful)
	}
	if s.PromptTokens > 0 {
		s.CacheTokenRate = float64(s.CachedTokens) / float64(s.PromptTokens)
	}
	s.TTFT = computePercentiles(ttftSamples)
	s.Total = computePercentiles(totalSamples)
	if !firstStart.IsZero() && lastEnd.After(firstStart) {
		wall := lastEnd.Sub(firstStart).Seconds()
		s.WallTimeSeconds = wall
		if wall > 0 {
			s.ThroughputRPS = float64(s.Successful) / wall
		}
	}
	return s
}

// computePercentiles returns p50/p95/p99 and the arithmetic mean for the
// given sample slice. The slice is sorted in place. The percentile method is
// nearest-rank with ceiling rounding ("upper" percentile), matching the
// interpretation used by most benchmark tools: P95 means "at least 95% of
// samples are <= this value".
func computePercentiles(samples []time.Duration) Percentiles {
	if len(samples) == 0 {
		return Percentiles{}
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	n := len(samples)
	pick := func(p float64) time.Duration {
		// rank in 1..n
		rank := int(p * float64(n))
		if float64(rank) < p*float64(n) {
			rank++
		}
		if rank < 1 {
			rank = 1
		}
		if rank > n {
			rank = n
		}
		return samples[rank-1]
	}
	var sum time.Duration
	for _, v := range samples {
		sum += v
	}
	return Percentiles{
		P50:  pick(0.50),
		P95:  pick(0.95),
		P99:  pick(0.99),
		Mean: sum / time.Duration(n),
	}
}

// WriteJSON encodes the list of summaries as a single JSON document.
func WriteJSON(summaries []Summary, w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(map[string]any{"summaries": summaries})
}

// WriteMarkdown emits a human-readable comparison report.
func WriteMarkdown(summaries []Summary, w io.Writer) error {
	bw := &errWriter{w: w}
	bw.line("# LLM Router Benchmark Report")
	bw.line("")
	bw.line("## Strategy comparison")
	bw.line("")
	bw.line("| Strategy | Requests | Errors | Hit rate | KV cached | TTFT p50 | TTFT p95 | TTFT p99 | Total p50 | Total p95 | RPS |")
	bw.line("| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |")
	for _, s := range summaries {
		cacheRate := "-"
		if s.PromptTokens > 0 {
			cacheRate = fmt.Sprintf("%.2f%%", s.CacheTokenRate*100)
		}
		bw.linef("| %s | %d | %d | %.2f%% | %s | %s | %s | %s | %s | %s | %.2f |",
			s.Strategy,
			s.Requests,
			s.Errors,
			s.HitRate*100,
			cacheRate,
			fmtDur(s.TTFT.P50),
			fmtDur(s.TTFT.P95),
			fmtDur(s.TTFT.P99),
			fmtDur(s.Total.P50),
			fmtDur(s.Total.P95),
			s.ThroughputRPS,
		)
	}
	bw.line("")
	bw.line("## Backend distribution")
	bw.line("")
	for _, s := range summaries {
		bw.linef("### %s", s.Strategy)
		bw.line("")
		ids := make([]string, 0, len(s.BackendCounts))
		for id := range s.BackendCounts {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			bw.linef("- %s: %d", id, s.BackendCounts[id])
		}
		bw.line("")
	}

	// Decision breakdown is only meaningful for prefix-aware (or other
	// strategies that produce non-Neutral classes); skip the section
	// entirely when every strategy is fully Neutral.
	if anyPrefixDecisions(summaries) {
		bw.line("## Routing decision breakdown")
		bw.line("")
		bw.line("| Strategy | Pinned (longest-prefix) | Spilled (next-best) | Fallback | Neutral | Upstream KV hit reqs |")
		bw.line("| --- | ---: | ---: | ---: | ---: | ---: |")
		for _, s := range summaries {
			bw.linef("| %s | %d | %d | %d | %d | %d / %d |",
				s.Strategy,
				s.PinnedRequests,
				s.SpilledRequests,
				s.FallbackRequests,
				s.NeutralRequests,
				s.UpstreamHits,
				s.Successful,
			)
		}
		bw.line("")
	}
	bw.line("## Notes")
	bw.line("")
	bw.line("- Hit rate is the fraction of successful requests for which the router")
	bw.line("  reported a prefix-driven decision (longest-prefix or spilled-from-saturated).")
	bw.line("  Non-prefix strategies always report 0% by construction.")
	bw.line("- KV cached is the upstream-reported cached_tokens / prompt_tokens, summed")
	bw.line("  across requests. \"-\" means the upstream did not return usage data (e.g.")
	bw.line("  streaming SSE responses, or a fake backend).")
	bw.line("- TTFT is wall-clock from request start to first received byte at the")
	bw.line("  replayer; it includes router overhead.")
	bw.line("- RPS is computed as successful requests divided by the wall-clock window")
	bw.line("  between the first started and last completed request.")
	return bw.err
}

// anyPrefixDecisions reports whether any summary observed a non-Neutral
// routing decision class. Used to gate the decision-breakdown section so
// reports that compare only oblivious strategies stay terse.
func anyPrefixDecisions(summaries []Summary) bool {
	for _, s := range summaries {
		if s.PinnedRequests+s.SpilledRequests+s.FallbackRequests > 0 {
			return true
		}
	}
	return false
}

// errWriter is a tiny adapter that captures the first write error so loops can
// emit many lines without a noisy error check on every call.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) line(s string) {
	if e.err != nil {
		return
	}
	_, e.err = fmt.Fprintln(e.w, s)
}

func (e *errWriter) linef(format string, args ...any) {
	if e.err != nil {
		return
	}
	_, e.err = fmt.Fprintf(e.w, format+"\n", args...)
}

// fmtDur formats a duration with a suitable unit for short benchmarks. Returns
// "-" if d is zero (i.e. no samples).
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
		return fmt.Sprintf("%.2fms", float64(d.Nanoseconds())/1e6)
	}
	return fmt.Sprintf("%.2fs", d.Seconds())
}
