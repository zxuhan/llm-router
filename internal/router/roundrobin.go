package router

import (
	"context"
	"sync/atomic"

	"github.com/zxuhan/llm-router/internal/backend"
)

// RoundRobin distributes requests across the backend list in strict rotation.
// It is intentionally trivial: it serves as the baseline against which other
// strategies are compared.
type RoundRobin struct {
	base
	counter atomic.Uint64
}

// NewRoundRobin builds a RoundRobin router over the given backends.
func NewRoundRobin(backends []backend.Backend) *RoundRobin {
	return &RoundRobin{base: newBase(backends)}
}

// Name implements Router.
func (*RoundRobin) Name() string { return "roundrobin" }

// Choose implements Router.
func (r *RoundRobin) Choose(_ context.Context, _ string) (Decision, error) {
	pool := r.healthy()
	if len(pool) == 0 {
		return Decision{}, ErrNoBackends
	}
	// Fetch-and-add gives a strictly monotonic counter even under concurrency.
	// We subtract 1 so the first call returns index 0.
	idx := int((r.counter.Add(1) - 1) % uint64(len(pool)))
	return Decision{Backend: pool[idx], Reason: "round-robin"}, nil
}

// Update implements Router. RoundRobin does not maintain per-prompt state.
func (*RoundRobin) Update(string, backend.Backend) {}
