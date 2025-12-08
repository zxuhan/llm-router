// Package metrics is the central registry of router-level Prometheus metrics.
//
// The package exposes:
//
//   - A small struct of *prometheus.HistogramVec / *prometheus.CounterVec
//     instruments, owned by a private prometheus.Registry.
//   - A proxy.Recorder adapter that increments the instruments on every
//     proxied request.
//   - A custom Collector for prefix-tree stats so trees report their state on
//     /metrics scrape.
//   - An http.Handler exposing /metrics in Prometheus text format.
//
// Registries are intentionally non-global. Callers construct one with New()
// and embed it in the server. This makes tests trivially independent and
// avoids the usual gotchas of process-global state.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/xzhou/llm-router/internal/prefixtree"
	"github.com/xzhou/llm-router/internal/proxy"
)

// Registry bundles every metric the router emits.
type Registry struct {
	registry *prometheus.Registry

	requestsTotal     *prometheus.CounterVec
	bytesOutTotal     *prometheus.CounterVec
	requestDuration   *prometheus.HistogramVec
	timeToFirstByte   *prometheus.HistogramVec
	matchChunks       *prometheus.HistogramVec
	cacheHitRequests  *prometheus.CounterVec
}

// New returns a fresh Registry. It does not install Go process collectors;
// callers can add them by calling Registry.RegisterRuntime.
func New() *Registry {
	r := prometheus.NewRegistry()
	reg := &Registry{
		registry: r,
		requestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "router_requests_total",
			Help: "Total number of routed requests.",
		}, []string{"strategy", "backend", "reason", "status"}),
		bytesOutTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "router_response_bytes_total",
			Help: "Total bytes streamed back to clients.",
		}, []string{"strategy", "backend"}),
		requestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "router_request_duration_seconds",
			Help:    "End-to-end request duration in seconds.",
			Buckets: prometheus.ExponentialBuckets(0.005, 2, 12),
		}, []string{"strategy", "backend", "reason"}),
		timeToFirstByte: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "router_time_to_first_byte_seconds",
			Help:    "Time from request entry to first response byte.",
			Buckets: prometheus.ExponentialBuckets(0.001, 2, 14),
		}, []string{"strategy", "backend", "reason"}),
		matchChunks: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "router_prefix_match_chunks",
			Help:    "Distribution of prefix-tree chunks the chosen backend already held for each request.",
			Buckets: []float64{0, 1, 2, 4, 8, 16, 32, 64, 128, 256, 512, 1024, 2048},
		}, []string{"strategy", "backend"}),
		cacheHitRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "router_cache_hit_requests_total",
			Help: "Requests counted by hit category (hit, miss). A request is a hit when the routing decision matched at least one prefix chunk.",
		}, []string{"strategy", "backend", "category"}),
	}
	r.MustRegister(
		reg.requestsTotal,
		reg.bytesOutTotal,
		reg.requestDuration,
		reg.timeToFirstByte,
		reg.matchChunks,
		reg.cacheHitRequests,
	)
	return reg
}

// RegisterRuntime adds the standard Go process collectors. Call this in
// production; tests typically skip it to keep the scrape output minimal.
func (r *Registry) RegisterRuntime() {
	r.registry.MustRegister(
		prometheus.NewGoCollector(),
		prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}),
	)
}

// Recorder returns a proxy.Recorder that updates the registry's instruments.
func (r *Registry) Recorder() proxy.Recorder {
	return func(s proxy.RequestStats) {
		status := statusBucket(s.StatusCode)
		strategy := emptyAs(s.Strategy, "unknown")
		backend := emptyAs(s.BackendID, "none")
		reason := emptyAs(s.Reason, "n/a")

		r.requestsTotal.WithLabelValues(strategy, backend, reason, status).Inc()

		if s.BytesOut > 0 {
			r.bytesOutTotal.WithLabelValues(strategy, backend).Add(float64(s.BytesOut))
		}
		if s.Total > 0 {
			r.requestDuration.WithLabelValues(strategy, backend, reason).
				Observe(s.Total.Seconds())
		}
		if s.TTFT > 0 {
			r.timeToFirstByte.WithLabelValues(strategy, backend, reason).
				Observe(s.TTFT.Seconds())
		}
		if s.BackendID != "" {
			r.matchChunks.WithLabelValues(strategy, backend).
				Observe(float64(s.MatchChunks))
			category := "miss"
			if s.MatchChunks > 0 {
				category = "hit"
			}
			r.cacheHitRequests.WithLabelValues(strategy, backend, category).Inc()
		}
	}
}

// Handler returns the http.Handler exposing /metrics for this Registry.
func (r *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(r.registry, promhttp.HandlerOpts{})
}

// PrefixTreeProvider supplies live tree stats to RegisterPrefixTrees. Any
// router that owns per-backend trees can satisfy it; PrefixAware does.
type PrefixTreeProvider interface {
	TreeStats() map[string]prefixtree.Stats
}

// RegisterPrefixTrees adds a custom Collector that exposes one set of tree
// metrics per backend on every scrape, sourced from p. It is safe to call at
// most once per Registry.
func (r *Registry) RegisterPrefixTrees(p PrefixTreeProvider) {
	r.registry.MustRegister(newTreeCollector(p))
}

// statusBucket reduces a numeric status code to a stable label value. We do
// not use the literal numeric code because cardinality would explode if a
// misbehaving upstream rotated through 4xx/5xx values.
func statusBucket(code int) string {
	switch {
	case code == 0:
		return "no_response"
	case code < 200:
		return "1xx"
	case code < 300:
		return "2xx"
	case code < 400:
		return "3xx"
	case code < 500:
		return "4xx"
	default:
		return "5xx"
	}
}

func emptyAs(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// treeCollector implements prometheus.Collector by snapshotting tree stats
// from a PrefixTreeProvider on every Collect call.
type treeCollector struct {
	provider PrefixTreeProvider

	chunks      *prometheus.Desc
	terminals   *prometheus.Desc
	maxChunks   *prometheus.Desc
	insertsT    *prometheus.Desc
	queriesT    *prometheus.Desc
	evictionsT  *prometheus.Desc
}

func newTreeCollector(p PrefixTreeProvider) *treeCollector {
	return &treeCollector{
		provider:   p,
		chunks:     prometheus.NewDesc("router_prefix_tree_chunks", "Chunks currently held in each backend's prefix tree.", []string{"backend"}, nil),
		terminals:  prometheus.NewDesc("router_prefix_tree_terminals", "Live terminal nodes in each backend's prefix tree.", []string{"backend"}, nil),
		maxChunks:  prometheus.NewDesc("router_prefix_tree_max_chunks", "Configured chunk budget for each backend's tree (0 = unlimited).", []string{"backend"}, nil),
		insertsT:   prometheus.NewDesc("router_prefix_tree_inserts_total", "Cumulative inserts into each backend's tree.", []string{"backend"}, nil),
		queriesT:   prometheus.NewDesc("router_prefix_tree_queries_total", "Cumulative LongestMatch queries against each backend's tree.", []string{"backend"}, nil),
		evictionsT: prometheus.NewDesc("router_prefix_tree_evictions_total", "Cumulative evicted terminals from each backend's tree.", []string{"backend"}, nil),
	}
}

func (c *treeCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.chunks
	ch <- c.terminals
	ch <- c.maxChunks
	ch <- c.insertsT
	ch <- c.queriesT
	ch <- c.evictionsT
}

func (c *treeCollector) Collect(ch chan<- prometheus.Metric) {
	for backend, st := range c.provider.TreeStats() {
		ch <- prometheus.MustNewConstMetric(c.chunks, prometheus.GaugeValue, float64(st.Chunks), backend)
		ch <- prometheus.MustNewConstMetric(c.terminals, prometheus.GaugeValue, float64(st.Terminals), backend)
		ch <- prometheus.MustNewConstMetric(c.maxChunks, prometheus.GaugeValue, float64(st.MaxChunks), backend)
		ch <- prometheus.MustNewConstMetric(c.insertsT, prometheus.CounterValue, float64(st.Inserts), backend)
		ch <- prometheus.MustNewConstMetric(c.queriesT, prometheus.CounterValue, float64(st.Queries), backend)
		ch <- prometheus.MustNewConstMetric(c.evictionsT, prometheus.CounterValue, float64(st.Evicted), backend)
	}
}
