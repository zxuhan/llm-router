package router

import (
	"context"

	"github.com/xzhou/llm-router/internal/backend"
)

// LeastLoaded picks the backend with the smallest current inflight count.
// Ties are broken by backend list order, so the routing is deterministic
// given the same load snapshot.
type LeastLoaded struct {
	base
}

// NewLeastLoaded returns a LeastLoaded router.
func NewLeastLoaded(backends []backend.Backend) *LeastLoaded {
	return &LeastLoaded{base: newBase(backends)}
}

// Name implements Router.
func (*LeastLoaded) Name() string { return "leastloaded" }

// Choose implements Router.
func (l *LeastLoaded) Choose(_ context.Context, _ string) (Decision, error) {
	if len(l.backends) == 0 {
		return Decision{}, ErrNoBackends
	}
	// Snapshot inflight counts up front. The values may change between this
	// loop and dispatch, but a self-consistent local view is enough for the
	// tie-break to be deterministic.
	bestIdx := 0
	bestLoad := l.backends[0].Inflight()
	for i := 1; i < len(l.backends); i++ {
		if got := l.backends[i].Inflight(); got < bestLoad {
			bestIdx = i
			bestLoad = got
		}
	}
	return Decision{Backend: l.backends[bestIdx], Reason: "least-loaded"}, nil
}

// Update implements Router.
func (*LeastLoaded) Update(string, backend.Backend) {}
