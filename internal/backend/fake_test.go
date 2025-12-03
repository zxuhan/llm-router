package backend

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFakeServer_DefaultJSON(t *testing.T) {
	f := NewFakeServer(FakeServerOptions{})
	defer f.Close()

	b, err := f.Backend("w0", 1024)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := b.Do(context.Background(), Request{
		Path: "/v1/chat/completions",
		Body: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload["object"] != "chat.completion" {
		t.Errorf("payload object = %v", payload["object"])
	}

	if got := f.Requests(); len(got) != 1 || got[0].Path != "/v1/chat/completions" {
		t.Errorf("Requests() = %#v", got)
	}
}

func TestFakeServer_StreamingChunks(t *testing.T) {
	chunks := []string{
		`{"choices":[{"delta":{"content":"Hel"}}]}`,
		`{"choices":[{"delta":{"content":"lo"}}]}`,
	}
	f := NewFakeServer(FakeServerOptions{
		Chunks:     chunks,
		TTFT:       2 * time.Millisecond,
		InterChunk: 1 * time.Millisecond,
	})
	defer f.Close()

	b, _ := f.Backend("w0", 0)
	start := time.Now()
	resp, err := b.Do(context.Background(), Request{
		Path: "/v1/chat/completions",
		Body: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}

	var lines []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	dur := time.Since(start)
	if dur < 2*time.Millisecond {
		t.Errorf("response too fast (%v); TTFT delay was not honoured", dur)
	}

	wantSubstrings := []string{`"Hel"`, `"lo"`, `[DONE]`}
	joined := strings.Join(lines, "\n")
	for _, w := range wantSubstrings {
		if !strings.Contains(joined, w) {
			t.Errorf("stream missing %q. got:\n%s", w, joined)
		}
	}
}

func TestFakeServer_CustomHandlerSeesBody(t *testing.T) {
	want := `{"messages":[{"role":"user","content":"echoed"}]}`
	f := NewFakeServer(FakeServerOptions{
		Handler: func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusTeapot)
			_, _ = w.Write(b)
		},
	})
	defer f.Close()

	b, _ := f.Backend("w0", 0)
	resp, err := b.Do(context.Background(), Request{
		Path: "/anything",
		Body: []byte(want),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTeapot {
		t.Errorf("status = %d, want 418", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != want {
		t.Errorf("echo body = %q, want %q", got, want)
	}
}

func TestFakeServer_RequestsCloneAndReset(t *testing.T) {
	f := NewFakeServer(FakeServerOptions{})
	defer f.Close()
	b, _ := f.Backend("w0", 0)

	for i := 0; i < 3; i++ {
		resp, err := b.Do(context.Background(), Request{Path: "/v1/x", Body: []byte("{}")})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	got := f.Requests()
	if len(got) != 3 {
		t.Fatalf("len(Requests) = %d", len(got))
	}
	// Mutating returned slice must not affect the server's internal state.
	got[0].Path = "mutated"
	if again := f.Requests(); again[0].Path == "mutated" {
		t.Errorf("Requests() returned non-copy")
	}
	f.Reset()
	if len(f.Requests()) != 0 {
		t.Errorf("Reset did not clear")
	}
}

func TestFakeServer_ConcurrentRequests(t *testing.T) {
	f := NewFakeServer(FakeServerOptions{})
	defer f.Close()
	b, _ := f.Backend("w0", 0)

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := b.Do(context.Background(), Request{Path: "/v1/x", Body: []byte("{}")})
			if err != nil {
				t.Errorf("Do: %v", err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}()
	}
	wg.Wait()
	if got := len(f.Requests()); got != 32 {
		t.Errorf("len(Requests) = %d, want 32", got)
	}
}

func TestFakeServer_StatusCodeOverride(t *testing.T) {
	f := NewFakeServer(FakeServerOptions{StatusCode: http.StatusInternalServerError})
	defer f.Close()
	b, _ := f.Backend("w0", 0)
	resp, err := b.Do(context.Background(), Request{Path: "/x", Body: []byte("{}")})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d", resp.StatusCode)
	}
}
