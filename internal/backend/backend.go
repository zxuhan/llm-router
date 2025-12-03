// Package backend abstracts an upstream LLM inference server behind a small
// interface so the router can target llama.cpp, mlx-lm.server, vLLM-compatible
// endpoints, or a fake backend in tests with the same code path.
//
// A Backend exposes:
//   - identity (ID, URL),
//   - the KV cache budget the router should assume for prefix-tree sizing,
//   - a Do method for forwarding a single request,
//   - lightweight in-flight counters that the router consults to make
//     load-aware decisions.
//
// Inflight tracking lives on the backend (not the proxy) so that any caller
// holding a Backend can cheaply observe instantaneous load. The proxy is
// responsible for calling Acquire/Release around Do.
package backend

import (
	"context"
	"net/http"
)

// Request describes a single upstream call. Body is pre-buffered: the proxy
// always reads the client body to extract the prompt for routing, so passing
// bytes avoids re-buffering inside the backend.
type Request struct {
	Method  string
	Path    string
	Body    []byte
	Headers http.Header
}

// Backend is the abstract upstream LLM inference server.
type Backend interface {
	// ID is a stable identifier used in logs and metrics.
	ID() string
	// URL returns the base URL of the upstream server.
	URL() string
	// KVBudget is the approximate number of cache "chunks" the router should
	// assume the upstream can hold before its KV cache evicts. Used by the
	// prefix tree's LRU sizing. Zero disables the budget hint.
	KVBudget() int

	// Do forwards a single HTTP request and returns the response. The caller
	// owns the response body and must close it. Streaming responses (SSE) are
	// returned with their body unread; the caller streams from there.
	Do(ctx context.Context, r Request) (*http.Response, error)

	// Inflight returns the number of currently outstanding requests this
	// backend is processing.
	Inflight() int64
	// Acquire increments the inflight counter; called by the proxy before Do.
	Acquire()
	// Release decrements the inflight counter; called by the proxy after the
	// response has been fully proxied (or on error).
	Release()
}
