package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// LlamaCppOptions configures a LlamaCpp backend instance. Defaults are applied
// for any zero-valued fields.
type LlamaCppOptions struct {
	ID       string
	URL      string
	KVBudget int
	// Timeout is the upstream timeout for reading the response *headers* (TTFT
	// envelope). The body is streamed without a deadline so that long
	// generations are not cut off.
	Timeout time.Duration
	// HTTPClient lets tests inject a stubbed transport; nil uses a default
	// streaming-friendly client.
	HTTPClient *http.Client
	// CircuitBreakerThreshold is the number of consecutive failures that
	// trips the breaker. Defaults to 5 (clamped from non-positive values).
	CircuitBreakerThreshold int
	// CircuitBreakerCooldown is how long the breaker stays open after
	// tripping. Defaults to 30s.
	CircuitBreakerCooldown time.Duration
}

// LlamaCpp is a Backend implementation that talks to llama.cpp's built-in HTTP
// server (the OpenAI-compatible /v1/chat/completions endpoint). Other
// OpenAI-compatible servers (mlx-lm.server, vLLM, etc.) work with the same
// client because the wire format matches.
type LlamaCpp struct {
	id       string
	base     string
	kvBudget int
	client   *http.Client
	inflight atomic.Int64
	breaker  *CircuitBreaker
}

// NewLlamaCpp constructs a LlamaCpp backend with sane defaults.
func NewLlamaCpp(opts LlamaCppOptions) (*LlamaCpp, error) {
	if opts.ID == "" {
		return nil, errors.New("llamacpp backend: ID is required")
	}
	if opts.URL == "" {
		return nil, errors.New("llamacpp backend: URL is required")
	}
	u, err := url.Parse(opts.URL)
	if err != nil {
		return nil, fmt.Errorf("llamacpp backend: parse URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("llamacpp backend: URL %q must be http or https", opts.URL)
	}
	if opts.KVBudget < 0 {
		return nil, errors.New("llamacpp backend: KVBudget must be >= 0")
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{
			// No client-level timeout: that would also cap the streaming body.
			// We cap header read time on the transport instead.
			Transport: &http.Transport{
				MaxIdleConns:          64,
				MaxIdleConnsPerHost:   16,
				IdleConnTimeout:       90 * time.Second,
				ResponseHeaderTimeout: timeout,
			},
		}
	}
	return &LlamaCpp{
		id:       opts.ID,
		base:     strings.TrimRight(opts.URL, "/"),
		kvBudget: opts.KVBudget,
		client:   client,
		breaker:  NewCircuitBreaker(opts.CircuitBreakerThreshold, opts.CircuitBreakerCooldown),
	}, nil
}

// Healthy implements Backend. Returns false while the circuit breaker is
// open. The breaker auto-resets after its cooldown.
func (b *LlamaCpp) Healthy() bool { return b.breaker.Allow() }

// CircuitState exposes the breaker's snapshot for diagnostics and tests.
func (b *LlamaCpp) CircuitState() CircuitState { return b.breaker.Snapshot() }

// ID implements Backend.
func (b *LlamaCpp) ID() string { return b.id }

// URL implements Backend.
func (b *LlamaCpp) URL() string { return b.base }

// KVBudget implements Backend.
func (b *LlamaCpp) KVBudget() int { return b.kvBudget }

// Inflight implements Backend.
func (b *LlamaCpp) Inflight() int64 { return b.inflight.Load() }

// Acquire implements Backend.
func (b *LlamaCpp) Acquire() { b.inflight.Add(1) }

// Release implements Backend. It clamps at zero rather than allowing the
// counter to go negative, which can otherwise mask double-Release bugs in a
// way that confuses operators reading dashboards.
func (b *LlamaCpp) Release() {
	for {
		v := b.inflight.Load()
		if v <= 0 {
			return
		}
		if b.inflight.CompareAndSwap(v, v-1) {
			return
		}
	}
}

// Do implements Backend. It builds an http.Request against the backend's base
// URL plus the request path and forwards it. The returned response body is
// expected to be streamed by the caller.
func (b *LlamaCpp) Do(ctx context.Context, r Request) (*http.Response, error) {
	if r.Method == "" {
		r.Method = http.MethodPost
	}
	if r.Path == "" {
		return nil, errors.New("backend: request path is empty")
	}
	if !strings.HasPrefix(r.Path, "/") {
		return nil, fmt.Errorf("backend: request path %q must start with '/'", r.Path)
	}
	full := b.base + r.Path
	req, err := http.NewRequestWithContext(ctx, r.Method, full, bytes.NewReader(r.Body))
	if err != nil {
		return nil, fmt.Errorf("backend: build request: %w", err)
	}
	for k, vs := range r.Headers {
		// Skip hop-by-hop headers; the proxy is responsible for end-to-end ones.
		if isHopByHop(k) {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if req.Header.Get("Content-Type") == "" && len(r.Body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := b.client.Do(req)
	if err != nil {
		b.breaker.RecordFailure()
		return nil, fmt.Errorf("backend %s: do: %w", b.id, err)
	}
	// 5xx counts as a server-side failure for the breaker; 4xx is a client
	// problem and should not trip routing away from this worker.
	if resp.StatusCode >= 500 {
		b.breaker.RecordFailure()
	} else {
		b.breaker.RecordSuccess()
	}
	return resp, nil
}

// hopByHopHeaders are not forwarded by an HTTP/1.1 proxy. List from RFC 7230.
var hopByHopHeaders = map[string]struct{}{
	"connection":          {},
	"keep-alive":          {},
	"proxy-authenticate":  {},
	"proxy-authorization": {},
	"te":                  {},
	"trailer":             {},
	"transfer-encoding":   {},
	"upgrade":             {},
}

func isHopByHop(h string) bool {
	_, ok := hopByHopHeaders[strings.ToLower(h)]
	return ok
}
