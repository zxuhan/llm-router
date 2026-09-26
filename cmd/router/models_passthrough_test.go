package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zxuhan/llm-router/internal/backend"
)

// mustBackend builds a LlamaCpp backend for a stub upstream. threshold > 0
// configures the circuit breaker; cooldown keeps it open for the test run.
func mustBackend(t *testing.T, id, url string, threshold int, cooldown time.Duration) *backend.LlamaCpp {
	t.Helper()
	opts := backend.LlamaCppOptions{ID: id, URL: url}
	if threshold > 0 {
		opts.CircuitBreakerThreshold = threshold
		opts.CircuitBreakerCooldown = cooldown
	}
	b, err := backend.NewLlamaCpp(opts)
	if err != nil {
		t.Fatalf("new backend %s: %v", id, err)
	}
	return b
}

func TestModelPassthrough_ServesFirstWorker(t *testing.T) {
	var gotPath string
	var hits int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m0"}]}`))
	}))
	defer upstream.Close()

	h := newModelPassthrough([]backend.Backend{mustBackend(t, "w0", upstream.URL, 0, 0)}, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/models" {
		t.Errorf("upstream path = %q, want /v1/models", gotPath)
	}
	if atomic.LoadInt64(&hits) != 1 {
		t.Errorf("upstream hits = %d, want 1", hits)
	}
	if body := rec.Body.String(); body != `{"object":"list","data":[{"id":"m0"}]}` {
		t.Errorf("body = %q, want upstream body verbatim", body)
	}
}

func TestModelPassthrough_SkipsUnhealthyWorker(t *testing.T) {
	var secondHits int64
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer dead.Close()
	alive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&secondHits, 1)
		_, _ = w.Write([]byte("ok"))
	}))
	defer alive.Close()

	w0 := mustBackend(t, "w0", dead.URL, 1, time.Hour)
	w1 := mustBackend(t, "w1", alive.URL, 0, 0)

	// Trip w0's breaker with one 5xx.
	if resp, err := w0.Do(context.Background(), backend.Request{Method: http.MethodGet, Path: "/v1/models"}); err == nil {
		_ = resp.Body.Close()
	}
	if w0.Healthy() {
		t.Fatal("expected w0 breaker to be open after a 5xx")
	}

	h := newModelPassthrough([]backend.Backend{w0, w1}, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if atomic.LoadInt64(&secondHits) != 1 {
		t.Errorf("w1 hits = %d, want 1", secondHits)
	}
}

func TestModelPassthrough_AllUnhealthyIs503(t *testing.T) {
	// Port 1: nothing is listening, so Do fails at the transport and opens the breaker.
	b := mustBackend(t, "w0", "http://127.0.0.1:1", 1, time.Hour)
	if _, err := b.Do(context.Background(), backend.Request{Method: http.MethodGet, Path: "/v1/models"}); err == nil {
		t.Fatal("expected transport failure")
	}
	if b.Healthy() {
		t.Fatal("expected breaker to be open")
	}

	h := newModelPassthrough([]backend.Backend{b}, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}
