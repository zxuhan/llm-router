package backend

import (
	"sync"
	"time"
)

// CircuitBreaker is a small two-state breaker (closed / open). It opens
// after `threshold` consecutive failures and stays open for `cooldown`,
// after which it returns to closed and resamples normally. Successive
// failures will trip it again immediately.
//
// We deliberately do not implement a "half-open one-probe" state: the
// extra book-keeping is not worth the complexity for the small fleets
// this router targets, and the simpler "closed -> open -> closed"
// transition is easier to reason about under concurrency. The trade-off
// is documented in docs/decisions/0007-circuit-breaker.md.
type CircuitBreaker struct {
	threshold int
	cooldown  time.Duration

	mu       sync.Mutex
	failures int
	open     bool
	openedAt time.Time

	// trips is incremented every time the breaker transitions
	// closed -> open. Useful for diagnostics and metrics.
	trips uint64
}

// NewCircuitBreaker returns a breaker that trips after threshold
// consecutive failures and remains open for cooldown. Non-positive
// arguments are clamped to safe defaults (threshold=5, cooldown=30s) so
// every Backend has a working breaker even when its config is omitted.
func NewCircuitBreaker(threshold int, cooldown time.Duration) *CircuitBreaker {
	if threshold <= 0 {
		threshold = 5
	}
	if cooldown <= 0 {
		cooldown = 30 * time.Second
	}
	return &CircuitBreaker{threshold: threshold, cooldown: cooldown}
}

// Allow reports whether requests are currently permitted. It is
// idempotent and may be called from any goroutine; calling Allow does
// not consume any internal capacity (the breaker is purely a gate).
func (c *CircuitBreaker) Allow() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.open && time.Since(c.openedAt) >= c.cooldown {
		c.open = false
		c.failures = 0
	}
	return !c.open
}

// RecordSuccess clears the failure counter. Always safe to call.
func (c *CircuitBreaker) RecordSuccess() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failures = 0
	if c.open {
		c.open = false
	}
}

// RecordFailure increments the failure counter and trips the breaker
// when threshold consecutive failures have landed.
func (c *CircuitBreaker) RecordFailure() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failures++
	if !c.open && c.failures >= c.threshold {
		c.open = true
		c.openedAt = time.Now()
		c.trips++
	}
}

// CircuitState is a snapshot of a CircuitBreaker for diagnostics. Stable
// JSON keys so the metrics package can attach this to a label without
// parsing.
type CircuitState struct {
	Open       bool          `json:"open"`
	Failures   int           `json:"failures"`
	Trips      uint64        `json:"trips"`
	OpenedAt   time.Time     `json:"opened_at,omitempty"`
	Cooldown   time.Duration `json:"cooldown"`
	Threshold  int           `json:"threshold"`
	TimeOpenIn time.Duration `json:"time_open_in"`
}

// Snapshot returns a CircuitState describing the breaker's current state.
func (c *CircuitBreaker) Snapshot() CircuitState {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := CircuitState{
		Open:      c.open,
		Failures:  c.failures,
		Trips:     c.trips,
		Cooldown:  c.cooldown,
		Threshold: c.threshold,
	}
	if c.open {
		st.OpenedAt = c.openedAt
		st.TimeOpenIn = time.Until(c.openedAt.Add(c.cooldown))
	}
	return st
}
