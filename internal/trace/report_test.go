package trace

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func makeResult(backend, reason string, ttft, total time.Duration) Result {
	now := time.Now()
	return Result{
		BackendID:   backend,
		Reason:      reason,
		StatusCode:  200,
		StartedAt:   now,
		CompletedAt: now.Add(total),
		TTFT:        ttft,
		Total:       total,
		BytesIn:     128,
	}
}

func TestSummarise_EmptyResults(t *testing.T) {
	s := Summarise("any", nil)
	if s.Requests != 0 || s.HitRate != 0 || s.ThroughputRPS != 0 {
		t.Errorf("empty summary not zeroed: %+v", s)
	}
}

func TestSummarise_CacheTokenRate(t *testing.T) {
	now := time.Now()
	results := []Result{
		{StatusCode: 200, BackendID: "a", StartedAt: now, CompletedAt: now.Add(time.Millisecond), TTFT: time.Millisecond, Total: time.Millisecond,
			PromptTokens: 100, CachedTokens: 80},
		{StatusCode: 200, BackendID: "a", StartedAt: now, CompletedAt: now.Add(time.Millisecond), TTFT: time.Millisecond, Total: time.Millisecond,
			PromptTokens: 200, CachedTokens: 100},
	}
	s := Summarise("any", results)
	if s.PromptTokens != 300 || s.CachedTokens != 180 {
		t.Errorf("PromptTokens=%d CachedTokens=%d", s.PromptTokens, s.CachedTokens)
	}
	if s.CacheTokenRate < 0.59 || s.CacheTokenRate > 0.61 {
		t.Errorf("CacheTokenRate = %v, want ~0.6", s.CacheTokenRate)
	}
}

func TestWriteMarkdown_ShowsCacheRateWhenAvailable(t *testing.T) {
	now := time.Now()
	summaries := []Summary{
		Summarise("real", []Result{
			{StatusCode: 200, BackendID: "a", Reason: "longest-prefix", StartedAt: now,
				CompletedAt: now.Add(time.Millisecond), TTFT: time.Millisecond, Total: time.Millisecond,
				PromptTokens: 100, CachedTokens: 80},
		}),
	}
	var buf bytes.Buffer
	if err := WriteMarkdown(summaries, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "80.00%") {
		t.Errorf("expected cache rate 80%% in markdown, got:\n%s", buf.String())
	}
}

func TestSummarise_HitRateAndDistribution(t *testing.T) {
	results := []Result{
		makeResult("a", "longest-prefix", 1*time.Millisecond, 10*time.Millisecond),
		makeResult("a", "longest-prefix", 2*time.Millisecond, 12*time.Millisecond),
		makeResult("b", "fallback-least-loaded", 3*time.Millisecond, 14*time.Millisecond),
		makeResult("b", "spilled-from-saturated", 4*time.Millisecond, 16*time.Millisecond),
		{Err: "boom", BackendID: "a"},
	}
	s := Summarise("prefixaware", results)
	if s.Requests != 5 {
		t.Errorf("Requests = %d", s.Requests)
	}
	if s.Errors != 1 || s.Successful != 4 {
		t.Errorf("Errors=%d Successful=%d", s.Errors, s.Successful)
	}
	// Decision-class partition: 2 pinned + 1 spilled + 1 fallback = 4 successful.
	if s.PinnedRequests != 2 || s.SpilledRequests != 1 || s.FallbackRequests != 1 {
		t.Errorf("decision classes: pinned=%d spilled=%d fallback=%d",
			s.PinnedRequests, s.SpilledRequests, s.FallbackRequests)
	}
	if s.NeutralRequests != 0 {
		t.Errorf("NeutralRequests = %d, want 0 for prefix-aware traffic", s.NeutralRequests)
	}
	// HitRequests rolls up pinned + spilled.
	if s.HitRequests != 3 {
		t.Errorf("HitRequests = %d", s.HitRequests)
	}
	if s.HitRate < 0.7 || s.HitRate > 0.8 {
		t.Errorf("HitRate = %v, want ~0.75", s.HitRate)
	}
	if s.BackendCounts["a"] != 2 || s.BackendCounts["b"] != 2 {
		t.Errorf("BackendCounts = %v", s.BackendCounts)
	}
	if s.TTFT.P50 == 0 || s.Total.P50 == 0 {
		t.Errorf("percentiles not computed: %+v", s)
	}
}

func TestSummarise_NonOK_StatusCounted_AsError(t *testing.T) {
	// A 4xx or 5xx upstream response must NOT be folded into the success
	// percentiles. Catches the cloud-bench regression where a misconfigured
	// vLLM worker returned 405 to every request and the bench reported its
	// ~1ms time-to-error as "TTFT p50 282µs".
	now := time.Now()
	results := []Result{
		// healthy
		{StatusCode: 200, BackendID: "w1", Reason: "longest-prefix", StartedAt: now,
			CompletedAt: now.Add(50 * time.Millisecond), TTFT: 50 * time.Millisecond, Total: 50 * time.Millisecond},
		// dead worker rejecting fast
		{StatusCode: 405, BackendID: "w0", Reason: "longest-prefix", StartedAt: now,
			CompletedAt: now.Add(time.Millisecond), TTFT: 200 * time.Microsecond, Total: 200 * time.Microsecond},
		{StatusCode: 502, BackendID: "w0", Reason: "longest-prefix", StartedAt: now,
			CompletedAt: now.Add(time.Millisecond), TTFT: 300 * time.Microsecond, Total: 300 * time.Microsecond},
	}
	s := Summarise("prefixaware", results)
	if s.Errors != 2 || s.Successful != 1 {
		t.Errorf("Errors=%d Successful=%d, want Errors=2 Successful=1", s.Errors, s.Successful)
	}
	if s.TTFT.P50 < 40*time.Millisecond {
		t.Errorf("TTFT.P50 = %v; errors must not pull p50 below the one healthy 50ms request", s.TTFT.P50)
	}
}

func TestSummarise_NeutralStrategies(t *testing.T) {
	// A non-prefix router (round-robin) should produce 100% Neutral and 0
	// PinnedRequests/SpilledRequests/FallbackRequests. The sum of decision
	// classes must equal Successful for any strategy.
	results := []Result{
		makeResult("a", "round-robin", 1*time.Millisecond, 5*time.Millisecond),
		makeResult("b", "round-robin", 1*time.Millisecond, 5*time.Millisecond),
		makeResult("c", "round-robin", 1*time.Millisecond, 5*time.Millisecond),
	}
	s := Summarise("roundrobin", results)
	if s.NeutralRequests != 3 {
		t.Errorf("NeutralRequests = %d, want 3", s.NeutralRequests)
	}
	if s.PinnedRequests+s.SpilledRequests+s.FallbackRequests != 0 {
		t.Errorf("non-Neutral classes should be zero for round-robin, got %+v", s)
	}
	got := s.PinnedRequests + s.SpilledRequests + s.FallbackRequests + s.NeutralRequests
	if got != s.Successful {
		t.Errorf("classes sum to %d, want %d (Successful)", got, s.Successful)
	}
}

func TestSummarise_UpstreamHitRate(t *testing.T) {
	now := time.Now()
	results := []Result{
		// Two upstream hits (cached_tokens > 0), one upstream miss.
		{StatusCode: 200, BackendID: "a", Reason: "longest-prefix",
			StartedAt: now, CompletedAt: now.Add(time.Millisecond),
			TTFT: time.Millisecond, Total: time.Millisecond,
			PromptTokens: 100, CachedTokens: 80},
		{StatusCode: 200, BackendID: "a", Reason: "longest-prefix",
			StartedAt: now, CompletedAt: now.Add(time.Millisecond),
			TTFT: time.Millisecond, Total: time.Millisecond,
			PromptTokens: 100, CachedTokens: 50},
		{StatusCode: 200, BackendID: "a", Reason: "fallback-least-loaded",
			StartedAt: now, CompletedAt: now.Add(time.Millisecond),
			TTFT: time.Millisecond, Total: time.Millisecond,
			PromptTokens: 100, CachedTokens: 0},
	}
	s := Summarise("prefixaware", results)
	if s.UpstreamHits != 2 {
		t.Errorf("UpstreamHits = %d, want 2", s.UpstreamHits)
	}
	if s.UpstreamHitRate < 0.66 || s.UpstreamHitRate > 0.67 {
		t.Errorf("UpstreamHitRate = %v, want ~0.667", s.UpstreamHitRate)
	}
}

func TestWriteMarkdown_DecisionBreakdownAppearsForPrefixStrategies(t *testing.T) {
	now := time.Now()
	mk := func(reason string) Result {
		return Result{StatusCode: 200, BackendID: "a", Reason: reason, StartedAt: now,
			CompletedAt: now.Add(time.Millisecond),
			TTFT:        time.Millisecond, Total: time.Millisecond}
	}
	summaries := []Summary{
		Summarise("prefixaware", []Result{mk("longest-prefix"), mk("spilled-from-saturated"), mk("fallback-least-loaded")}),
	}
	var buf bytes.Buffer
	if err := WriteMarkdown(summaries, &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "Routing decision breakdown") {
		t.Errorf("expected breakdown section, got:\n%s", out)
	}
	if !strings.Contains(out, "| prefixaware | 1 | 1 | 1 |") {
		t.Errorf("expected breakdown row '| prefixaware | 1 | 1 | 1 |', got:\n%s", out)
	}
}

func TestWriteMarkdown_DecisionBreakdownSkippedForNeutralOnly(t *testing.T) {
	summaries := []Summary{
		Summarise("roundrobin", []Result{makeResult("a", "round-robin", time.Millisecond, time.Millisecond)}),
	}
	var buf bytes.Buffer
	if err := WriteMarkdown(summaries, &buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "Routing decision breakdown") {
		t.Errorf("breakdown should be skipped when every strategy is Neutral")
	}
}

func TestComputePercentiles_SortsAndPicks(t *testing.T) {
	samples := []time.Duration{
		10 * time.Millisecond,
		1 * time.Millisecond,
		3 * time.Millisecond,
		7 * time.Millisecond,
		2 * time.Millisecond,
	}
	p := computePercentiles(samples)
	if p.P50 != 3*time.Millisecond {
		t.Errorf("P50 = %v", p.P50)
	}
	if p.P95 != 10*time.Millisecond {
		t.Errorf("P95 = %v", p.P95)
	}
	if p.Mean == 0 {
		t.Errorf("Mean = 0")
	}
}

func TestComputePercentiles_EmptyReturnsZero(t *testing.T) {
	p := computePercentiles(nil)
	if p.P50 != 0 || p.Mean != 0 {
		t.Errorf("expected zero, got %+v", p)
	}
}

func TestWriteJSON_Roundtrip(t *testing.T) {
	summaries := []Summary{
		Summarise("rr", []Result{makeResult("a", "round-robin", 1*time.Millisecond, 5*time.Millisecond)}),
		Summarise("pa", []Result{makeResult("a", "longest-prefix", 1*time.Millisecond, 5*time.Millisecond)}),
	}
	var buf bytes.Buffer
	if err := WriteJSON(summaries, &buf); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["summaries"]; !ok {
		t.Errorf("output missing summaries key")
	}
}

func TestWriteMarkdown_ContainsTablesAndStrategies(t *testing.T) {
	summaries := []Summary{
		Summarise("roundrobin", []Result{
			makeResult("a", "round-robin", 1*time.Millisecond, 10*time.Millisecond),
			makeResult("b", "round-robin", 2*time.Millisecond, 12*time.Millisecond),
		}),
		Summarise("prefixaware", []Result{
			makeResult("a", "longest-prefix", 1*time.Millisecond, 9*time.Millisecond),
			makeResult("a", "longest-prefix", 1*time.Millisecond, 9*time.Millisecond),
		}),
	}
	var buf bytes.Buffer
	if err := WriteMarkdown(summaries, &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, w := range []string{
		"# LLM Router Benchmark Report",
		"## Strategy comparison",
		"| Strategy |",
		"roundrobin",
		"prefixaware",
		"### roundrobin",
		"### prefixaware",
		"## Notes",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("markdown missing %q", w)
		}
	}
}

func TestFmtDur_Branches(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "-"},
		{500 * time.Nanosecond, "500ns"},
		{500 * time.Microsecond, "500.0µs"},
		{500 * time.Millisecond, "500.00ms"},
		{2 * time.Second, "2.00s"},
	}
	for _, c := range cases {
		got := fmtDur(c.in)
		if got != c.want {
			t.Errorf("fmtDur(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
