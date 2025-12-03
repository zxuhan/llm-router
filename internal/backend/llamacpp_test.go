package backend

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewLlamaCpp_Validation(t *testing.T) {
	cases := []struct {
		name string
		opts LlamaCppOptions
		want string
	}{
		{"missing id", LlamaCppOptions{URL: "http://x:1"}, "ID is required"},
		{"missing url", LlamaCppOptions{ID: "w0"}, "URL is required"},
		{"unparseable url", LlamaCppOptions{ID: "w0", URL: "://broken"}, "parse URL"},
		{"non-http", LlamaCppOptions{ID: "w0", URL: "ftp://x"}, "must be http or https"},
		{"negative kv budget", LlamaCppOptions{ID: "w0", URL: "http://x:1", KVBudget: -1}, "KVBudget"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewLlamaCpp(tc.opts)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestNewLlamaCpp_DefaultsAndAccessors(t *testing.T) {
	b, err := NewLlamaCpp(LlamaCppOptions{ID: "w0", URL: "http://example.test:8001/", KVBudget: 1024})
	if err != nil {
		t.Fatalf("NewLlamaCpp: %v", err)
	}
	if b.ID() != "w0" {
		t.Errorf("ID = %q", b.ID())
	}
	// Trailing slash should be normalised.
	if b.URL() != "http://example.test:8001" {
		t.Errorf("URL = %q", b.URL())
	}
	if b.KVBudget() != 1024 {
		t.Errorf("KVBudget = %d", b.KVBudget())
	}
	if b.Inflight() != 0 {
		t.Errorf("Inflight = %d, want 0", b.Inflight())
	}
}

func TestLlamaCpp_AcquireReleaseClampsAtZero(t *testing.T) {
	b, _ := NewLlamaCpp(LlamaCppOptions{ID: "w0", URL: "http://x:1"})
	b.Acquire()
	b.Acquire()
	if b.Inflight() != 2 {
		t.Fatalf("Inflight = %d, want 2", b.Inflight())
	}
	b.Release()
	b.Release()
	b.Release() // extra; must clamp at zero
	if b.Inflight() != 0 {
		t.Fatalf("Inflight = %d, want 0", b.Inflight())
	}
}

func TestLlamaCpp_AcquireReleaseConcurrent(t *testing.T) {
	b, _ := NewLlamaCpp(LlamaCppOptions{ID: "w0", URL: "http://x:1"})
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				b.Acquire()
				b.Release()
			}
		}()
	}
	wg.Wait()
	if got := b.Inflight(); got != 0 {
		t.Fatalf("Inflight = %d, want 0 after balanced acquire/release", got)
	}
}

func TestLlamaCpp_Do_HappyPath(t *testing.T) {
	var got struct {
		method      string
		path        string
		body        string
		contentType string
		hadHopByHop bool
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method = r.Method
		got.path = r.URL.Path
		got.contentType = r.Header.Get("Content-Type")
		// Ensure hop-by-hop "Connection" header was *not* forwarded.
		got.hadHopByHop = r.Header.Get("Connection") != ""
		body, _ := io.ReadAll(r.Body)
		got.body = string(body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	b, err := NewLlamaCpp(LlamaCppOptions{ID: "w0", URL: srv.URL})
	if err != nil {
		t.Fatalf("NewLlamaCpp: %v", err)
	}

	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer token")
	hdr.Set("Connection", "keep-alive") // hop-by-hop
	resp, err := b.Do(context.Background(), Request{
		Method:  http.MethodPost,
		Path:    "/v1/chat/completions",
		Body:    []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
		Headers: hdr,
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if got.method != http.MethodPost {
		t.Errorf("method = %q", got.method)
	}
	if got.path != "/v1/chat/completions" {
		t.Errorf("path = %q", got.path)
	}
	if !strings.Contains(got.body, `"hi"`) {
		t.Errorf("body = %q", got.body)
	}
	if got.contentType != "application/json" {
		t.Errorf("content-type defaulting failed: %q", got.contentType)
	}
	if got.hadHopByHop {
		t.Errorf("Connection (hop-by-hop) header should not be forwarded")
	}
}

func TestLlamaCpp_Do_DefaultsMethodAndContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST default", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content-type = %q, want json default", r.Header.Get("Content-Type"))
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	b, _ := NewLlamaCpp(LlamaCppOptions{ID: "w0", URL: srv.URL})
	resp, err := b.Do(context.Background(), Request{Path: "/x", Body: []byte("{}")})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()
}

func TestLlamaCpp_Do_RejectsBadPath(t *testing.T) {
	b, _ := NewLlamaCpp(LlamaCppOptions{ID: "w0", URL: "http://example.test:8001"})
	if _, err := b.Do(context.Background(), Request{Path: ""}); err == nil {
		t.Error("expected error for empty path")
	}
	if _, err := b.Do(context.Background(), Request{Path: "no-leading-slash"}); err == nil {
		t.Error("expected error for path without leading slash")
	}
}

func TestLlamaCpp_Do_PreservesContentTypeWhenSet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "text/plain" {
			t.Errorf("content-type = %q, want text/plain (caller set it)", r.Header.Get("Content-Type"))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	b, _ := NewLlamaCpp(LlamaCppOptions{ID: "w0", URL: srv.URL})
	hdr := http.Header{}
	hdr.Set("Content-Type", "text/plain")
	resp, err := b.Do(context.Background(), Request{Path: "/x", Body: []byte("hi"), Headers: hdr})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()
}

func TestLlamaCpp_Do_PropagatesContextCancellation(t *testing.T) {
	// The handler intentionally stalls before writing a response. The client
	// context expires; we expect Do to return a context error promptly. The
	// upper time.After bound is just defensive cleanup so srv.Close() never
	// hangs if cancellation propagation is slow on this platform.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()
	b, _ := NewLlamaCpp(LlamaCppOptions{ID: "w0", URL: srv.URL})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := b.Do(ctx, Request{Path: "/x", Body: []byte("{}")})
	if err == nil {
		t.Fatal("expected context error")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "context") {
		t.Errorf("expected context-related error, got %v", err)
	}
}

func TestLlamaCpp_Do_TransportError(t *testing.T) {
	// Closed listener address; connect should fail.
	b, _ := NewLlamaCpp(LlamaCppOptions{ID: "w0", URL: "http://127.0.0.1:1"})
	_, err := b.Do(context.Background(), Request{Path: "/x", Body: []byte("{}")})
	if err == nil {
		t.Fatal("expected transport error")
	}
	if !strings.Contains(err.Error(), "backend w0") {
		t.Errorf("error %q should mention backend id", err.Error())
	}
}
