package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Regression coverage for the failure seen with the router behind a LiteLLM
// proxy in front of Pi:
//
//	400 parse body: json: cannot unmarshal array into Go struct field
//	    .messages.content of type string
//
// messages[].content was decoded as a Go string, so a request using OpenAI's
// content-part list form died in the extractor and never reached the router.
// These drive the real handler over HTTP so the 400 itself is what breaks.

// historicFailure is the response body recorded from the live router on 2026-09-22.
const historicFailure = "parse body: json: cannot unmarshal array into Go struct field " +
	".messages.content of type string"

func postCompletions(t *testing.T, url, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(url+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, strings.TrimSpace(string(b))
}

// assertRouted fails on anything the handler does not forward. The reject body is
// matched exactly so a regression reads as the incident itself.
func assertRouted(t *testing.T, status int, body string) {
	t.Helper()
	if status == http.StatusBadRequest && body == historicFailure {
		t.Fatalf("reproduced the recorded litellm/Pi failure: %q", body)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", status, body)
	}
}

func TestProxy_ContentPartListIsRoutable(t *testing.T) {
	shapes := map[string]string{
		"text part list":   `[{"type":"text","text":"Reply with the word: alpha"}]`,
		"tool_result list": `[{"type":"tool_result","tool_use_id":"t1","content":"42"}]`,
		"image + text":     `[{"type":"image_url","image_url":{"url":"http://x/y.png"}},{"type":"text","text":"Reply: beta"}]`,
	}
	for name, parts := range shapes {
		t.Run(name, func(t *testing.T) {
			h, _, _, _ := newTwoBackendHandler(t)
			srv := httptest.NewServer(h)
			defer srv.Close()

			body := `{"model":"m","max_tokens":8,"messages":[` +
				`{"role":"system","content":"Be terse."},` +
				`{"role":"user","content":` + parts + `}]}`
			status, respBody := postCompletions(t, srv.URL, body)
			assertRouted(t, status, respBody)
		})
	}
}

func TestProxy_RealisticAgentTurnIsRoutable(t *testing.T) {
	msgs := []map[string]any{
		{"role": "system", "content": "You are a coding agent."},
		{"role": "user", "content": []map[string]any{
			{"type": "text", "text": "what file is this?"},
			{"type": "image_url", "image_url": map[string]string{"url": "data:image/png;base64,AAAA"}},
		}},
		{"role": "assistant", "content": nil, "tool_calls": []map[string]any{
			{"id": "c1", "type": "function", "function": map[string]string{"name": "read", "arguments": `{"path":"a.go"}`}},
		}},
		{"role": "tool", "tool_call_id": "c1", "content": "package main"},
		{"role": "user", "content": []map[string]any{{"type": "text", "text": "thanks"}}},
	}
	body, err := json.Marshal(map[string]any{"model": "m", "max_tokens": 8, "messages": msgs})
	if err != nil {
		t.Fatal(err)
	}

	h, a, b, _ := newTwoBackendHandler(t)
	srv := httptest.NewServer(h)
	defer srv.Close()

	status, respBody := postCompletions(t, srv.URL, string(body))
	assertRouted(t, status, respBody)

	if got := len(a.Requests()) + len(b.Requests()); got != 1 {
		t.Errorf("backends received %d requests, want 1: the turn was not forwarded", got)
	}
}
