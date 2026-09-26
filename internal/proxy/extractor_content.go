package proxy

import (
	"bytes"
	"encoding/json"
)

// contentKey renders one message's `content` value as routing-key text. A JSON
// string is returned verbatim so string-content traffic keeps byte-identical
// keys; anything else is keyed by its whitespace-compacted JSON.
func contentKey(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return compactJSON(raw)
}

// compactJSON removes insignificant whitespace from raw JSON, returning the
// input unchanged if it is not valid JSON.
func compactJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}
