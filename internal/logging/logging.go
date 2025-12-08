// Package logging configures a single structured logger for the router and
// provides a small set of helpers (request IDs, access-log adapter) that the
// rest of the system uses through stable types.
//
// The logger is built on the standard library's log/slog. We avoid pulling
// in zap/zerolog so the runtime dependency surface stays minimal.
package logging

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/xzhou/llm-router/internal/proxy"
)

// New returns a *slog.Logger configured for the given level and format.
// Unrecognised values return a wrapped error so the caller can surface a
// useful message at startup.
//
// level: "debug" | "info" | "warn" | "error" (case-insensitive; empty defaults to info).
// format: "text" | "json" (case-insensitive; empty defaults to text).
func New(level, format string) (*slog.Logger, error) {
	return NewWithWriter(level, format, os.Stderr)
}

// NewWithWriter is like New but lets the caller plug in a writer (used by
// tests to capture output without touching stderr).
func NewWithWriter(level, format string, w io.Writer) (*slog.Logger, error) {
	var l slog.Level
	switch strings.ToLower(level) {
	case "debug":
		l = slog.LevelDebug
	case "info", "":
		l = slog.LevelInfo
	case "warn", "warning":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		return nil, fmt.Errorf("logging: invalid level %q", level)
	}
	opts := &slog.HandlerOptions{Level: l}
	var h slog.Handler
	switch strings.ToLower(format) {
	case "json":
		h = slog.NewJSONHandler(w, opts)
	case "text", "":
		h = slog.NewTextHandler(w, opts)
	default:
		return nil, fmt.Errorf("logging: invalid format %q", format)
	}
	return slog.New(h), nil
}

// RequestID returns a short hex-encoded random string suitable for request
// correlation. Falls back to a deterministic placeholder if the system RNG
// fails (which on macOS/Linux essentially never happens).
func RequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "no-rng"
	}
	return hex.EncodeToString(b[:])
}

// AccessLogRecorder returns a proxy.Recorder that logs one structured record
// per request at INFO level for successes and at ERROR level for failures.
func AccessLogRecorder(logger *slog.Logger) proxy.Recorder {
	if logger == nil {
		logger = slog.Default()
	}
	return func(s proxy.RequestStats) {
		attrs := []slog.Attr{
			slog.String("request_id", s.RequestID),
			slog.String("strategy", s.Strategy),
			slog.String("backend", s.BackendID),
			slog.String("reason", s.Reason),
			slog.Int("match_chunks", s.MatchChunks),
			slog.Int("status", s.StatusCode),
			slog.Duration("ttft", s.TTFT),
			slog.Duration("total", s.Total),
			slog.Int64("bytes_out", s.BytesOut),
		}
		if s.Err != "" {
			attrs = append(attrs, slog.String("err", s.Err))
			logger.LogAttrs(nil, slog.LevelError, "request failed", attrs...)
			return
		}
		logger.LogAttrs(nil, slog.LevelInfo, "request completed", attrs...)
	}
}
