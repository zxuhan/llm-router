package logging

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/zxuhan/llm-router/internal/proxy"
)

func TestNew_AllLevels(t *testing.T) {
	for _, lvl := range []string{"debug", "info", "warn", "warning", "error", ""} {
		t.Run(lvl, func(t *testing.T) {
			if _, err := New(lvl, "text"); err != nil {
				t.Errorf("New level %q: %v", lvl, err)
			}
		})
	}
}

func TestNew_BadLevel(t *testing.T) {
	if _, err := New("loud", "text"); err == nil {
		t.Fatal("expected error")
	}
}

func TestNew_BadFormat(t *testing.T) {
	if _, err := New("info", "yaml"); err == nil {
		t.Fatal("expected error")
	}
}

func TestNew_BothFormats(t *testing.T) {
	for _, f := range []string{"text", "json", ""} {
		if _, err := New("info", f); err != nil {
			t.Errorf("format %q: %v", f, err)
		}
	}
}

func TestNewWithWriter_WritesAtCorrectLevel(t *testing.T) {
	var buf bytes.Buffer
	l, err := NewWithWriter("warn", "json", &buf)
	if err != nil {
		t.Fatal(err)
	}
	l.Info("should not appear")
	l.Warn("should appear")
	out := buf.String()
	if strings.Contains(out, "should not appear") {
		t.Errorf("info leaked when level=warn: %s", out)
	}
	if !strings.Contains(out, "should appear") {
		t.Errorf("warn missing: %s", out)
	}
}

func TestRequestID_FormatAndUniqueness(t *testing.T) {
	a := RequestID()
	b := RequestID()
	if len(a) != 16 || len(b) != 16 {
		t.Fatalf("RequestID lengths = %d, %d", len(a), len(b))
	}
	if a == b {
		t.Fatalf("RequestID returned the same value twice: %q", a)
	}
}

func TestAccessLogRecorder_LogsSuccess(t *testing.T) {
	var buf bytes.Buffer
	l, err := NewWithWriter("debug", "json", &buf)
	if err != nil {
		t.Fatal(err)
	}
	rec := AccessLogRecorder(l)
	rec(proxy.RequestStats{
		RequestID:   "req-abc",
		BackendID:   "a",
		Strategy:    "prefixaware",
		Reason:      "longest-prefix",
		MatchChunks: 4,
		StatusCode:  200,
		TTFT:        2 * time.Millisecond,
		Total:       10 * time.Millisecond,
		BytesOut:    100,
	})
	out := buf.String()
	if !strings.Contains(out, `"request_id":"req-abc"`) {
		t.Errorf("missing request_id: %s", out)
	}
	if !strings.Contains(out, `"msg":"request completed"`) {
		t.Errorf("missing success message: %s", out)
	}
	if !strings.Contains(out, `"backend":"a"`) {
		t.Errorf("missing backend: %s", out)
	}
}

func TestAccessLogRecorder_LogsError(t *testing.T) {
	var buf bytes.Buffer
	l, err := NewWithWriter("debug", "json", &buf)
	if err != nil {
		t.Fatal(err)
	}
	AccessLogRecorder(l)(proxy.RequestStats{
		RequestID:  "req-x",
		StatusCode: 502,
		Err:        "upstream down",
	})
	out := buf.String()
	if !strings.Contains(out, `"msg":"request failed"`) {
		t.Errorf("missing failure message: %s", out)
	}
	if !strings.Contains(out, `"err":"upstream down"`) {
		t.Errorf("missing err attr: %s", out)
	}
	if !strings.Contains(out, `"level":"ERROR"`) {
		t.Errorf("missing ERROR level: %s", out)
	}
}

func TestAccessLogRecorder_NilLoggerUsesDefault(t *testing.T) {
	rec := AccessLogRecorder(nil)
	// Must not panic; we can't easily assert against the default logger
	// without redirecting, but coverage of the nil branch is enough.
	rec(proxy.RequestStats{RequestID: "r", StatusCode: 200})
}
