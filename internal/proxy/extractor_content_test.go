package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

func keyOf(t *testing.T, body string) string {
	t.Helper()
	got, err := DefaultExtractor([]byte(body))
	if err != nil {
		t.Fatalf("DefaultExtractor: %v", err)
	}
	return got
}

func TestExtractor_StringContentKeyUnchanged(t *testing.T) {
	body := `{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"u"}]}`
	want := "system:sys\nuser:u\n"
	if got := keyOf(t, body); got != want {
		t.Errorf("key = %q, want %q (string content must hash exactly as pre-patch)", got, want)
	}
}

func TestExtractor_PartListContentIsRoutable(t *testing.T) {
	cases := map[string]string{
		"text part list":   `[{"type":"text","text":"hello"}]`,
		"tool_result list": `[{"type":"tool_result","tool_use_id":"t1","content":"42"}]`,
		"image + text":     `[{"type":"image_url","image_url":{"url":"http://x/y.png"}},{"type":"text","text":"look"}]`,
	}
	for name, parts := range cases {
		body := `{"messages":[{"role":"user","content":` + parts + `}]}`
		got := keyOf(t, body)
		if !strings.HasPrefix(got, "user:") {
			t.Errorf("%s: key = %q, want it to start with the role", name, got)
		}
		if !strings.Contains(got, `"type":"text"`) && !strings.Contains(got, `"type":"tool_result"`) &&
			!strings.Contains(got, `"type":"image_url"`) {
			t.Errorf("%s: key %q does not reflect the part list", name, got)
		}
	}
}

func TestExtractor_NullContent(t *testing.T) {
	if got, want := keyOf(t, `{"messages":[{"role":"assistant","content":null}]}`), "assistant:\n"; got != want {
		t.Errorf("key = %q, want %q", got, want)
	}
}

func TestExtractor_PrefixProperty(t *testing.T) {
	shapes := []string{`"more"`, `[{"type":"text","text":"more"}]`}
	for _, last := range shapes {
		short := `{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"one"}]}`
		long := `{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"one"},` +
			`{"role":"assistant","content":"ok"},{"role":"user","content":` + last + `}]}`
		kShort, kLong := keyOf(t, short), keyOf(t, long)
		if !strings.HasPrefix(kLong, kShort) {
			t.Errorf("last content %s: longer key %q does not extend shorter key %q", last, kLong, kShort)
		}
		if len(kLong) <= len(kShort) {
			t.Errorf("last content %s: expected the longer key to be strictly longer", last)
		}
	}
}

func TestExtractor_DifferentPartListsDiffer(t *testing.T) {
	a := `{"messages":[{"role":"user","content":[{"type":"text","text":"alpha"}]}]}`
	b := `{"messages":[{"role":"user","content":[{"type":"text","text":"beta"}]}]}`
	if keyOf(t, a) == keyOf(t, b) {
		t.Error("distinct part lists must not share a routing key")
	}
}

func TestExtractor_WhitespaceInsensitive(t *testing.T) {
	pretty := `{"messages":[{"role":"user","content":[{"type":"text",  "text":"same"}]}]}`
	tight := `{"messages":[{"role":"user","content":[{"type":"text","text":"same"}]}]}`
	if keyOf(t, pretty) != keyOf(t, tight) {
		t.Error("insignificant whitespace inside a part list must not change the key")
	}
}

func TestExtractor_ToolCallsArePartOfTheKey(t *testing.T) {
	a := `{"messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"c1","function":{"name":"f","arguments":"{}"}}]}]}`
	b := `{"messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"c2","function":{"name":"g","arguments":"{}"}}]}]}`
	if keyOf(t, a) == keyOf(t, b) {
		t.Error("assistant turns differing only in tool_calls must not share a key")
	}
	if !strings.Contains(keyOf(t, a), "|tool_calls:") {
		t.Error("tool_calls not folded into the key")
	}
}

func TestExtractor_ToolCallIDIsPartOfTheKey(t *testing.T) {
	a := `{"messages":[{"role":"tool","tool_call_id":"c1","content":"42"}]}`
	b := `{"messages":[{"role":"tool","tool_call_id":"c2","content":"42"}]}`
	if keyOf(t, a) == keyOf(t, b) {
		t.Error("tool results with different tool_call_id must not share a key")
	}
}

func TestExtractor_ToolsHeaderStillFoldedIn(t *testing.T) {
	got := keyOf(t, `{"tools":[{"type":"function","function":{"name":"f"}}],"messages":[]}`)
	if !strings.Contains(got, "tools:") || !strings.Contains(got, `"name":"f"`) {
		t.Errorf("tools block not reflected: %q", got)
	}
}

func TestExtractor_BadJSONStillErrors(t *testing.T) {
	if _, err := DefaultExtractor([]byte("not json")); err == nil {
		t.Fatal("expected error for a body that is not JSON")
	}
}

func TestExtractor_RealisticAgentTurn(t *testing.T) {
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
	body, err := json.Marshal(map[string]any{"model": "m", "messages": msgs})
	if err != nil {
		t.Fatal(err)
	}
	key, err := DefaultExtractor(body)
	if err != nil {
		t.Fatalf("realistic agent turn rejected: %v", err)
	}
	for _, want := range []string{"system:You are a coding agent.", "what file is this?", "data:image/png;base64,AAAA", "|tool_calls:", "tool<c1>:package main"} {
		if !strings.Contains(key, want) {
			t.Errorf("key missing %q; key=%q", want, key)
		}
	}
}
