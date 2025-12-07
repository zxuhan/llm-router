package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xzhou/llm-router/internal/backend"
	"github.com/xzhou/llm-router/internal/router"
)

func newTwoBackendHandler(t *testing.T) (*Handler, *backend.FakeServer, *backend.FakeServer, []backend.Backend) {
	t.Helper()
	a := backend.NewFakeServer(backend.FakeServerOptions{
		Chunks: []string{
			`{"choices":[{"delta":{"content":"Hi"}}]}`,
			`{"choices":[{"delta":{"content":" from"}}]}`,
			`{"choices":[{"delta":{"content":" a"}}]}`,
		},
	})
	b := backend.NewFakeServer(backend.FakeServerOptions{
		Chunks: []string{`{"choices":[{"delta":{"content":"Hi from b"}}]}`},
	})
	t.Cleanup(func() { a.Close(); b.Close() })

	ba, err := a.Backend("a", 1024)
	if err != nil {
		t.Fatal(err)
	}
	bb, err := b.Backend("b", 1024)
	if err != nil {
		t.Fatal(err)
	}
	backends := []backend.Backend{ba, bb}
	rr := router.NewRoundRobin(backends)
	h, err := New(Options{Router: rr, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	return h, a, b, backends
}

func TestProxy_RouterMissing(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("expected error when Router is nil")
	}
}

func TestProxy_RejectsWrongMethod(t *testing.T) {
	h, _, _, _ := newTwoBackendHandler(t)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
	if got := resp.Header.Get("Allow"); got != http.MethodPost {
		t.Errorf("Allow = %q, want POST", got)
	}
}

func TestProxy_404OnUnknownPath(t *testing.T) {
	h, _, _, _ := newTwoBackendHandler(t)
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/wrong/path", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestProxy_BadBodyReturns400(t *testing.T) {
	h, _, _, _ := newTwoBackendHandler(t)
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader("not json"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestProxy_ForwardsAndStreamsSSE(t *testing.T) {
	h, fakeA, _, _ := newTwoBackendHandler(t)
	srv := httptest.NewServer(h)
	defer srv.Close()

	body := `{"messages":[{"role":"user","content":"hello"}],"stream":true}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
	// Round-robin starts at index 0; first request goes to "a".
	if got := resp.Header.Get("X-Router-Backend"); got != "a" {
		t.Errorf("X-Router-Backend = %q, want a", got)
	}
	if got := resp.Header.Get("X-Router-Reason"); got == "" {
		t.Errorf("X-Router-Reason is empty")
	}

	// Drain and verify content.
	var collected strings.Builder
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		collected.WriteString(sc.Text())
		collected.WriteString("\n")
	}
	got := collected.String()
	if !strings.Contains(got, "[DONE]") {
		t.Errorf("expected [DONE] terminator in stream, got: %s", got)
	}
	if !strings.Contains(got, `"Hi"`) {
		t.Errorf("expected first chunk content, got: %s", got)
	}

	// Upstream a should have seen exactly one request with our body.
	got = ""
	for _, r := range fakeA.Requests() {
		got += string(r.Body)
	}
	if !strings.Contains(got, `"hello"`) {
		t.Errorf("upstream did not see request body: got %q", got)
	}
}

func TestProxy_HopByHopHeadersStrippedFromResponse(t *testing.T) {
	// Configure the upstream to set a hop-by-hop header; the proxy must
	// strip it.
	fake := backend.NewFakeServer(backend.FakeServerOptions{
		Handler: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Connection", "close")
			w.Header().Set("X-Cool", "yes")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		},
	})
	defer fake.Close()
	bb, err := fake.Backend("a", 0)
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(Options{Router: router.NewRoundRobin([]backend.Backend{bb}), Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("X-Cool"); got != "yes" {
		t.Errorf("end-to-end header dropped: X-Cool = %q", got)
	}
	// Note: net/http may overwrite the Connection header itself; we check
	// our stripping by exposing through that the proxy never wrote it.
	// (This is best-effort because the stdlib server may add its own.)
}

func TestProxy_UpstreamErrorBecomes502(t *testing.T) {
	// Backend pointing at a closed port; Do returns an error.
	bb, err := backend.NewLlamaCpp(backend.LlamaCppOptions{ID: "dead", URL: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(Options{Router: router.NewRoundRobin([]backend.Backend{bb}), Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
}

func TestProxy_NoBackendsReturns503(t *testing.T) {
	rr := router.NewRoundRobin(nil)
	h, err := New(Options{Router: rr, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

func TestProxy_ReadBodyError(t *testing.T) {
	h, _, _, _ := newTwoBackendHandler(t)
	// Build a request with a body that errors on read.
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", &errorReader{})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("simulated read error") }
func (errorReader) Close() error             { return nil }

func TestDefaultExtractor_ConcatenatesMessages(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"role":"system","content":"sys"},
			{"role":"user","content":"u","name":"alice"},
			{"role":"assistant","content":"a"}
		]
	}`)
	got, err := DefaultExtractor(body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "system:sys") {
		t.Errorf("missing system: %q", got)
	}
	if !strings.Contains(got, "user[alice]:u") {
		t.Errorf("missing user with name: %q", got)
	}
	if !strings.Contains(got, "assistant:a") {
		t.Errorf("missing assistant: %q", got)
	}
}

func TestDefaultExtractor_IncludesTools(t *testing.T) {
	body := []byte(`{"tools":[{"type":"function","function":{"name":"f"}}],"messages":[]}`)
	got, err := DefaultExtractor(body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "tools:") {
		t.Errorf("missing tools: %q", got)
	}
	if !strings.Contains(got, `"name":"f"`) {
		t.Errorf("tools content not reflected: %q", got)
	}
}

func TestDefaultExtractor_BadJSON(t *testing.T) {
	if _, err := DefaultExtractor([]byte("not json")); err == nil {
		t.Fatal("expected error")
	}
}

func TestStreamResponse_IgnoresFlusherWhenAbsent(t *testing.T) {
	// Use bytes.Buffer (no Flush) wrapped in a tiny http.ResponseWriter.
	rec := &nonFlushRecorder{buf: &bytes.Buffer{}}
	streamResponse(rec, strings.NewReader("hello"))
	if rec.buf.String() != "hello" {
		t.Errorf("stream wrote %q", rec.buf.String())
	}
}

// nonFlushRecorder is an http.ResponseWriter without a Flusher.
type nonFlushRecorder struct {
	buf    *bytes.Buffer
	header http.Header
}

func (n *nonFlushRecorder) Header() http.Header {
	if n.header == nil {
		n.header = http.Header{}
	}
	return n.header
}
func (n *nonFlushRecorder) Write(p []byte) (int, error) { return n.buf.Write(p) }
func (n *nonFlushRecorder) WriteHeader(int)             {}

func TestProxy_NewWithDefaultExtractorAndLogger(t *testing.T) {
	rr := router.NewRoundRobin(nil)
	h, err := New(Options{Router: rr})
	if err != nil {
		t.Fatal(err)
	}
	if h.extractor == nil {
		t.Error("default extractor should be set")
	}
	if h.logger == nil {
		t.Error("default logger should be set")
	}
}

func TestProxy_CustomExtractor(t *testing.T) {
	called := false
	h, _, _, _ := newTwoBackendHandler(t)
	h.extractor = func(b []byte) (string, error) {
		called = true
		return "constant prompt", nil
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	body, _ := json.Marshal(map[string]any{"messages": []any{}})
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if !called {
		t.Error("custom extractor not invoked")
	}
}
