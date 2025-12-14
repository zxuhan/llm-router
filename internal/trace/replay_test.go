package trace

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newSpyServer(t *testing.T) (string, *atomic.Int64, func()) {
	t.Helper()
	var count atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.Header().Set("X-Router-Backend", "fake-w0")
		w.Header().Set("X-Router-Reason", "longest-prefix")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Two SSE chunks with a small gap to make TTFT measurable.
		_, _ = w.Write([]byte("data: hello\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(2 * time.Millisecond)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	return srv.URL, &count, srv.Close
}

func TestReplayer_FiresEveryRequest(t *testing.T) {
	endpoint, count, closeSrv := newSpyServer(t)
	defer closeSrv()

	tr := Trace{Requests: []Request{
		{SessionID: "s1", DelayMs: 0, Pattern: "x", Body: Body{Messages: []Message{{Role: "user", Content: "hi"}}}},
		{SessionID: "s1", DelayMs: 5, Pattern: "x", Body: Body{Messages: []Message{{Role: "user", Content: "again"}}}},
		{SessionID: "s2", DelayMs: 0, Pattern: "x", Body: Body{Messages: []Message{{Role: "user", Content: "hello"}}}},
	}}

	r := &Replayer{Endpoint: endpoint}
	res, err := r.Replay(context.Background(), tr)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 3 {
		t.Fatalf("len(res) = %d", len(res))
	}
	if got := count.Load(); got != 3 {
		t.Errorf("server saw %d requests, want 3", got)
	}
	for i, r := range res {
		if r.StatusCode != http.StatusOK {
			t.Errorf("res[%d] status = %d", i, r.StatusCode)
		}
		if r.BackendID != "fake-w0" {
			t.Errorf("res[%d] backend = %q", i, r.BackendID)
		}
		if r.TTFT <= 0 {
			t.Errorf("res[%d] TTFT not recorded", i)
		}
		if r.BytesIn == 0 {
			t.Errorf("res[%d] BytesIn = 0", i)
		}
	}
}

func TestReplayer_PreservesIndexOrder(t *testing.T) {
	endpoint, _, closeSrv := newSpyServer(t)
	defer closeSrv()

	tr := Trace{Requests: []Request{
		{SessionID: "a", DelayMs: 0, Pattern: "p1"},
		{SessionID: "b", DelayMs: 0, Pattern: "p2"},
		{SessionID: "a", DelayMs: 5, Pattern: "p3"},
	}}
	r := &Replayer{Endpoint: endpoint}
	res, err := r.Replay(context.Background(), tr)
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Pattern != "p1" || res[1].Pattern != "p2" || res[2].Pattern != "p3" {
		t.Errorf("results out of order: %v %v %v", res[0].Pattern, res[1].Pattern, res[2].Pattern)
	}
	if res[0].Index != 0 || res[1].Index != 1 || res[2].Index != 2 {
		t.Errorf("indices wrong")
	}
}

func TestReplayer_MissingEndpoint(t *testing.T) {
	r := &Replayer{}
	_, err := r.Replay(context.Background(), Trace{})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestReplayer_ContextCancellationStopsRemainingTurns(t *testing.T) {
	endpoint, count, closeSrv := newSpyServer(t)
	defer closeSrv()

	// Only one trace request fires before cancellation.
	tr := Trace{Requests: []Request{
		{SessionID: "s", DelayMs: 0, Pattern: "p"},
		{SessionID: "s", DelayMs: 500, Pattern: "p"},
		{SessionID: "s", DelayMs: 1000, Pattern: "p"},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	r := &Replayer{Endpoint: endpoint}

	done := make(chan []Result, 1)
	go func() {
		res, _ := r.Replay(ctx, tr)
		done <- res
	}()
	// Let the first request fire then cancel.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case res := <-done:
		if got := count.Load(); got > 1 {
			t.Errorf("expected at most 1 dispatched request after cancellation, got %d", got)
		}
		// At least the first should be present.
		if res[0].StatusCode != http.StatusOK {
			t.Errorf("first result not fired: %#v", res[0])
		}
		// Later results should carry the cancellation error.
		if res[1].Err == "" && res[2].Err == "" {
			t.Errorf("expected cancellation errors on later turns")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Replay did not return after cancellation")
	}
}

func TestReplayer_DialErrorRecorded(t *testing.T) {
	r := &Replayer{Endpoint: "http://127.0.0.1:1"}
	tr := Trace{Requests: []Request{{SessionID: "s", DelayMs: 0, Body: Body{}}}}
	res, err := r.Replay(context.Background(), tr)
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Err == "" || !strings.Contains(res[0].Err, "do:") {
		t.Errorf("expected do error, got %q", res[0].Err)
	}
}

func TestReplayer_ReadErrorRecorded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hijack and close the connection mid-stream.
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("no hijacker")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatal(err)
		}
		// Write minimal headers + body, then RST.
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\nshort"))
		conn.Close()
	}))
	defer srv.Close()
	r := &Replayer{Endpoint: srv.URL}
	tr := Trace{Requests: []Request{{SessionID: "s", DelayMs: 0}}}
	res, err := r.Replay(context.Background(), tr)
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Err == "" {
		t.Errorf("expected read error to be captured")
	}
}

func TestReplayer_BadEndpointURL(t *testing.T) {
	r := &Replayer{Endpoint: "http://%xx_invalid"}
	tr := Trace{Requests: []Request{{SessionID: "s", DelayMs: 0}}}
	res, _ := r.Replay(context.Background(), tr)
	if !strings.Contains(res[0].Err, "new request") {
		t.Errorf("expected new-request error, got %q", res[0].Err)
	}
}

func TestReplayer_ParsesUpstreamCachedTokens(t *testing.T) {
	// JSON upstream returning OpenAI-style usage with cached_tokens. The
	// replayer should populate Result.PromptTokens / CachedTokens.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
		  "usage": {
		    "prompt_tokens": 100,
		    "prompt_tokens_details": {"cached_tokens": 75}
		  },
		  "choices": [{"message": {"role":"assistant","content":"ok"}}]
		}`))
	}))
	defer srv.Close()
	r := &Replayer{Endpoint: srv.URL}
	tr := Trace{Requests: []Request{{SessionID: "s", DelayMs: 0, Body: Body{}}}}
	res, err := r.Replay(context.Background(), tr)
	if err != nil {
		t.Fatal(err)
	}
	if res[0].PromptTokens != 100 || res[0].CachedTokens != 75 {
		t.Errorf("PromptTokens=%d CachedTokens=%d, want 100/75", res[0].PromptTokens, res[0].CachedTokens)
	}
}

func TestReplayer_NonJSONUpstreamLeavesUsageZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: hello\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()
	r := &Replayer{Endpoint: srv.URL}
	tr := Trace{Requests: []Request{{SessionID: "s", DelayMs: 0, Body: Body{}}}}
	res, _ := r.Replay(context.Background(), tr)
	if res[0].PromptTokens != 0 || res[0].CachedTokens != 0 {
		t.Errorf("expected usage zero for streaming response; got %d/%d", res[0].PromptTokens, res[0].CachedTokens)
	}
}

func TestReplayer_MalformedJSONUsageIsSwallowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`not really json`))
	}))
	defer srv.Close()
	r := &Replayer{Endpoint: srv.URL}
	tr := Trace{Requests: []Request{{SessionID: "s", DelayMs: 0, Body: Body{}}}}
	res, err := r.Replay(context.Background(), tr)
	if err != nil {
		t.Fatal(err)
	}
	// Body is not parseable, but the request still succeeded - usage is just zero.
	if res[0].StatusCode != 200 || res[0].PromptTokens != 0 || res[0].CachedTokens != 0 {
		t.Errorf("unexpected: %+v", res[0])
	}
}

func TestIsJSONContentType(t *testing.T) {
	cases := []struct {
		ct   string
		want bool
	}{
		{"application/json", true},
		{"application/json; charset=utf-8", true},
		{"text/event-stream", false},
		{"text/plain", false},
		{"", false},
		{"applicat", false},
	}
	for _, c := range cases {
		if got := isJSONContentType(c.ct); got != c.want {
			t.Errorf("isJSONContentType(%q) = %v, want %v", c.ct, got, c.want)
		}
	}
}

// ensure errors.Is path types stay imported (lint defence).
var _ = errors.New
var _ = io.EOF
