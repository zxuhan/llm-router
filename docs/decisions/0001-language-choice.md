# ADR 0001: Implement the router in Go (rather than Rust)

## Context

The router is a concurrent HTTP service that proxies streaming responses,
tracks per-worker in-flight counters, and maintains a per-worker prefix tree.
It needs to be fast enough not to dominate the cost of the LLM call (TTFT
overhead < 5 ms on a warm path is a reasonable bar) and easy enough to
operate that a small team can keep it healthy.

Two natural language choices are Go and Rust. Both can produce a single
static binary, both have mature HTTP stacks, both have good observability
ecosystems. The decision is project-shape, not raw-speed.

## Decision

Use Go.

## Rationale

- **Concurrency model fits the problem.** Goroutines plus `sync.RWMutex`
  make the per-worker tree trivially expressible. Streaming requests are
  one-goroutine-per-request out of the box.
- **HTTP and SSE pass-through are first-class in stdlib.** `net/http` plus
  `http.Flusher` does what we need without third-party libs. The proxy is
  ~150 lines.
- **Prometheus, slog, and httptest are well-trod.** Equivalent Rust stacks
  exist (axum, tracing, prometheus-client, wiremock-rs) but each adds a
  decision.
- **Build and iteration speed.** `go build ./...` is a few seconds; the
  comparable Rust project would compile considerably more slowly during
  development, which matters for a small portfolio reference.
- **Borrow checker overhead is not paid back here.** The hot data structure
  is a tree behind a mutex. Rust's lifetime guarantees pay off for
  zero-copy, allocator-aware systems; for a router whose hottest allocation
  is a 4 KB stream buffer, the marginal benefit is not worth the friction.

## Consequences

- We accept Go's GC overhead. Pauses are bounded and well below LLM-call
  latencies; the GC has not appeared in any profile we have taken.
- Generics are available but used sparingly. Most code is concrete types
  with small interfaces (`Backend`, `Router`).
- Switching to Rust later is open: the wire surface is OpenAI-compatible
  HTTP, and the hot kernel (the radix tree) is around 250 lines of
  language-agnostic logic.
