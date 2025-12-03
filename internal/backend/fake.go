package backend

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"
)

// FakeServer is an httptest-based simulator of a llama.cpp-style OpenAI
// /v1/chat/completions endpoint. It is used by router and proxy tests to
// exercise the real HTTP code path (transport, headers, SSE) against a
// deterministic, low-overhead upstream.
//
// FakeServer is safe for concurrent calls. It records every received request
// for assertions; tests can call Requests() to inspect them.
type FakeServer struct {
	server     *httptest.Server
	statusCode int
	chunks     []string
	ttft       time.Duration
	interChunk time.Duration
	json       any
	customHFn  http.HandlerFunc

	mu       sync.Mutex
	received []ReceivedRequest
}

// ReceivedRequest is a captured copy of an inbound request.
type ReceivedRequest struct {
	Method  string
	Path    string
	Body    []byte
	Headers http.Header
	At      time.Time
}

// FakeServerOptions configures the simulator. The zero value is a server that
// answers every request with a single non-streaming JSON envelope.
type FakeServerOptions struct {
	// StatusCode for the response. Defaults to 200.
	StatusCode int
	// JSON is the response body for non-streaming responses. Marshalled with
	// encoding/json. Ignored if Chunks is non-empty.
	JSON any
	// Chunks are the SSE "data: ..." payloads to stream. The terminator
	// "data: [DONE]\n\n" is appended automatically. If empty, JSON is sent.
	Chunks []string
	// TTFT is the delay before the first byte of body is written.
	TTFT time.Duration
	// InterChunk is the delay between successive SSE chunks.
	InterChunk time.Duration
	// Handler, if non-nil, replaces all of the above and is invoked verbatim.
	Handler http.HandlerFunc
}

// NewFakeServer starts a FakeServer.
func NewFakeServer(opts FakeServerOptions) *FakeServer {
	f := &FakeServer{
		statusCode: opts.StatusCode,
		chunks:     opts.Chunks,
		ttft:       opts.TTFT,
		interChunk: opts.InterChunk,
		json:       opts.JSON,
		customHFn:  opts.Handler,
	}
	if f.statusCode == 0 {
		f.statusCode = http.StatusOK
	}
	if f.json == nil && len(f.chunks) == 0 && f.customHFn == nil {
		f.json = defaultChatCompletion()
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

// URL returns the base URL of the fake server.
func (f *FakeServer) URL() string { return f.server.URL }

// Close stops the fake server.
func (f *FakeServer) Close() { f.server.Close() }

// Backend constructs a LlamaCpp backend pointing at this fake server.
func (f *FakeServer) Backend(id string, kvBudget int) (*LlamaCpp, error) {
	return NewLlamaCpp(LlamaCppOptions{
		ID:       id,
		URL:      f.URL(),
		KVBudget: kvBudget,
	})
}

// Requests returns a copy of all requests received so far.
func (f *FakeServer) Requests() []ReceivedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ReceivedRequest, len(f.received))
	copy(out, f.received)
	return out
}

// Reset discards previously recorded requests. Useful between sub-tests.
func (f *FakeServer) Reset() {
	f.mu.Lock()
	f.received = f.received[:0]
	f.mu.Unlock()
}

func (f *FakeServer) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.received = append(f.received, ReceivedRequest{
		Method:  r.Method,
		Path:    r.URL.Path,
		Body:    body,
		Headers: r.Header.Clone(),
		At:      time.Now(),
	})
	f.mu.Unlock()

	if f.customHFn != nil {
		// Re-attach the body so the custom handler can read it.
		r.Body = io.NopCloser(bytes.NewReader(body))
		f.customHFn(w, r)
		return
	}

	// Streaming path
	if len(f.chunks) > 0 {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(f.statusCode)
		flusher, _ := w.(http.Flusher)
		if f.ttft > 0 {
			time.Sleep(f.ttft)
		}
		for i, c := range f.chunks {
			if i > 0 && f.interChunk > 0 {
				time.Sleep(f.interChunk)
			}
			fmt.Fprintf(w, "data: %s\n\n", c)
			if flusher != nil {
				flusher.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		return
	}

	// Non-streaming path
	if f.ttft > 0 {
		time.Sleep(f.ttft)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(f.statusCode)
	_ = json.NewEncoder(w).Encode(f.json)
}

// defaultChatCompletion is the default non-streaming response if the test
// supplies neither JSON nor Chunks. Shape matches llama.cpp's OpenAI emulation.
func defaultChatCompletion() map[string]any {
	return map[string]any{
		"id":     "chatcmpl-fake",
		"object": "chat.completion",
		"model":  "fake-model",
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": "ok"},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
	}
}

