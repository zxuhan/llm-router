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

	"github.com/xzhou/llm-router/internal/backend"
	"github.com/xzhou/llm-router/internal/router"
)

// PromptExtractor pulls a routing-relevant string out of a request body. The
// router uses this string for prefix matching; only the byte-for-byte prefix
// matters, so any deterministic concatenation of message fields works.
type PromptExtractor func(body []byte) (string, error)

// Handler is the HTTP handler that proxies chat completions through the
// configured Router and Backend(s).
type Handler struct {
	router    router.Router
	extractor PromptExtractor
	logger    *log.Logger
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
	return &Handler{router: opts.Router, extractor: ext, logger: logger}, nil
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/chat/completions" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	prompt, err := h.extractor(body)
	if err != nil {
		http.Error(w, "parse body: "+err.Error(), http.StatusBadRequest)
		return
	}

	decision, err := h.router.Choose(r.Context(), prompt)
	if err != nil {
		http.Error(w, "route: "+err.Error(), http.StatusServiceUnavailable)
		return
	}

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

	streamResponse(w, resp.Body)
}

// streamResponse copies r into w with periodic flushes so SSE chunks reach
// the client without sitting in the response buffer.
func streamResponse(w http.ResponseWriter, r io.Reader) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
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
