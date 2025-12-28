# ADR 0007: Two-state circuit breaker per backend

## Context

A worker can become unresponsive (process crashed, GPU stuck, deadlock,
network blip). Without back-pressure the router would happily keep
sending traffic at it; every request would 502 until an operator notices.
We need a way for unhealthy workers to recuse themselves automatically.

## Decision

Each `LlamaCpp` backend owns a small two-state circuit breaker:

- **closed**: every request is allowed; success resets the failure
  counter; transport errors and 5xx responses increment it.
- **open**: every Allow returns false; the breaker auto-resets to
  closed after the configured cooldown.

The transition is `closed -> open -> closed`. There is no half-open
state, no exponential backoff, no per-request probe accounting.

When the breaker is open, the corresponding backend reports `Healthy()
== false`. Every routing strategy filters out unhealthy backends before
making a decision, so an open breaker effectively removes the worker
from the pool until cooldown elapses.

## Rationale

- **Simplicity wins for small fleets.** The router is for tens of
  workers, not thousands. The half-open one-probe pattern adds two
  states and a CAS-y "did the probe succeed" flow that is bookkeeping
  for noise. The simpler form is correct under concurrency and easy to
  read. Unit tests cover trip + auto-reset + 4xx-doesn't-trip.
- **5xx vs 4xx distinction matters.** A 400 means the *client* sent
  garbage; routing the next client request away from this worker would
  not help. Only 5xx and transport errors trip the breaker.
- **Auto-reset, not operator action.** Half-open's value is "let one
  request probe instead of all of them". For a small fleet the
  thundering herd at cooldown is bounded and self-limiting (if the
  worker is still broken, the next N failures trip the breaker
  immediately and we go right back to open).
- **Per-backend, not global.** A failing worker should not pull the
  rest of the fleet off the air; the router should naturally route
  away from it.

## Consequences

- Tunables on `WorkerConfig`: `breaker_threshold` (default 5) and
  `breaker_cooldown` (default 30s). Test fakes override both for
  fast-iteration assertions.
- The router strategies all call a private `healthy()` helper before
  picking. When every backend is unhealthy, `Choose` returns
  `ErrNoBackends`; the proxy surfaces this as 503 to the caller, which
  is the right thing operationally (let the upstream load balancer
  shed traffic).
- The breaker's `Snapshot()` is exposed for diagnostics. A future
  metrics commit can label gauges by backend state without changing
  this code.

## Alternatives considered

- **Half-open one-probe with sync.Once-style probe slot.** Correct but
  involves either a goroutine pool or an extra atomic; not worth it
  here. Documented as a possible future change.
- **Token-bucket rate limiter instead of consecutive-failure
  counter.** Could rate-limit retries to a sick worker. The
  consecutive-failure trip threshold is simpler and matches the
  qualitative behaviour we want (sick workers go fully off rotation,
  not "slow rotation"). 
- **Health probes (separate goroutine pinging /health).** Adds a
  background timer per backend; the in-band approach is sufficient
  because every dispatch already exercises the upstream.
