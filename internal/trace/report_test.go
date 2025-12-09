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
	// Hits: 2x longest-prefix + 1x spilled-from-saturated = 3.
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
