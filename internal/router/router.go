// Package router selects a backend for an incoming chat-completion request.
//
// A Router is a small interface so that strategies can be swapped at runtime
// from configuration. The proxy speaks only to this interface; it does not
// know whether the strategy is prefix-aware or oblivious.
package router

import (
	"context"
	"errors"

	"github.com/xzhou/llm-router/internal/backend"
)

// Decision describes the outcome of a routing call. Reason is a short,
// machine-readable label used for metrics labels and structured logs.
type Decision struct {
	// Backend is the worker chosen for this request.
	Backend backend.Backend
	// MatchChunks is the number of leading chunks of the prompt that the
	// chosen backend was estimated to already hold in its KV cache. Always
	// zero for routers that do not consider the prompt.
	MatchChunks int
	// Reason is a short label such as "round-robin", "longest-prefix",
	// "spilled-from-saturated". It is included on metrics and log lines.
	Reason string
}

// Router is the routing interface implemented by each strategy.
type Router interface {
	// Name returns the strategy's stable identifier ("roundrobin", "random",
	// "leastloaded", "prefixaware"). Used for metrics labelling.
	Name() string
	// Choose selects a backend for the given prompt. The prompt may be empty
	// for non-prompt-aware routers.
	Choose(ctx context.Context, prompt string) (Decision, error)
	// Update notifies the router that the chosen backend has been dispatched
	// the given prompt. Stateless routers ignore this; prefix-aware uses it
	// to update its per-worker prefix tree.
	Update(prompt string, chosen backend.Backend)
}

// ErrNoBackends is returned when a router has no backends to choose from.
var ErrNoBackends = errors.New("router: no backends configured")

// base holds the backend list shared by every strategy. It is unexported and
// embedded into concrete strategies so they all expose the same Backends()
// view to the server without duplicating fields.
type base struct {
	backends []backend.Backend
}

func newBase(backends []backend.Backend) base {
	cp := make([]backend.Backend, len(backends))
	copy(cp, backends)
	return base{backends: cp}
}

// Backends returns the configured backend list. The slice is internal; callers
// must not mutate it.
func (b base) Backends() []backend.Backend { return b.backends }

// healthy returns the subset of the configured backends whose breakers
// currently allow traffic. Strategies call this on every Choose; if it
// returns empty, the strategy returns ErrNoBackends so the proxy can
// surface a 503 to the caller.
//
// Fast path: when every backend is healthy (the steady-state common case),
// the configured slice is returned directly with no allocation. Strategies
// must therefore treat the returned slice as read-only.
func (b base) healthy() []backend.Backend {
	for i, x := range b.backends {
		if !x.Healthy() {
			// Slow path: at least one backend is unhealthy. Build a fresh
			// slice excluding it (and any subsequent unhealthy ones).
			out := make([]backend.Backend, 0, len(b.backends)-1)
			out = append(out, b.backends[:i]...)
			for _, y := range b.backends[i+1:] {
				if y.Healthy() {
					out = append(out, y)
				}
			}
			return out
		}
	}
	return b.backends
}
