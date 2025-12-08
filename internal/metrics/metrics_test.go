package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xzhou/llm-router/internal/prefixtree"
	"github.com/xzhou/llm-router/internal/proxy"
)

func TestRegistry_RecorderProducesScrapeLines(t *testing.T) {
	r := New()
	rec := r.Recorder()

	rec(proxy.RequestStats{
		BackendID:   "a",
		Strategy:    "prefixaware",
		Reason:      "longest-prefix",
		MatchChunks: 5,
		StatusCode:  200,
		TTFT:        10 * time.Millisecond,
		Total:       50 * time.Millisecond,
		BytesOut:    1234,
	})
	rec(proxy.RequestStats{
		BackendID:  "b",
		Strategy:   "prefixaware",
		Reason:     "longest-prefix",
		StatusCode: 500,
	})
	// Failed-route call: no backend.
	rec(proxy.RequestStats{Strategy: "prefixaware", StatusCode: 503, Err: "no backends"})

	out := scrape(t, r)

	wants := []string{
		`router_requests_total{backend="a",reason="longest-prefix",status="2xx",strategy="prefixaware"} 1`,
		`router_requests_total{backend="b",reason="longest-prefix",status="5xx",strategy="prefixaware"} 1`,
		`router_requests_total{backend="none",reason="n/a",status="5xx",strategy="prefixaware"} 1`,
		`router_response_bytes_total{backend="a",strategy="prefixaware"} 1234`,
		`router_cache_hit_requests_total{backend="a",category="hit",strategy="prefixaware"} 1`,
		`router_cache_hit_requests_total{backend="b",category="miss",strategy="prefixaware"} 1`,
		`router_request_duration_seconds_count{backend="a",reason="longest-prefix",strategy="prefixaware"} 1`,
		`router_time_to_first_byte_seconds_count{backend="a",reason="longest-prefix",strategy="prefixaware"} 1`,
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("scrape missing %q\n--- scrape ---\n%s", w, out)
		}
	}
}

func TestRegistry_StatusBucketCoversAllRanges(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{0, "no_response"},
		{100, "1xx"},
		{200, "2xx"},
		{301, "3xx"},
		{418, "4xx"},
		{500, "5xx"},
		{502, "5xx"},
		{99, "1xx"},
	}
	for _, c := range cases {
		got := statusBucket(c.in)
		if got != c.want {
			t.Errorf("statusBucket(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

type stubProvider struct {
	stats map[string]prefixtree.Stats
}

func (s stubProvider) TreeStats() map[string]prefixtree.Stats { return s.stats }

func TestRegistry_TreeCollectorEmitsPerBackendMetrics(t *testing.T) {
	r := New()
	r.RegisterPrefixTrees(stubProvider{
		stats: map[string]prefixtree.Stats{
			"a": {Chunks: 100, Terminals: 7, MaxChunks: 1024, Inserts: 12, Queries: 33, Evicted: 1},
			"b": {Chunks: 0, Terminals: 0, MaxChunks: 1024, Inserts: 0, Queries: 0, Evicted: 0},
		},
	})

	out := scrape(t, r)
	for _, w := range []string{
		`router_prefix_tree_chunks{backend="a"} 100`,
		`router_prefix_tree_terminals{backend="a"} 7`,
		`router_prefix_tree_max_chunks{backend="a"} 1024`,
		`router_prefix_tree_inserts_total{backend="a"} 12`,
		`router_prefix_tree_queries_total{backend="a"} 33`,
		`router_prefix_tree_evictions_total{backend="a"} 1`,
		`router_prefix_tree_chunks{backend="b"} 0`,
	} {
		if !strings.Contains(out, w) {
			t.Errorf("scrape missing %q", w)
		}
	}
}

func TestRegistry_RegisterRuntimeAddsGoMetrics(t *testing.T) {
	r := New()
	r.RegisterRuntime()
	out := scrape(t, r)
	if !strings.Contains(out, "go_goroutines") {
		t.Errorf("expected go_goroutines after RegisterRuntime")
	}
}

func TestEmptyAs(t *testing.T) {
	if got := emptyAs("", "x"); got != "x" {
		t.Errorf("emptyAs(empty) = %q", got)
	}
	if got := emptyAs("y", "x"); got != "y" {
		t.Errorf("emptyAs(non-empty) = %q", got)
	}
}

// scrape runs an in-memory GET against the registry's /metrics handler.
func scrape(t *testing.T, r *Registry) string {
	t.Helper()
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}
