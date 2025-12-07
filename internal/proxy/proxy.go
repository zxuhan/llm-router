// Package proxy is the HTTP front door for the router.
//
// The proxy speaks the OpenAI-compatible /v1/chat/completions API. For each
// request it extracts the prompt, asks the configured Router which backend
// should serve it, forwards the request, and streams the response back to
// the client without buffering. SSE chunks are flushed promptly so that
// time-to-first-token reaches the client as soon as the upstream produces it.
package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/xzhou/llm-router/internal/backend"
	"github.com/xzhou/llm-router/internal/router"
)

// PromptExtractor pulls a routing-relevant string out of a request body. The
// router uses this string for prefix matching; only the byte-for-byte prefix
// matters, so any deterministic concatenation of message fields works.
type PromptExtractor func(body []byte) (string, error)

// RequestStats summarises one proxied request. The Recorder hook receives one
// RequestStats per request, regardless of success or failure. It is the wiring
// point for metrics, structured logs, and any other observer.
type RequestStats struct {
	BackendID   string        // chosen backend id (empty if route failed)
	Strategy    string        // router name
	Reason      string        // routing reason ("longest-prefix", "fallback-...")
	MatchChunks int           // prefix chunks the chosen backend already held
	StatusCode  int           // upstream status (0 if dispatch failed before headers)
	TTFT        time.Duration // time to first response byte (0 if never reached)
	Total       time.Duration // wall time from request entry to response close
	BytesOut    int64         // bytes streamed back to the client
	Err         string        // non-empty on failure
}

// Recorder is a callback invoked once per request. Implementations must be
// non-blocking; the handler does not bound how many goroutines call into it.
type Recorder func(RequestStats)

// Handler is the HTTP handler that proxies chat completions through the
// configured Router and Backend(s).
type Handler struct {
	router    router.Router
	extractor PromptExtractor
	logger    *log.Logger
	recorder  Recorder
}

// Options configures the Handler.
type Options struct {
	// Router is required.
	Router router.Router
	// Extractor turns the request body into a prompt string. Defaults to
	// DefaultExtractor (OpenAI chat-completions schema).
	Extractor PromptExtractor
	// Logger is an optional logger for non-fatal events. Defaults to log.Default().
	Logger *log.Logger
	// Recorder is invoked with a RequestStats summary at the end of every
	// request. Defaults to a no-op.
	Recorder Recorder
}

// New constructs a Handler.
func New(opts Options) (*Handler, error) {
	if opts.Router == nil {
		return nil, errors.New("proxy: Router is required")
	}
	ext := opts.Extractor
	if ext == nil {
		ext = DefaultExtractor
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}
	rec := opts.Recorder
	if rec == nil {
		rec = func(RequestStats) {}
	}
	return &Handler{router: opts.Router, extractor: ext, logger: logger, recorder: rec}, nil
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	stats := RequestStats{Strategy: h.router.Name()}
	defer func() {
		stats.Total = time.Since(start)
		h.recorder(stats)
	}()

	if r.URL.Path != "/v1/chat/completions" {
		http.NotFound(w, r)
		stats.StatusCode = http.StatusNotFound
		stats.Err = "not found"
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		stats.StatusCode = http.StatusMethodNotAllowed
		stats.Err = "method not allowed"
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		stats.StatusCode = http.StatusBadRequest
		stats.Err = "read body: " + err.Error()
		return
	}
	prompt, err := h.extractor(body)
	if err != nil {
		http.Error(w, "parse body: "+err.Error(), http.StatusBadRequest)
		stats.StatusCode = http.StatusBadRequest
		stats.Err = "parse body: " + err.Error()
		return
	}

	decision, err := h.router.Choose(r.Context(), prompt)
	if err != nil {
		http.Error(w, "route: "+err.Error(), http.StatusServiceUnavailable)
		stats.StatusCode = http.StatusServiceUnavailable
		stats.Err = "route: " + err.Error()
		return
	}
	stats.BackendID = decision.Backend.ID()
	stats.Reason = decision.Reason
	stats.MatchChunks = decision.MatchChunks

	// Update the router's per-worker prefix tree before dispatch so that
	// near-simultaneous requests with the same prefix can pin to the same
	// worker. The cost of being wrong (e.g. dispatch fails) is one cold start
	// at worst; see docs/decisions/0005-safety-valve.md for the trade-off.
	h.router.Update(prompt, decision.Backend)

	decision.Backend.Acquire()
	defer decision.Backend.Release()

	upstreamReq := backend.Request{
		Method:  r.Method,
		Path:    r.URL.Path,
		Body:    body,
		Headers: r.Header,
	}
	resp, err := decision.Backend.Do(r.Context(), upstreamReq)
	if err != nil {
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		h.logger.Printf("upstream error: backend=%s err=%v", decision.Backend.ID(), err)
		stats.StatusCode = http.StatusBadGateway
		stats.Err = "upstream: " + err.Error()
		return
	}
	defer resp.Body.Close()

	// Copy upstream headers (excluding hop-by-hop) and status.
	for k, vs := range resp.Header {
		if isHopByHop(k) {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	// Surface routing decisions to clients/operators via response headers.
	w.Header().Set("X-Router-Backend", decision.Backend.ID())
	w.Header().Set("X-Router-Reason", decision.Reason)
	w.WriteHeader(resp.StatusCode)
	stats.StatusCode = resp.StatusCode

	bytes, ttft := streamResponse(w, resp.Body, start)
	stats.BytesOut = bytes
	stats.TTFT = ttft
}

// streamResponse copies r into w with periodic flushes so SSE chunks reach
// the client without sitting in the response buffer. It also reports the
// time-to-first-byte (relative to start) and the total bytes written.
func streamResponse(w http.ResponseWriter, r io.Reader, start time.Time) (int64, time.Duration) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 4096)
	var total int64
	var ttft time.Duration
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if ttft == 0 {
				ttft = time.Since(start)
			}
			written, werr := w.Write(buf[:n])
			total += int64(written)
			if werr != nil {
				return total, ttft
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return total, ttft
		}
	}
}

// DefaultExtractor parses an OpenAI-style chat completion body and returns a
// stable string concatenation of the messages. The format is intentionally
// not a parsed JSON re-emit: any deterministic byte-identical string for the
// same logical messages will give the prefix tree a useful key.
func DefaultExtractor(body []byte) (string, error) {
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
			Name    string `json:"name,omitempty"`
		} `json:"messages"`
		// Tools and tool_calls are folded in as raw JSON below.
		Tools     json.RawMessage `json:"tools,omitempty"`
		ToolCalls json.RawMessage `json:"tool_calls,omitempty"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&req); err != nil {
		return "", err
	}
	var sb strings.Builder
	if len(req.Tools) > 0 {
		sb.WriteString("tools:")
		sb.Write(req.Tools)
		sb.WriteString("\n")
	}
	for _, m := range req.Messages {
		sb.WriteString(m.Role)
		if m.Name != "" {
			sb.WriteByte('[')
			sb.WriteString(m.Name)
			sb.WriteByte(']')
		}
		sb.WriteString(":")
		sb.WriteString(m.Content)
		sb.WriteString("\n")
	}
	return sb.String(), nil
}

// hopByHopHeaders are not forwarded between client and upstream. We replicate
// the small list rather than depending on net/http internal helpers.
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
