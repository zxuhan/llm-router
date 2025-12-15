package router

import (
	"context"
	"math/rand/v2"

	"github.com/xzhou/llm-router/internal/backend"
)

// Random picks a backend uniformly at random per request. Like RoundRobin it
// is a baseline; under skewed traffic patterns it slightly outperforms strict
// round-robin because it avoids lock-step coupling between callers.
type Random struct {
	base
	// rng is overridable for deterministic tests. The default is the
	// concurrency-safe top-level math/rand/v2 source.
	rng func(n int) int
}

// NewRandom returns a Random router. The default RNG uses math/rand/v2 which
// is goroutine-safe. Tests can swap in a deterministic RNG via WithRNG.
func NewRandom(backends []backend.Backend) *Random {
	return &Random{base: newBase(backends), rng: rand.IntN}
}

// WithRNG replaces the random integer source. Returns the receiver for
// chaining at construction sites.
func (r *Random) WithRNG(fn func(n int) int) *Random {
	r.rng = fn
	return r
}

// Name implements Router.
func (*Random) Name() string { return "random" }

// Choose implements Router.
func (r *Random) Choose(_ context.Context, _ string) (Decision, error) {
	pool := r.healthy()
	if len(pool) == 0 {
		return Decision{}, ErrNoBackends
	}
	idx := r.rng(len(pool))
	return Decision{Backend: pool[idx], Reason: "random"}, nil
}

// Update implements Router.
func (*Random) Update(string, backend.Backend) {}
