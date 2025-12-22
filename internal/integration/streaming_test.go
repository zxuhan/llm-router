package integration

import (
	"bufio"
	"bytes"
	"context"
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
)

// TestStreaming_ChunksDeliveredIncrementally drives a backend that emits SSE
// chunks with deliberate gaps and verifies that each chunk reaches the client
// before the next one is produced (i.e. no buffering at the proxy).
func TestStreaming_ChunksDeliveredIncrementally(t *testing.T) {
	const (
		nChunks    = 5
		gapBetween = 25 * time.Millisecond
	)

	fake := backend.NewFakeServer(backend.FakeServerOptions{
		Chunks: func() []string {
			cs := make([]string, nChunks)
			for i := range cs {
				cs[i] = fmt.Sprintf(`{"choices":[{"delta":{"content":"c%d"}}]}`, i)
			}
			return cs
		}(),
		InterChunk: gapBetween,
	})
	defer fake.Close()

	bb, err := fake.Backend("a", 0)
	if err != nil {
		t.Fatal(err)
	}
	h, err := proxy.New(proxy.Options{
		Router: router.NewRoundRobin([]backend.Backend{bb}),
		Logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	body := strings.NewReader(`{"messages":[{"role":"user","content":"hi"}],"stream":true}`)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
		srv.URL+"/v1/chat/completions", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Errorf("Content-Type = %q", resp.Header.Get("Content-Type"))
	}

	// Read line-by-line, recording time per chunk.
	type stamped struct {
		t    time.Time
		line string
	}
	var got []stamped
	sc := bufio.NewScanner(resp.Body)
	start := time.Now()
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		got = append(got, stamped{time.Now(), line})
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scanner: %v", err)
	}

	// We expect nChunks+1 data lines (chunks plus [DONE]).
	if len(got) != nChunks+1 {
		t.Fatalf("got %d data lines, want %d", len(got), nChunks+1)
	}
	// Verify the gaps between *content* chunks (excluding [DONE], which the
	// fake server emits immediately after the last content chunk with no
	// inter-chunk gap). Each successive content chunk should arrive at least
	// ~gap later than the previous one. We use 60% of the configured gap as a
	// lower bound to absorb scheduler jitter on busy CI machines.
	minSep := gapBetween * 6 / 10
	for i := 1; i < nChunks; i++ {
		diff := got[i].t.Sub(got[i-1].t)
		if diff < minSep {
			t.Errorf("chunk %d arrived %v after chunk %d, want >= %v (suggests buffering)",
				i, diff, i-1, minSep)
		}
	}
	t.Logf("first chunk after %v; total %v; %d chunks observed",
		got[0].t.Sub(start), got[len(got)-1].t.Sub(start), len(got))
}

// TestStreaming_NonStreamingResponseStillProxies verifies that non-stream
// (single JSON) responses pass through unchanged.
func TestStreaming_NonStreamingResponseStillProxies(t *testing.T) {
	fake := backend.NewFakeServer(backend.FakeServerOptions{
		JSON: map[string]any{
			"id":     "x",
			"object": "chat.completion",
			"choices": []map[string]any{
				{"index": 0, "message": map[string]any{"role": "assistant", "content": "hi"}},
			},
		},
	})
	defer fake.Close()
	bb, _ := fake.Backend("a", 0)
	h, _ := proxy.New(proxy.Options{
		Router: router.NewRoundRobin([]backend.Backend{bb}),
		Logger: log.New(io.Discard, "", 0),
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q", resp.Header.Get("Content-Type"))
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte(`"hi"`)) {
		t.Errorf("body did not pass through verbatim: %s", body)
	}
}

// TestStreaming_ClientCancelEndsUpstream verifies that when the client
// disconnects, the upstream context is cancelled and resources are freed.
func TestStreaming_ClientCancelEndsUpstream(t *testing.T) {
	upstreamSeen := make(chan struct{}, 1)
	upstreamCancelled := make(chan struct{}, 1)

	fake := backend.NewFakeServer(backend.FakeServerOptions{
		Handler: func(w http.ResponseWriter, r *http.Request) {
			upstreamSeen <- struct{}{}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			fmt.Fprint(w, "data: tick\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			// Block on either the upstream request context being cancelled
			// (the desired behaviour) or a 2s safety bound.
			select {
			case <-r.Context().Done():
				upstreamCancelled <- struct{}{}
			case <-time.After(2 * time.Second):
			}
		},
	})
	defer fake.Close()
	bb, _ := fake.Backend("a", 0)
	h, _ := proxy.New(proxy.Options{
		Router: router.NewRoundRobin([]backend.Backend{bb}),
		Logger: log.New(io.Discard, "", 0),
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")

	var clientWg sync.WaitGroup
	clientWg.Add(1)
	go func() {
		defer clientWg.Done()
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			// Read the first chunk then close prematurely.
			buf := make([]byte, 32)
			_, _ = resp.Body.Read(buf)
			resp.Body.Close()
		}
	}()

	<-upstreamSeen
	// Cancel the client's request context, which closes the proxy's upstream
	// connection and propagates the cancellation to the fake server.
	cancel()
	clientWg.Wait()

	select {
	case <-upstreamCancelled:
	case <-time.After(2 * time.Second):
		t.Errorf("upstream did not observe cancellation within timeout")
	}
}
