// Package integration holds tests that exercise the full router stack against
// multiple fake backends. The package itself is empty in production builds;
// only its _test files compile.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zxuhan/llm-router/internal/backend"
	"github.com/zxuhan/llm-router/internal/proxy"
	"github.com/zxuhan/llm-router/internal/router"
	"github.com/zxuhan/llm-router/internal/trace"
)

// makeStreamingHandler returns an http.HandlerFunc that emits a few SSE
// chunks. Used to give every fake backend a llama.cpp-shaped response.
func makeStreamingHandler(content string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", content)
		if flusher != nil {
			flusher.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
}

// fixture builds three fake backends behind the real proxy + the requested
// router strategy.
func fixture(t *testing.T, build func([]backend.Backend) router.Router) (string, []backend.Backend, *httptest.Server) {
	t.Helper()
	servers := []*backend.FakeServer{
		backend.NewFakeServer(backend.FakeServerOptions{Handler: makeStreamingHandler("from a")}),
		backend.NewFakeServer(backend.FakeServerOptions{Handler: makeStreamingHandler("from b")}),
		backend.NewFakeServer(backend.FakeServerOptions{Handler: makeStreamingHandler("from c")}),
	}
	t.Cleanup(func() {
		for _, s := range servers {
			s.Close()
		}
	})
	backends := make([]backend.Backend, len(servers))
	for i, s := range servers {
		b, err := s.Backend(fmt.Sprintf("w%d", i), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		backends[i] = b
	}
	r := build(backends)
	h, err := proxy.New(proxy.Options{
		Router: r,
		Logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	frontend := httptest.NewServer(h)
	t.Cleanup(frontend.Close)
	return frontend.URL + "/v1/chat/completions", backends, frontend
}

// runReplay generates a small synthetic trace and replays it against the
// router endpoint, returning the per-result list.
func runReplay(t *testing.T, endpoint string, seed int64) []trace.Result {
	t.Helper()
	tr := trace.Generate(trace.Options{
		Seed:                 seed,
		Sessions:             6,
		TurnsPerSession:      4,
		SharedSystemLen:      128,
		UserTurnLen:          32,
		CodeContextLen:       0,
		CodeSessionShare:     0,
		ToolLoopProb:         0,
		SessionStartJitterMs: 50,
		TurnGapMs:            10,
	})
	r := &trace.Replayer{Endpoint: endpoint}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := r.Replay(ctx, tr)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestEndToEnd_PrefixAwareBeatsRoundRobinOnHitRate(t *testing.T) {
	endpointRR, _, _ := fixture(t, func(b []backend.Backend) router.Router {
		return router.NewRoundRobin(b)
	})
	resRR := runReplay(t, endpointRR, 1)

	endpointPA, _, _ := fixture(t, func(b []backend.Backend) router.Router {
		return router.NewPrefixAware(b, router.PrefixAwareOptions{
			Chunker:        router.NewChunker(16),
			MinMatchChunks: 1,
		})
	})
	resPA := runReplay(t, endpointPA, 1)

	hitRate := func(rs []trace.Result) float64 {
		hits, ok := 0, 0
		for _, r := range rs {
			if r.Err != "" {
				continue
			}
			ok++
			if r.Reason == "longest-prefix" || r.Reason == "spilled-from-saturated" {
				hits++
			}
		}
		if ok == 0 {
			return 0
		}
		return float64(hits) / float64(ok)
	}

	rr := hitRate(resRR)
	pa := hitRate(resPA)
	t.Logf("hit rate roundrobin=%.2f%% prefixaware=%.2f%%", rr*100, pa*100)
	if rr != 0 {
		t.Errorf("round-robin should never report a prefix hit; got %v", rr)
	}
	if pa <= rr+0.2 {
		t.Errorf("prefixaware hit rate %.2f%% should comfortably beat roundrobin %.2f%%", pa*100, rr*100)
	}
}

func TestEndToEnd_HeadersForwardRoutingDecision(t *testing.T) {
	endpoint, _, _ := fixture(t, func(b []backend.Backend) router.Router {
		return router.NewPrefixAware(b, router.PrefixAwareOptions{
			Chunker:        router.NewChunker(8),
			MinMatchChunks: 1,
		})
	})

	body := []byte(`{"messages":[{"role":"user","content":"hello world"}]}`)
	resp, err := http.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("X-Router-Backend") == "" {
		t.Errorf("missing X-Router-Backend header")
	}
	if resp.Header.Get("X-Router-Reason") == "" {
		t.Errorf("missing X-Router-Reason header")
	}
	if resp.Header.Get("X-Request-ID") == "" {
		t.Errorf("missing X-Request-ID header")
	}
}

func TestEndToEnd_LeastLoadedReactsToBackendLoad(t *testing.T) {
	endpoint, backends, _ := fixture(t, func(b []backend.Backend) router.Router {
		return router.NewLeastLoaded(b)
	})
	// Pre-load backends 0 and 1, leave 2 idle.
	backends[0].Acquire()
	backends[0].Acquire()
	backends[1].Acquire()
	t.Cleanup(func() {
		backends[0].Release()
		backends[0].Release()
		backends[1].Release()
	})

	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got := resp.Header.Get("X-Router-Backend")
	if got != "w2" {
		t.Errorf("LeastLoaded routed to %q, want w2 (the idle one)", got)
	}
}

func TestEndToEnd_RoundRobinRotates(t *testing.T) {
	endpoint, _, _ := fixture(t, func(b []backend.Backend) router.Router {
		return router.NewRoundRobin(b)
	})
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	got := []string{}
	for i := 0; i < 6; i++ {
		resp, err := http.Post(endpoint, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, resp.Header.Get("X-Router-Backend"))
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	want := []string{"w0", "w1", "w2", "w0", "w1", "w2"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d: got %q want %q (full sequence: %v)", i, got[i], want[i], got)
		}
	}
}

func TestEndToEnd_ConcurrentClientsNoDataRace(t *testing.T) {
	endpoint, _, _ := fixture(t, func(b []backend.Backend) router.Router {
		return router.NewPrefixAware(b, router.PrefixAwareOptions{
			Chunker:        router.NewChunker(8),
			MinMatchChunks: 1,
		})
	})

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				body, _ := json.Marshal(map[string]any{
					"messages": []map[string]string{
						{"role": "system", "content": strings.Repeat("S", 64)},
						{"role": "user", "content": fmt.Sprintf("g=%d j=%d", i, j)},
					},
				})
				resp, err := http.Post(endpoint, "application/json", bytes.NewReader(body))
				if err != nil {
					t.Errorf("post: %v", err)
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}(i)
	}
	wg.Wait()
}
