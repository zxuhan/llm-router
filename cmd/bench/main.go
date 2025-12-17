// Command bench runs every routing strategy against the same synthetic trace
// using in-process fake backends, then writes a comparison report.
//
// Because it uses fake backends, the absolute numbers are not directly
// comparable to a production deployment with real LLM workers. The point of
// this tool is to compare *strategies* on identical traffic - that comparison
// is what the project thesis stands on.
//
// Usage:
//
//	bench --out docs/results.md
//	bench --json results.json --markdown results.md --seed 42
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/xzhou/llm-router/internal/backend"
	"github.com/xzhou/llm-router/internal/proxy"
	"github.com/xzhou/llm-router/internal/router"
	"github.com/xzhou/llm-router/internal/trace"
)

func main() {
	if err := run(os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		os.Exit(1)
	}
}

func run(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	fs.SetOutput(stderr)

	mdPath := fs.String("markdown", "", "write Markdown report to this path")
	jsonPath := fs.String("json", "", "write JSON summary to this path")
	rawDir := fs.String("raw-results", "", "directory in which to write per-strategy JSONL of raw per-request results (one line per request)")
	seed := fs.Int64("seed", 42, "trace seed")
	sessions := fs.Int("sessions", 12, "trace sessions")
	turns := fs.Int("turns", 6, "turns per session")
	sysLen := fs.Int("system-len", 768, "system prompt length")
	codeLen := fs.Int("code-context-len", 1536, "code-context length (0 disables)")
	codeShare := fs.Float64("code-share", 0.4, "fraction of sessions using code-edit pattern")
	chunkSize := fs.Int("chunk-size", 32, "prefix-aware chunk size")
	minMatch := fs.Int("min-match-chunks", 2, "prefix-aware match threshold")
	saturate := fs.Int("saturation-inflight", 8, "prefix-aware safety-valve threshold")
	backendDelay := fs.Duration("backend-ttft", 8*time.Millisecond, "simulated upstream TTFT (fake-backend mode only)")
	backendCount := fs.Int("backends", 3, "number of fake backends (fake-backend mode only)")
	realBackends := fs.String("real", "", "comma-separated real backend URLs; when set, runs against real upstreams instead of fakes")
	maxTokens := fs.Int("max-tokens", 32, "max_tokens to request from real upstreams (keeps the run short)")
	onlyStrategy := fs.String("strategy", "", "only run this strategy (one of: roundrobin, random, leastloaded, prefixaware); empty runs all four")

	if err := fs.Parse(args); err != nil {
		return err
	}

	tr := trace.Generate(trace.Options{
		Seed:                 *seed,
		Sessions:             *sessions,
		TurnsPerSession:      *turns,
		SharedSystemLen:      *sysLen,
		UserTurnLen:          80,
		CodeContextLen:       *codeLen,
		CodeSessionShare:     *codeShare,
		ToolLoopProb:         0.3,
		SessionStartJitterMs: 200,
		TurnGapMs:            40,
	})
	// In real-backend mode we want non-streaming responses so we can parse
	// the OpenAI-style `usage.prompt_tokens_details.cached_tokens` field.
	if *realBackends != "" {
		for i := range tr.Requests {
			tr.Requests[i].Body.Stream = false
			tr.Requests[i].Body.MaxTokens = *maxTokens
		}
	}
	shape := trace.Shape(tr)
	_, _ = fmt.Fprintf(stderr, "trace: %d requests, %d sessions, mean prompt %d chars, max delay %v\n",
		shape.Requests, shape.Sessions, shape.MeanContentChars, shape.MaxDelay)

	type strategyEntry struct {
		name string
		make func([]backend.Backend) router.Router
	}
	strategies := []strategyEntry{
		{"roundrobin", func(b []backend.Backend) router.Router { return router.NewRoundRobin(b) }},
		{"random", func(b []backend.Backend) router.Router { return router.NewRandom(b) }},
		{"leastloaded", func(b []backend.Backend) router.Router { return router.NewLeastLoaded(b) }},
		{"prefixaware", func(b []backend.Backend) router.Router {
			return router.NewPrefixAware(b, router.PrefixAwareOptions{
				Chunker:            router.NewChunker(*chunkSize),
				MinMatchChunks:     *minMatch,
				SaturationInflight: *saturate,
			})
		}},
	}

	realURLs := splitCSV(*realBackends)
	summaries := make([]trace.Summary, 0, len(strategies))
	for _, st := range strategies {
		if *onlyStrategy != "" && st.name != *onlyStrategy {
			continue
		}
		_, _ = fmt.Fprintf(stderr, "running %s...\n", st.name)
		var results []trace.Result
		var err error
		if len(realURLs) > 0 {
			results, err = runStrategyReal(st.name, st.make, tr, realURLs)
		} else {
			results, err = runStrategy(st.name, st.make, tr, *backendCount, *backendDelay)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", st.name, err)
		}
		s := trace.Summarise(st.name, results)
		summaries = append(summaries, s)
		if *rawDir != "" {
			if err := writeResultsJSONL(*rawDir, st.name, results); err != nil {
				return fmt.Errorf("%s raw results: %w", st.name, err)
			}
		}
		_, _ = fmt.Fprintf(stderr, "  %s: hit=%5.2f%%  cache=%5.2f%%  ttft p50=%s p95=%s  rps=%.1f\n",
			st.name, s.HitRate*100, s.CacheTokenRate*100, fmtDur(s.TTFT.P50), fmtDur(s.TTFT.P95), s.ThroughputRPS)
	}

	if *jsonPath != "" {
		if err := writeFile(*jsonPath, func(w io.Writer) error { return trace.WriteJSON(summaries, w) }); err != nil {
			return err
		}
	}
	if *mdPath != "" {
		if err := writeFile(*mdPath, func(w io.Writer) error { return trace.WriteMarkdown(summaries, w) }); err != nil {
			return err
		}
	}
	return nil
}

func runStrategy(name string, build func([]backend.Backend) router.Router, tr trace.Trace, backendCount int, ttft time.Duration) ([]trace.Result, error) {
	// Build N fake backends with a small simulated TTFT. The handler is
	// shared (same content) so any strategy difference is in *who* gets the
	// request, not what the upstream returns.
	servers := make([]*backend.FakeServer, backendCount)
	backends := make([]backend.Backend, backendCount)
	for i := 0; i < backendCount; i++ {
		fs := backend.NewFakeServer(backend.FakeServerOptions{
			TTFT: ttft,
			Chunks: []string{
				`{"choices":[{"delta":{"content":"hello"}}]}`,
				`{"choices":[{"delta":{"content":" world"}}]}`,
			},
		})
		servers[i] = fs
		bb, err := fs.Backend(fmt.Sprintf("w%d", i), 1<<20)
		if err != nil {
			return nil, err
		}
		backends[i] = bb
	}
	defer closeAll(servers)

	r := build(backends)
	h, err := proxy.New(proxy.Options{
		Router: r,
		Logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		return nil, err
	}
	frontend := httptest.NewServer(h)
	defer frontend.Close()

	rep := &trace.Replayer{Endpoint: frontend.URL + "/v1/chat/completions"}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return rep.Replay(ctx, tr)
}

// runStrategyReal runs the same in-process pipeline (router + proxy) but with
// real LlamaCpp backends instead of fakes. Each strategy uses the same backend
// URL list; the router lives in this process via httptest, so there is no
// long-running OS-level router to coordinate.
func runStrategyReal(name string, build func([]backend.Backend) router.Router, tr trace.Trace, urls []string) ([]trace.Result, error) {
	backends := make([]backend.Backend, len(urls))
	for i, u := range urls {
		bb, err := backend.NewLlamaCpp(backend.LlamaCppOptions{
			ID:       fmt.Sprintf("w%d", i),
			URL:      u,
			KVBudget: 1 << 16,
			Timeout:  60 * time.Second,
		})
		if err != nil {
			return nil, err
		}
		backends[i] = bb
	}

	r := build(backends)
	h, err := proxy.New(proxy.Options{
		Router: r,
		Logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		return nil, err
	}
	frontend := httptest.NewServer(h)
	defer frontend.Close()

	rep := &trace.Replayer{Endpoint: frontend.URL + "/v1/chat/completions"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	return rep.Replay(ctx, tr)
}

// writeResultsJSONL writes one Result per line to <dir>/<strategy>.jsonl.
// The directory is created if missing.
func writeResultsJSONL(dir, strategy string, results []trace.Result) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := dir + "/" + strategy + ".jsonl"
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	for _, r := range results {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return nil
}

// splitCSV trims and skips empty entries from a comma-separated string.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func closeAll(srvs []*backend.FakeServer) {
	var wg sync.WaitGroup
	for _, s := range srvs {
		wg.Add(1)
		go func(s *backend.FakeServer) { defer wg.Done(); s.Close() }(s)
	}
	wg.Wait()
}

func writeFile(path string, fn func(io.Writer) error) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return fn(f)
}

func fmtDur(d time.Duration) string {
	if d == 0 {
		return "-"
	}
	if d < time.Millisecond {
		return fmt.Sprintf("%.0fµs", float64(d.Nanoseconds())/1e3)
	}
	if d < time.Second {
		return fmt.Sprintf("%.2fms", float64(d.Nanoseconds())/1e6)
	}
	return d.String()
}
